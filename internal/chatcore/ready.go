package chatcore

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harnessname"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// authGateStabilizeGap is how long a soft logged-out BANNER (matching
// authRequired but not onboardingWall, and never reaching readyForInput) must
// persist before waitReadyForSend short-circuits with ErrAuthRequired. The dwell
// distinguishes a persistent banner from a transient startup frame. An onboarding
// WALL does NOT wait for this — it fires immediately (see waitReadyForSend),
// because a sign-in / device-code / login-method screen never becomes ready and
// can flash by in a single frame as the CLI advances its own login flow.
const authGateStabilizeGap = 2 * time.Second

// unrecognizedDialogStabilizeGap is how long a blocking dialog this build cannot
// parse (turns.DialogUnparseable) must persist before waitReadyForSend
// short-circuits with ErrUnrecognizedDialog. It follows the onboarding-WALL
// precedent — the condition is permanent, so waiting out the send deadline
// gains nothing — with ONE deliberate difference: the wall fires immediately
// because it can flash by in a single frame, whereas an unparseable frame can
// simply be a HALF-PAINTED one, so this state must survive a re-check of the
// live screen before it is believed. Same dwell as the auth banner above.
const unrecognizedDialogStabilizeGap = authGateStabilizeGap

// Cross-language asymmetry, deliberate (PUPPET-315): the TS port has a second,
// NON-throwing readiness helper (awaitPromptReadyUntil) used by input/answer,
// setPermissionMode and probeCodexStatus, and that helper carries its own auth
// classification so those callers can report an auth cause instead of a bare
// timeout. Go has no such twin — every readiness wait here goes through
// waitReadyForSend, which already classifies and returns ErrAuthRequired — so
// there is nothing to mirror. If a bounded, non-throwing readiness wait is ever
// added on this side, give it the same classification.

