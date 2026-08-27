package chat

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/pi"
)

// authGateStabilizeGap is how long a soft logged-out BANNER (matching
// authRequired but not onboardingWall, and never reaching readyForInput) must
// persist before waitReadyForSend short-circuits with ErrAuthRequired. The dwell
// distinguishes a persistent banner from a transient startup frame. An onboarding
// WALL does NOT wait for this — it fires immediately (see waitReadyForSend),
// because a sign-in / device-code / login-method screen never becomes ready and
// can flash by in a single frame as the CLI advances its own login flow.
const authGateStabilizeGap = 2 * time.Second

func (c *Conversation) waitReadyForSend(ctx context.Context) error {
	// A prompt awaiting an external answer can never reach the ready state on
	// its own; fail fast so the caller answers it first. (A prompt being
	// auto-answered by a policy/handler is not "surfaced" and we keep waiting
	// for it to clear.)
	if c.inputAwaitingClient() {
		return ErrInputPending
	}
	if !requiresPromptReadiness(c.opts.Harness) {
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
	var authTimer *time.Timer
	var authCh <-chan time.Time
	armAuth := func() {
		if authTimer == nil {
			authTimer = time.NewTimer(authGateStabilizeGap)
			authCh = authTimer.C
		}
	}
	disarmAuth := func() {
		if authTimer != nil {
			if !authTimer.Stop() {
				select {
				case <-authTimer.C:
				default:
				}
			}
			authTimer = nil
			authCh = nil
		}
	}
	defer disarmAuth()

	// check classifies the current screen. An onboarding WALL (sign-in wizard /
	// device-code / login-method screen) fires NOW: it never becomes ready, and
	// it can appear for a single frame before the CLI advances its own login flow
	// past it — a dwell would miss it. A softer logged-out banner arms the
	// debounce timer instead. readyForInput wins first, so a real composer (even
	// with a stale banner scrolled above) is never auth-gated.
	check := func() (ready, wall bool) {
		txt := c.screen.Snapshot().Text
		if readyForInput(c.opts.Harness, txt) {
			return true, false
		}
		if onboardingWall(c.opts.Harness, txt) {
			return false, true
		}
		if authRequired(c.opts.Harness, txt) {
			armAuth()
		} else {
			disarmAuth()
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
			return ctx.Err()
		case <-c.closed:
			return ErrClosed
		case <-authCh:
			// Re-confirm against the live screen before committing: a frame may
			// have changed the screen without a wake we processed, so never
			// short-circuit on a stale banner.
			txt := c.screen.Snapshot().Text
			if !readyForInput(c.opts.Harness, txt) && authRequired(c.opts.Harness, txt) {
				return ErrAuthRequired
			}
			disarmAuth()
		case <-c.inputStateCh:
			if c.inputAwaitingClient() {
				return ErrInputPending
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
			if c.inputAwaitingClient() {
				return ErrInputPending
			}
			if ready, wall := check(); ready {
				return nil
			} else if wall {
				return ErrAuthRequired
			}
		}
	}
}

// chatClaudeCode is the adapter-style name for the Claude Code harness.
const chatClaudeCode = "claude-code"

func requiresPromptReadiness(harness string) bool {
	switch harness {
	case chatClaudeCode, "codex", "pi":
		return true
	default:
		return false
	}
}

func readyForInput(harness, text string) bool {
	switch harness {
	case chatClaudeCode:
		// A first-run onboarding WIZARD (theme picker, "Select login method")
		// paints the "Claude Code" header and a "❯" menu selector, so it would
		// otherwise look ready — but it is waiting for menu input and never turns
		// into a usable composer on its own. Treat it as not-ready so Send's auth
		// gate short-circuits it instead of typing the prompt into the wizard.
		if onboardingWall(harness, text) {
			return false
		}
		// A blocking dialog (folder-trust, bypass acceptance) renders its own
		// "❯" selector and the "Claude Code" header, which would otherwise
		// look ready. Treat the dialog as not-ready so Send waits for it to
		// clear instead of typing the message into the menu.
		if _, blocking := claudecode.DetectInput(text); blocking {
			return false
		}
		// The "Claude Code" startup banner is deliberately NOT required. It is a
		// scroll-position artifact, not a readiness signal: on any reply long
		// enough to fill the viewport the banner scrolls out, so requiring it made
		// a settled post-turn screen read as not-ready. That is what left
		// maybeIdleComplete's fallback inert on Claude Code 2.1.247, whose settled
		// summary ("✻ Churned for 2m 27s · done 2:26 AM") the end-of-turn marker
		// regex was also rejecting — so a finished turn had no way at all to
		// complete. Recorded on the real thing: test/corpus/claude-code/
		// settled-after-turn, a settled 2.1.247 frame with the banner scrolled off.
		//
		// The composer prompt alone is therefore the readiness signal. The "is it
		// actually finished" half comes from the turns.BusyDetector gate that
		// maybeIdleComplete applies AFTER this check (conversation.go), and the
		// send path cannot type into a live turn either: Send rejects with
		// ErrTurnInFlight on currentTurn BEFORE waitReadyForSend is reached
		// (send.go, pinned by TestSend_TurnInFlightRejectedBeforeReadiness). Same
		// reasoning as the codex branch below.
		return strings.Contains(text, "❯")
	case "codex":
		// The never-signed-in onboarding menu ("Sign in with ChatGPT") renders a
		// "›"-highlighted row and would look ready; it is a stuck sign-in wall, so
		// treat it as not-ready and let Send's auth gate short-circuit it.
		if onboardingWall(harness, text) {
			return false
		}
		// A blocking startup interstitial (update notice, model migration)
		// renders its own "›" highlight and looks ready. Treat it as not-ready
		// so Send waits for the auto-dismiss to clear it instead of typing the
		// message into the menu. Once cleared, the idle composer's "›" prompt
		// (PromptReady) means Codex is accepting input. In-flight turns are
		// gated by currentTurn before waitReadyForSend is consulted, so the
		// composer prompt alone suffices here.
		if _, blocking := codex.DetectInput(text); blocking {
			return false
		}
		return codex.PromptReady(text)
	case "pi":
		// pi has a noisy, network-touching startup (model resolution, optional
		// fd/ripgrep download, an "Update Available" banner) during which the
		// composer is painted but not yet listening. Gate Send until pi's idle
		// status line is up and no turn is mid-flight, so the prompt + CR aren't
		// typed into a composer that drops them.
		return pi.PromptReady(text)
	default:
		return true
	}
}

// Logged-out / re-authentication AND not-yet-onboarded banners, per harness. A
// harness whose CLI login has expired, was never established, or that is still
// sitting in first-run onboarding produces NO assistant output for the turn. The
// anchors are grounded in real observed CLI output, not invented — see
// test/corpus/auth for the captured screen each one matches:
//   - claude-code: "Not logged in · Please run /login" (logged out); "Invalid API
//     key · Fix external API key" (bad external key); the first-run onboarding
//     "Choose the text style" theme picker and the "Select login method" screen.
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
	claudeOnboardingRE = []*regexp.Regexp{
		regexp.MustCompile(`(?i)choose the text style`), // theme picker
		regexp.MustCompile(`(?i)select login method`),   // login-method screen
	}
	codexOnboardingRE = []*regexp.Regexp{
		regexp.MustCompile(`(?i)sign in with chatgpt`),               // never-signed-in menu
		regexp.MustCompile(`(?i)finish signing in via your browser`), // the login flow the menu advances into (browser + device-code)
	}
	// Logged-out / bad-key banners left on an otherwise-ready screen. Handled on
	// the completion path (a turn that yielded no reply), not by refusing to send.
	claudeLoggedOutRE = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\brun /login\b`),
		regexp.MustCompile(`(?i)\bnot logged in\b`),
		regexp.MustCompile(`(?i)\binvalid api key\b`),
	}
	codexLoggedOutRE = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b401 unauthorized\b`),
		regexp.MustCompile(`(?i)missing bearer or basic authentication`),
		regexp.MustCompile(`(?i)\bnot logged in\b`),
		regexp.MustCompile(`(?i)\bcodex(?: mcp)? login\b`),
	}
)