func (c *Conversation) waitReadyForSend(ctx context.Context) error {
	// A prompt awaiting an external answer can never reach the ready state on
	// its own; fail fast so the caller answers it first. (A prompt being
	// auto-answered by a policy/handler is not "surfaced" and we keep waiting
	// for it to clear — bounded, because answerAndConfirm gives up and latches
	// an *InputUnresolvedError rather than letting the wait run to ctx.)
	if err := c.inputBlocked(); err != nil {
		return err
	}
	if !c.requiresPromptReadiness() {
		return nil
	}

	// Subscribe BEFORE the readiness check to avoid a lost-wakeup race: if the
	// prompt-ready frame lands between the check and Subscribe and the harness
	// then paints nothing further (it can sit idle at a static prompt — no
	// spinner, no cursor blink), the notification is missed and we block until
	// ctx. Subscribing first guarantees any later frame wakes us; the check
	// below still returns immediately for a prompt that was already ready.
	notifyCh, unsubscribe := c.screen.Subscribe()
	defer unsubscribe()

	// Stabilize timer for a soft logged-out BANNER on a not-ready screen (rare on
	// the send path). An onboarding WALL is handled separately, immediately.
	var auth stabilizer
	auth.gap = authGateStabilizeGap
	defer auth.disarm()
	// Stabilize timer for a blocking dialog whose choices cannot be parsed.
	var dialog stabilizer
	dialog.gap = unrecognizedDialogStabilizeGap
	defer dialog.disarm()

	// The busy gate. A harness that is working keeps its composer painted, so a
	// ready prompt does not mean it will take a message: text typed now lands
	// in a turn that is still running. Where the adapter can tell
	// (turns.BusyDetector), Send types only once the harness has been idle for
	// the confirmation window — the one that already rides out the footer
	// flickering during sub-agent work — and busy records why it is waiting,
	// so a ctx that ends first says so (ErrHarnessBusy).
	var busy bool
	var quiet *time.Timer
	var quietC <-chan time.Time
	defer func() {
		if quiet != nil {
			quiet.Stop()
		}
	}()
	armQuiet := func(d time.Duration) {
		if quiet != nil {
			quiet.Stop()
		}
		quiet = time.NewTimer(d)
		quietC = quiet.C
	}

	// check classifies the current screen. An onboarding WALL (sign-in wizard /
	// device-code / login-method screen) fires NOW: it never becomes ready, and
	// it can appear for a single frame before the CLI advances its own login flow
	// past it — a dwell would miss it. A softer logged-out banner, and an
	// unparseable blocking dialog, arm their debounce timers instead.
	// readyForInput wins first, so a real composer (even with a stale banner
	// scrolled above) is never auth-gated.
	check := func() (ready, wall bool) {
		snap := c.screen.Snapshot()
		txt := snap.Text
		busy = c.observeBusy(snap)
		if c.readyForInput(txt) && !busy {
			if rem := c.busyQuietRemaining(); rem > 0 {
				busy = true
				armQuiet(rem)
				return false, false
			}
			return true, false
		}
		if onboardingWall(c.opts.Harness, txt) {
			return false, true
		}
		if c.dialogState(txt) == turns.DialogUnparseable {
			dialog.arm()
		} else {
			dialog.disarm()
		}
		if authRequired(c.opts.Harness, txt) {
			auth.arm()
		} else {
			auth.disarm()
		}
		return false, false
	}

	if ready, wall := check(); ready {
		return nil
	} else if wall {
		return ErrAuthRequired
	}

	for {
		select {
		case <-ctx.Done():
			if busy {
				return fmt.Errorf("%w: %w", ErrHarnessBusy, ctx.Err())
			}
			return ctx.Err()
		case <-c.closed:
			return ErrClosed
		case <-quietC:
			quietC = nil
			if ready, wall := check(); ready {
				return nil
			} else if wall {
				return ErrAuthRequired
			}
		case <-auth.ch():
			// Re-confirm against the live screen before committing: a frame may
			// have changed the screen without a wake we processed, so never
			// short-circuit on a stale banner.
			txt := c.screen.Snapshot().Text
			if !c.readyForInput(txt) && authRequired(c.opts.Harness, txt) {
				return ErrAuthRequired
			}
			auth.disarm()
		case <-dialog.ch():
			// Same re-check, for the same reason and one more: an unparseable
			// frame can be a half-painted one, so the state must still hold
			// against the LIVE screen before the run is failed on it.
			txt := c.screen.Snapshot().Text
			if !c.readyForInput(txt) &&
				c.dialogState(txt) == turns.DialogUnparseable {
				return ErrUnrecognizedDialog
			}
			dialog.disarm()
		case <-c.inputStateCh:
			if err := c.inputBlocked(); err != nil {
				return err
			}
			if ready, wall := check(); ready {
				return nil
			} else if wall {
				return ErrAuthRequired
			}
		case _, ok := <-notifyCh:
			if !ok {
				return ErrClosed
			}
			if err := c.inputBlocked(); err != nil {
				return err
			}
			if ready, wall := check(); ready {
				return nil
			} else if wall {
				return ErrAuthRequired
			}
		}
	}
}

// stabilizer is a one-shot dwell timer: a screen state arms it, any other state
// disarms it, and it fires only if the state held for the whole gap. It exists
// so waitReadyForSend can debounce several independent conditions (a logged-out
// banner, an unparseable dialog) without a copy of the arm/disarm dance per
// condition. Not safe for concurrent use — it is driven from the single
// waitReadyForSend goroutine.
type stabilizer struct {
	gap   time.Duration
	timer *time.Timer
	c     <-chan time.Time
}

func (s *stabilizer) arm() {
	if s.timer == nil {
		s.timer = time.NewTimer(s.gap)
		s.c = s.timer.C
	}
}

func (s *stabilizer) disarm() {
	if s.timer == nil {
		return
	}
	if !s.timer.Stop() {
		select {
		case <-s.timer.C:
		default:
		}
	}
	s.timer = nil
	s.c = nil
}

// ch returns the fire channel, nil while disarmed — a nil channel blocks
// forever in a select, which is how an unarmed stabilizer stays inert.
func (s *stabilizer) ch() <-chan time.Time { return s.c }

// requiresPromptReadiness reports whether the conversation's harness must show
// a ready composer before a message is typed (turns.ReadinessDetector).
func (c *Conversation) requiresPromptReadiness() bool { return requiresReadiness(c.adapter) }

// readyForInput reports whether the conversation's screen will take a message.
func (c *Conversation) readyForInput(text string) bool {
	return readyOn(c.adapter, c.opts.Harness, text)
}

// dialogState is what the conversation's adapter reads of a blocking dialog on
// the screen (turns.DialogDetector).
func (c *Conversation) dialogState(text string) turns.DialogState {
	return dialogStateOn(c.adapter, text)
}

func requiresReadiness(adapter turns.Adapter) bool {
	_, ok := adapter.(turns.ReadinessDetector)
	return ok
}

// readyOn reports whether harness's screen, read by adapter, will take a
// message. An adapter that cannot tell (no turns.ReadinessDetector) is always
// ready.
//
// A first-run onboarding or sign-in WIZARD comes first: claude's theme picker
// and "Select login method" screen paint the "Claude Code" header and a "❯"
// menu selector, and codex's never-signed-in menu a "›"-highlighted row, so
// either looks ready — but it waits for menu input and never turns into a
// usable composer on its own. Not ready, so Send's auth gate short-circuits it
// instead of typing the prompt into the wizard.
//
// A composer painted while a turn runs is ready by this reading: whether the
// harness is still working is the turns.BusyDetector gate maybeIdleComplete
// applies after it, and Send rejects a message with ErrTurnInFlight before it
// reaches the readiness gate (send.go, pinned by
// TestSend_TurnInFlightRejectedBeforeReadiness).
func readyOn(adapter turns.Adapter, harness, text string) bool {
	rd, ok := adapter.(turns.ReadinessDetector)
	if !ok {
		return true
	}
	if onboardingWall(harness, text) {
		return false
	}
	return rd.ReadyForInput(text)
}

// dialogStateOn is adapter's reading of a blocking dialog on the screen, and
// DialogNone for an adapter that cannot read one.
func dialogStateOn(adapter turns.Adapter, text string) turns.DialogState {
	if d, ok := adapter.(turns.DialogDetector); ok {
		return d.DialogState(text)
	}
	return turns.DialogNone
}