func anyMatch(res []*regexp.Regexp, text string) bool {
	for _, re := range res {
		if re.MatchString(text) {
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
	case chatClaudeCode:
		return anyMatch(claudeOnboardingRE, text)
	case "codex":
		return anyMatch(codexOnboardingRE, text)
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
// Mirrored inline from the wrapper's sessionLimitRE (pkg/wrapper/internal/harness/
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
	case chatClaudeCode:
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
	case chatClaudeCode:
		return anyMatch(claudeOnboardingRE, text) || anyMatch(claudeLoggedOutRE, text)
	case "codex":
		return anyMatch(codexOnboardingRE, text) || anyMatch(codexLoggedOutRE, text)
	default:
		return false
	}
}

func submitKeyForHarness(harness, screenText string) []byte {
	switch harness {
	case chatClaudeCode:
		// Claude Code enables enhanced keyboard handling in its TUI and does
		// not submit the input box when a synthetic PTY writer sends plain
		// CR/LF — it only inserts a newline and the turn never runs. CSI 13 u
		// is the unmodified Enter key in that mode. Recent versions (≥2.1.x)
		// turn enhanced mode on unconditionally at startup — the auto-mode
		// composer shows neither "bypass permissions" nor "ctrl+g to edit in
		// Vim" — so we always send the enhanced Enter, mirroring codex below.
		return []byte("\x1b[13u")
	case "codex":
		// codex 0.141.0 turns on the enhanced (kitty) keyboard protocol at startup,
		// so a plain CR/LF from a synthetic PTY writer is NOT treated as submit — it
		// only inserts a newline in the composer and the turn never runs. CSI 13 u is
		// the unmodified Enter key in that mode (same as claude-code's enhanced TUI).
		// 0.140.0 accepted "\n", but enhanced mode is unconditional now.
		return []byte("\x1b[13u")
	case "pi":
		// pi's composer submits on a carriage return (the actual Enter byte); a bare
		// "\n" (line feed) is NOT treated as submit — it leaves the typed prompt sitting
		// in the composer unsent (verified live against pi 0.76.0: the prompt rendered
		// in the input box but the turn never ran). pi does NOT enable the kitty keyboard
		// protocol (only bracketed-paste / synchronized-output), so the enhanced CSI 13u
		// that claude-code/codex need is unnecessary — a plain CR submits.
		return []byte("\r")
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
	switch harness {
	case "claude", chatClaudeCode:
		// Verified live on 2.1.217: cycles auto → manual in the status line.
		return []byte(shiftTabCSI9_2u)
	case "codex":
		// Verified live on 0.144.5: cycles the footer into "Plan mode".
		return []byte(shiftTabCSI9_2u)
	default:
		// Unknown harnesses, and pi, get nil rather than a best-guess keystroke.
		// pi has no permission-mode cycle to drive at all: verified live on
		// 0.76.0 that Shift+Tab there reaches a *thinking* toggle ("Current
		// model does not support thinking") and leaves no mode indicator
		// changed. Returning nil lets the caller fail loudly on "this harness
		// has no Shift+Tab contract" instead of writing bytes that quietly do
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
// pi is deliberately LEFT OUT even though it also enables the mode (see the
// note at readyForInput): pi's composer has never been measured against a >2KB
// prompt, and pi is not what the fleet runs. Unmeasured harnesses keep today's
// behaviour — that is the whole point of a per-harness table. codex is included
// on the corpus evidence alone; if it ever proves wrong there, it comes out
// here and nowhere else.
func pasteWrapForHarness(harness string) (prefix, suffix []byte) {
	switch harness {
	case "claude", chatClaudeCode, "codex":
		return []byte(pasteStartCSI200), []byte(pasteEndCSI201)
	default:
		return nil, nil
	}
}