// Logged-out / re-authentication AND not-yet-onboarded banners, per harness. A
// harness whose CLI login has expired, was never established, or that is still
// sitting in first-run onboarding produces NO assistant output for the turn. The
// anchors are grounded in real observed CLI output, not invented — see
// test/corpus/auth for the captured screen each one matches:
//   - claude-code: "Not logged in · Please run /login" (logged out); "Invalid API
//     key · Fix external API key" (bad external key); the first-run onboarding
//     "Choose the text style" theme picker, the "Select login method" screen, and
//     the OAuth browser sign-in screen the latter advances into ("Use the url
//     below to sign in" / "Paste code here if prompted").
//   - codex:       "401 Unauthorized: missing bearer or basic authentication"
//     (bad/expired key); a logged-out TUI / `codex login status` say "Not logged
//     in"; codex's own remediation is "run `codex login`"; the never-signed-in
//     onboarding menu "Sign in with ChatGPT".
//
// Reachability: these are scanned (a) when a turn ends in failure, (b) on the
// completion path when the turn produced NO clean assistant text — an auth banner
// left on a settled screen (see maybeIdleComplete / handleTurnsEvent), and (c)
// before a turn is sent, to short-circuit an onboarding screen that would
// otherwise hang to the deadline (see Conversation.Send). They EXPLAIN or
// pre-empt a turn that cannot produce output; they never COMPLETE a turn that
// produced a real reply. The empty-output gate is what keeps a genuine reply
// mentioning logins, or a benign "your login expires in N days" WARNING on a
// still-valid session, from being scanned and mislabeled.
var (
	// Onboarding WIZARDS: interactive first-run screens that wait for menu input
	// and never become a usable composer on their own. readyForInput treats these
	// as not-ready (so Send's auth gate short-circuits them), distinct from a
	// normal composer showing a stale logged-out banner (which IS ready).
	//
	// Every wall anchor is the wall's OWN UI line, anchored at the start of a
	// line (after indentation). A wall is checked BEFORE readiness and fails
	// Send at once, so an anchor that matched anywhere on screen turned a reply
	// that merely quoted one of these phrases into a spurious ErrAuthRequired
	// for the next Send. Line anchoring rejects every mid-sentence quote; what
	// it cannot reject is a reply line that itself begins with the exact UI
	// wording, which needs the full phrase and (for the paste line) its input
	// prompt to collide.
	claudeOnboardingAnchors = []ScreenAnchor{
		{"claude.onboarding.theme_picker", regexp.MustCompile(`(?im)^[^\S\n]*choose the text style\b`)},      // theme picker
		{"claude.onboarding.select_login_method", regexp.MustCompile(`(?im)^[^\S\n]*select login method\b`)}, // login-method screen
		// The OAuth sign-in page the login-method menu advances into: the browser
		// handoff / paste-the-code screen. It is a WALL — it never becomes a
		// composer — and the "Select login method" anchor is gone from the screen
		// by the time it paints, so without these anchors it matches nothing and
		// waitReadyForSend blocks to the run deadline (PUPPET-315). Claude's
		// counterpart to codex's "finish signing in via your browser". Either
		// line alone suffices: on a short terminal the wrapped authorize URL
		// between them can push the first one off screen.
		{"claude.onboarding.oauth_browser_open", regexp.MustCompile(`(?im)^[^\S\n]*browser didn't open\? use the url below to sign in\b`)},
		{"claude.onboarding.oauth_paste_code", regexp.MustCompile(`(?im)^[^\S\n]*paste code here if prompted[^\S\n]*>`)},
	}
	codexOnboardingAnchors = []ScreenAnchor{
		// The never-signed-in menu: its title line and its highlighted row
		// ("> 1. Sign in with ChatGPT").
		{"codex.onboarding.sign_in_with_chatgpt", regexp.MustCompile(`(?im)^[^\S\n]*(?:[>›][^\S\n]*(?:\d+\.[^\S\n]*)?)?sign in with chatgpt\b`)},
		// The login flow the menu advances into (browser + device-code).
		{"codex.onboarding.browser_signin", regexp.MustCompile(`(?im)^[^\S\n]*finish signing in via your browser\b`)},
	}
	// Logged-out / bad-key banners left on an otherwise-ready screen. Handled on
	// the completion path (a turn that yielded no reply), not by refusing to send.
	claudeLoggedOutAnchors = []ScreenAnchor{
		{"claude.loggedout.run_login", regexp.MustCompile(`(?i)\brun /login\b`)},
		{"claude.loggedout.not_logged_in", regexp.MustCompile(`(?i)\bnot logged in\b`)},
		{"claude.loggedout.invalid_api_key", regexp.MustCompile(`(?i)\binvalid api key\b`)},
	}
	codexLoggedOutAnchors = []ScreenAnchor{
		{"codex.loggedout.401_unauthorized", regexp.MustCompile(`(?i)\b401 unauthorized\b`)},
		{"codex.loggedout.missing_bearer", regexp.MustCompile(`(?i)missing bearer or basic authentication`)},
		{"codex.loggedout.not_logged_in", regexp.MustCompile(`(?i)\bnot logged in\b`)},
		{"codex.loggedout.codex_login", regexp.MustCompile(`(?i)\bcodex(?: mcp)? login\b`)},
	}
)

// ScreenAnchor is one identified screen pattern: the regex this package
// matches with, and a stable id for it.
//
// The ids exist because a consumer that RECORDS which banner it saw needs a
// name for it, and copying the regexes out to invent one is how a consumer
// ends up tracking harness drift by hand. loom did exactly that: it mirrored
// these patterns into internal/agenterr, pinned to v0.7.7, and by v0.10 held 4
// of the 6 onboarding anchors — missing both of claude's OAuth sign-in walls —
// with its copies unanchored where these are line-anchored.
//
// The id strings are a DOWNSTREAM CONTRACT. loom's
// docs/adr/0002-authfailure-stays-terminal.md names them in its revisit
// triggers, so a rename silently breaks the trigger it belongs to. Add anchors
// freely; do not rename an existing id.
type ScreenAnchor struct {
	ID string
	RE *regexp.Regexp
}

// AuthAnchors returns the screen anchors whose presence means the harness
// cannot produce output until a human authenticates — onboarding wizard first,
// then logged-out banner, which is authRequired's own precedence, so a caller
// recording the FIRST hit names the arm this package would have taken.
//
// A harness this package has no banner set for returns nil. The empty string
// returns every harness's anchors, for a caller that describes a screen
// without knowing which harness drew it.
func AuthAnchors(harness string) []ScreenAnchor {
	switch harness {
	case harnessname.ClaudeCode:
		return concatAnchors(claudeOnboardingAnchors, claudeLoggedOutAnchors)
	case harnessname.Codex:
		return concatAnchors(codexOnboardingAnchors, codexLoggedOutAnchors)
	case "":
		return concatAnchors(claudeOnboardingAnchors, codexOnboardingAnchors,
			claudeLoggedOutAnchors, codexLoggedOutAnchors)
	default:
		return nil
	}
}

// DialogAnchors returns the literal lines a BLOCKING dialog paints — a
// folder-trust prompt, a bypass-permissions confirmation. A screen showing one
// is about a dialog, not a login, which is the distinction a caller recording
// evidence needs in order to tell a real auth wall from a verdict taken over a
// modal. Nil for a harness with no known dialogs. The empty string returns
// every registered harness's anchors (turns.DialogAnchorer), for a caller that
// describes a screen without knowing which harness drew it.
func DialogAnchors(harness string) []string {
	names := []string{harness}
	if harness == "" {
		names = registeredNames()
	}
	var out []string
	for _, name := range names {
		if da, ok := adapterNamed(name).(turns.DialogAnchorer); ok {
			out = append(out, da.DialogAnchors()...)
		}
	}
	return out
}

func concatAnchors(groups ...[]ScreenAnchor) []ScreenAnchor {
	var out []ScreenAnchor
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func anyAnchor(anchors []ScreenAnchor, text string) bool {
	for _, a := range anchors {
		if a.RE.MatchString(text) {
			return true
		}
	}
	return false
}

// onboardingWall reports whether the screen is a first-run onboarding / sign-in
// WIZARD that is waiting for menu input and will never turn into a usable
// composer on its own — distinct from a normal composer that merely shows a stale
// logged-out banner. readyForInput uses it to keep Send from typing a prompt into
// the wizard, so the auth gate short-circuits with ReasonAuthRequired instead.
func onboardingWall(harness, text string) bool {
	switch harness {
	case harnessname.ClaudeCode:
		return anyAnchor(claudeOnboardingAnchors, text)
	case harnessname.Codex:
		return anyAnchor(codexOnboardingAnchors, text)
	default:
		return false
	}
}

// --- usage / session-limit wall detection ---
//
// When the subscription's rolling usage window is exhausted, claude-code renders
// a wall line IN PLACE of an assistant reply, e.g.
//
//	"You've hit your session limit · resets 10:20pm (Europe/Warsaw)"
//
// The TUI paints it as an assistant bubble, so ExtractMessage captures it and it
// would otherwise be persisted as a genuine reply — a false success whose "Text"
// is the wall. usageLimitMessage lets the completion path detect it and error the
// turn with a usage-limit reason instead (see Conversation.usageLimitRelabel).
//
// Mirrored inline from the wrapper's sessionLimitRE (internal/wrapcore/harness/
// claude), which pkg/chat cannot import — it lives under pkg/wrapper's internal/
// tree — and which classifies a FINISHED RUN's recent output rather than an
// in-conversation turn. Anchored on the wall's own sentence and captured to
// end-of-line so the reset time rides along in the reason; a genuine reply merely
// mentioning a "usage limit" in prose won't match, because the CLI only ever emits
// this exact phrasing and the leading glyph + "hit your … limit" anchor rejects
// incidental prose.
var claudeUsageLimitRE = regexp.MustCompile(
	`(?im)^` + horizontalSpace + `*(?:[⎿·●⏺]` + horizontalSpace + `*)?(You(?:'ve|\s+have)\s+hit\s+your\s+(?:session|usage)\s+limit[^\r\n]*)$`,
)

// horizontalSpace matches one space-like character that is NOT a line break, so
// the (?m)-anchored patterns above stay on their own line. Mirrors the wrapper's
// constant of the same name.
const horizontalSpace = `[\t \x{00A0}]`

// usageLimitMessage returns the harness usage/session-limit wall line (its "out of
// quota" screen, rendered in place of a reply) when present — trimmed, including
// the "· resets …" tail — and false when absent. Returns false for any harness
// without a known wall (only claude-code today).
func usageLimitMessage(harness, text string) (string, bool) {
	switch harness {
	case harnessname.ClaudeCode:
		m := claudeUsageLimitRE.FindStringSubmatch(text)
		if m == nil {
			return "", false
		}
		return strings.TrimSpace(m[1]), true
	default:
		return "", false
	}
}

// authRequired reports whether the rendered screen shows a harness login-expiry /
// logged-out banner OR a first-run onboarding wizard — either way the turn can
// produce no assistant output until the human authenticates. Returns false for
// any harness without a known banner set.
func authRequired(harness, text string) bool {
	switch harness {
	case harnessname.ClaudeCode:
		return anyAnchor(claudeOnboardingAnchors, text) || anyAnchor(claudeLoggedOutAnchors, text)
	case harnessname.Codex:
		return anyAnchor(codexOnboardingAnchors, text) || anyAnchor(codexLoggedOutAnchors, text)
	default:
		return false
	}
}

func submitKeyForHarness(harness, screenText string) []byte {
	switch harness {
	case harnessname.ClaudeCode:
		// Claude Code enables enhanced keyboard handling in its TUI and does
		// not submit the input box when a synthetic PTY writer sends plain
		// CR/LF — it only inserts a newline and the turn never runs. CSI 13 u
		// is the unmodified Enter key in that mode. Recent versions (≥2.1.x)
		// turn enhanced mode on unconditionally at startup — the auto-mode
		// composer shows neither "bypass permissions" nor "ctrl+g to edit in
		// Vim" — so we always send the enhanced Enter, mirroring codex below.
		return []byte("\x1b[13u")
	case harnessname.Codex:
		// codex 0.141.0 turns on the enhanced (kitty) keyboard protocol at startup,
		// so a plain CR/LF from a synthetic PTY writer is NOT treated as submit — it
		// only inserts a newline in the composer and the turn never runs. CSI 13 u is
		// the unmodified Enter key in that mode (same as claude-code's enhanced TUI).
		// 0.140.0 accepted "\n", but enhanced mode is unconditional now.
		return []byte("\x1b[13u")
	default:
		return []byte("\n")
	}
}

// shiftTabForHarness returns the byte sequence a synthetic PTY writer must send
// to press Shift+Tab — the key claude-code and codex bind to "cycle permission
// mode" — or nil for a harness with no known encoding.
//
// It is the Shift+Tab twin of submitKeyForHarness above, and rests on the same
// fact: claude-code and codex both turn the kitty / enhanced keyboard protocol
// on unconditionally at startup, which is why a plain CR does not submit and
// CSI 13 u does. Under that protocol every key — modified or not — arrives as
// CSI <codepoint> ; <modifiers> u. Tab is codepoint 9 and the Shift modifier is
// encoded as 1+1 = 2, so Shift+Tab is "\x1b[9;2u" (CSI 9 ; 2 u).
//
// LIVE VERIFICATION (2026-07-22, claude-code 2.1.217 and codex 0.144.5, driven
// through a real PTY + vt10x screen exactly like this package's writer):
//
//   - claude-code: "\x1b[9;2u" cycles the mode — the status line goes from
//     "⏵⏵ auto mode on (shift+tab to cycle)" to "⏸ manual mode on".
//   - codex: "\x1b[9;2u" cycles the mode — the footer gains "Plan mode".
//   - Control: a bare "\t" does NOT cycle either one (on claude-code it only
//     swaps a hint line), so the mode change is genuinely attributable to the
//     Shift+Tab decode and not to any tab-ish byte.
//
// The legacy form is "\x1b[Z" (CSI Z, "cursor backward tabulation"), what a
// terminal emits for Shift+Tab in *unenhanced* mode. Contrary to what the
// enhanced-keyboard story would suggest, CSI Z was measured to cycle the mode
// on BOTH harnesses too — at these versions each TUI still keeps a legacy
// Shift+Tab path alongside its CSI u decoder, so this is not a case where one
// encoding works and the other silently no-ops.
//
// CSI 9;2u is nonetheless what we send, deliberately: it is the encoding the
// kitty protocol these TUIs *actually enable* defines for Shift+Tab, so it is
// what a real terminal would deliver and what their maintained input path is
// built around. The legacy branch is a compatibility shim that can be dropped
// when a TUI hardens its enhanced-mode decoder, whereas the protocol-native
// form cannot be — the same trade submitKeyForHarness already makes for Enter,
// where CR genuinely does nothing and CSI 13u is the only thing that submits.
//
// screenText is accepted to mirror submitKeyForHarness's shape — which takes it
// for the same reason and likewise does not branch on it today, leaving room for
// a screen-sensitive variant. Neither harness's Shift+Tab encoding depends on
// what is rendered.
func shiftTabForHarness(harness, screenText string) []byte {
	switch harnessname.Unalias(harness) {
	case harnessname.ClaudeCode:
		// Verified live on 2.1.217: cycles auto → manual in the status line.
		return []byte(shiftTabCSI9_2u)
	case harnessname.Codex:
		// Verified live on 0.144.5: cycles the footer into "Plan mode".
		return []byte(shiftTabCSI9_2u)
	default:
		// Unknown harnesses get nil rather than a best-guess keystroke.
		// Returning nil lets the caller fail loudly on "this harness has no
		// Shift+Tab contract" instead of writing bytes that quietly do
		// something unrelated.
		return nil
	}
}

// shiftTabCSI9_2u is Shift+Tab in the kitty / enhanced keyboard protocol: CSI
// 9 ; 2 u (Tab codepoint 9, Shift modifier 2). internal/fakeharness exports the
// identical string as ShiftTabCSI9_2u so hermetic scenarios and this production
// writer cannot drift; TestShiftTabMatchesFakeharness pins them byte-equal.
const shiftTabCSI9_2u = "\x1b[9;2u"

// pasteStartCSI200 / pasteEndCSI201 are the bracketed-paste framing markers a
// real terminal emitter wraps pasted text in: CSI 200 ~ before, CSI 201 ~ after.
// internal/fakeharness exports the identical strings as PasteStart / PasteEnd so
// hermetic scenarios and this production writer cannot drift;
// TestPasteWrapMatchesFakeharness pins them byte-equal.
const (
	pasteStartCSI200 = "\x1b[200~"
	pasteEndCSI201   = "\x1b[201~"
)

// pasteWrapForHarness returns the bracketed-paste framing a synthetic PTY writer
// must wrap a LARGE composer payload in for this harness, or (nil, nil) for a
// harness whose composer has not been measured against one.
//
// It is the paste twin of submitKeyForHarness / shiftTabForHarness above, and
// exists for a measured defect, not for tidiness. Typing a big prompt as one raw
// burst is not what a terminal does with a paste, and claude-code's composer
// treats each READ CHUNK of that burst as fresh typed input, keeping only the
// last one:
//
// LIVE MEASUREMENT (2026-08-27, claude-code 2.1.247, macOS, driven through
// pkg/oneshot against a 2627-byte / 43-line prompt whose FIRST line asks the
// model to echo three marker words, so the reply reveals what actually arrived):
//
//   - Unframed (today's behaviour): 5 of 10 runs answered the TAIL of the
//     prompt, every truncation starting at the same byte offset — 2044 of 2608
//     in the original report, i.e. one 2KB read chunk. The turn STARTS and
//     completes normally, so nothing in the wrapper notices; the run is silently
//     wasted. Truncated runs also ran long (14-25s) against ~6s for intact ones.
//   - Framed with CSI 200 ~ / CSI 201 ~: 10 of 10 runs echoed the FIRST words,
//     all in 5-8s.
//
// Both claude-code and codex ENABLE bracketed-paste mode at startup — the byte
// corpora in this repo open with ESC[?2004h and close with ESC[?2004l
// (test/corpus/claude-code/*/bytes.raw:1, test/corpus/codex/*/bytes.raw:1) — so
// the framing is the protocol-correct way to say "this is one paste, do not act
// on the newlines inside it", not a heuristic.
//
// Unmeasured harnesses keep today's
// behaviour — that is the whole point of a per-harness table. codex is included
// on the corpus evidence alone; if it ever proves wrong there, it comes out
// here and nowhere else.
func pasteWrapForHarness(harness string) (prefix, suffix []byte) {
	switch harnessname.Unalias(harness) {
	case harnessname.ClaudeCode, harnessname.Codex:
		return []byte(pasteStartCSI200), []byte(pasteEndCSI201)
	default:
		return nil, nil
	}
}
