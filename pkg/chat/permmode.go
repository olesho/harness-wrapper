package chat

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// This file is the LIVE half of permission-mode control: the passive read
// (Conversation.PermissionMode) and the mid-session switch
// (Conversation.SetPermissionMode). The pure per-harness screen parsers live
// behind turns.PermissionModeDetector (pkg/turns/harness/{claudecode,codex});
// this file never parses a screen itself, so there is exactly ONE harness
// switch here — permissionModeCapabilities, the target-set gate.
//
// It borrows pkg/chat/discover.go's IDIOMS — typed sentinels, a bounded render
// budget, the pickerPollInterval cadence, c.closed in every select — but NONE of
// its lifecycle, and the difference is the whole safety story:
//
//   - DiscoverModels opens its OWN ephemeral, memstore-backed session and
//     AcquireControls it internally (discover.go:104-130). Nothing it does can
//     corrupt a caller's session, because it never touches one: the probe is
//     read-only and the session is thrown away on return.
//
//   - SetPermissionMode mutates the CALLER'S LIVE session. Every keystroke it
//     writes lands in the conversation the caller is using, and the posture it
//     leaves behind outlives the call. That is why it requires the caller to
//     already hold the control token rather than taking one itself, why it
//     refuses to press through an open modal, and why it restores the starting
//     posture when it cannot reach the target — a partially cycled session
//     silently sitting in `bypass` is the failure mode this driver is designed
//     against.

// Permission-mode sentinels. They are the SetPermissionMode error contract;
// ErrNoControl, ErrTurnInFlight, ErrInputPending, ErrAuthRequired and ErrClosed
// (all in chat.go) are inherited from the preconditions and readiness gate.
var (
	// ErrPermissionModeUnsupported is returned by SetPermissionMode for a
	// harness with no permission-mode cycle this driver can drive — anything
	// but claude/claude-code and codex (opencode, pi, generic). Call sites wrap
	// it with the harness name; errors.Is still matches.
	ErrPermissionModeUnsupported = errors.New("chat: harness does not support permission-mode switching")

	// ErrPermissionModeUnreachable is returned by SetPermissionMode when the
	// target is not in the harness's reachable set. It is the single code path
	// that also rejects the LAUNCH-ONLY spellings — claude's "dontAsk", codex's
	// "read-only"/"workspace-write"/"danger-full-access", and every canonical
	// rung other than "plan" on codex (whose cycle drives the collaboration
	// axis only) — and the "bypass on a session that was not launched
	// bypass-enabled" case, which fast-fails before a single keystroke is
	// written.
	ErrPermissionModeUnreachable = errors.New("chat: permission mode not reachable on this harness")

	// ErrPermissionModeSwitchFailed is returned by SetPermissionMode when the
	// target was never observed within the press bound AND the starting posture
	// was successfully restored. The session is where it started; the switch
	// simply did not happen.
	ErrPermissionModeSwitchFailed = errors.New("chat: permission mode switch did not take effect")

	// ErrPermissionModeIndeterminate is returned by SetPermissionMode when the
	// switch failed AND restoration also failed AND the last observed posture is
	// strictly MORE PERMISSIVE than the starting one (wrapper.MorePermissive —
	// unknown values are never more permissive, so this fails closed). It is the
	// loud signal that the session was left looser than the caller found it.
	ErrPermissionModeIndeterminate = errors.New("chat: permission mode indeterminate — session may be more permissive than it started")

	// ErrPermissionModeBlockedByInput is returned by SetPermissionMode when a
	// BLOCKING interactive request (most importantly claude's bypass-acceptance
	// dialog) appeared while cycling. The driver stops pressing immediately
	// rather than typing Shift+Tab into an open modal.
	//
	// The concrete error is a *PermissionModeBlockedError carrying the
	// chat.InputRequest — the client-facing shape PendingInput returns and
	// Answer round-trips. Match it with errors.Is for the sentinel and
	// errors.As for the request:
	//
	//	var blocked *chat.PermissionModeBlockedError
	//	if errors.As(err, &blocked) {
	//	    _ = conv.Answer(ctx, blocked.Request.ID, chat.InputAnswer{OptionID: "1"})
	//	    mode, err = conv.SetPermissionMode(ctx, "bypass") // retry, token still held
	//	}
	ErrPermissionModeBlockedByInput = errors.New("chat: permission mode switch blocked by an interactive request")

	// ErrCodexPlanRefusedBusy is returned by SetPermissionMode(ctx, "plan") on
	// codex when codex kept refusing the `/plan` command with
	// "'/plan' is disabled while a task is in progress." for every one of
	// codexPlanRetryAttempts submissions. Startup MCP-server boot counts as "a
	// task in progress" while the `›` composer is already painted, so
	// prompt-readiness is not a sufficient gate and this refusal — not
	// ErrTurnInFlight — is the load-bearing guard on codex.
	//
	// It is ALSO returned, wrapped around ctx.Err(), when the CALLER'S ctx ended
	// the drive after at least one refusal had been seen. The retry bound is an
	// attempt count, so ctx is the only wall clock left (see codexEnterPlan): a
	// caller whose ctx is shorter than the worst-case drive would otherwise get a
	// bare context error and lose the one diagnosis whose remedy is "wait". Both
	// sentinels match under errors.Is, so a caller should check ctx.Err() /
	// errors.Is(err, context.Canceled) FIRST — a deliberate cancellation is not a
	// busy codex, and the "wait for the task to finish, then retry" remedy does
	// not apply to it.
	ErrCodexPlanRefusedBusy = errors.New("chat: codex refused /plan while a task is in progress")
)

// PermissionModeBlockedError is the concrete error behind
// ErrPermissionModeBlockedByInput. It carries the client-facing
// chat.InputRequest (NOT the internal turns.InputRequest): exactly the value
// PendingInput returns and Answer accepts, so the documented
// resolve-then-retry recovery round-trips without a type conversion the caller
// cannot perform.
//
// Observed is the last posture read before the driver stopped pressing; it is
// also returned as SetPermissionMode's string result.
type PermissionModeBlockedError struct {
	// Request is the blocking prompt that stopped the cycle.
	Request InputRequest
	// Observed is the last readable posture, or "" when the modal covered the
	// marker before any read succeeded.
	Observed string
}

func (e *PermissionModeBlockedError) Error() string {
	return fmt.Sprintf("%s: %s (kind %q, id %q)",
		ErrPermissionModeBlockedByInput.Error(), e.Request.Prompt, e.Request.Kind, e.Request.ID)
}

// Unwrap makes errors.Is(err, ErrPermissionModeBlockedByInput) match.
func (e *PermissionModeBlockedError) Unwrap() error { return ErrPermissionModeBlockedByInput }

// codexCollabPlan and codexCollabDefault are codex's COLLABORATION-axis values
// — the only two SetPermissionMode can reach there. They are not permission
// rungs; see turns.PermissionModeDetector for the axis distinction.
const (
	codexCollabPlan    = "plan"
	codexCollabDefault = "default"
)

// permissionRungBypass is the one rung whose reachability depends on how the
// session was LAUNCHED, so it is named rather than indexed.
const permissionRungBypass = "bypass"

// defaultPermissionModeRenderTimeout bounds how long the driver waits for the
// harness to REPAINT after one cycle keystroke before pressing again. It is the
// per-press twin of defaultPickerRenderTimeout (discover.go:48) — much shorter,
// because a footer redraw is a single frame, not a picker screen build.
const defaultPermissionModeRenderTimeout = 2 * time.Second

// codexPlanRetryAttempts is how many times codexEnterPlan SUBMITS `/plan`
// before giving up with ErrCodexPlanRefusedBusy. MCP-server boot is the common
// cause of the refusal and clears on its own within a few seconds.
//
// It is an ATTEMPT COUNT, not a wall clock, and that is the whole point: it
// matches cyclePermissionMode's already-attempt-based bound (which likewise
// treats the ring length as "only a bound" and settles the outcome by
// re-reading, never by arithmetic), so both drive paths bound the same way. A
// wall clock could not do this job, because a refused `/plan` produces NO
// posture change, so every attempt necessarily spends one whole repaint budget
// inside awaitPostureChange — a clock would buy budget/(write+await) attempts
// rather than the documented five, and under the hermetic tests' shrunken
// repaint budget that degrades to a random 1–3. Counting attempts makes the
// shrunken budget make each attempt FAST without making the loop SHORTER.
//
// The invariant this rests on: a refusal never changes the observed posture, so
// no attempt can return early and every one of the five is a real submission.
// (awaitPostureChange does return early on ANY posture change, but reaching that
// with a refusal requires an unreadable seed posture, which waitReadyForSend
// already excludes on codex — see codexEnterPlan.)
const codexPlanRetryAttempts = 5

// codexPlanRefusalRE matches codex's refusal banner for `/plan`:
//
//	■ '/plan' is disabled while a task is in progress.
//
// Recognized HERE, inside the driver, and deliberately NOT in codex.DetectInput:
// readyForInput calls that detector's blocking arm (ready.go:176-180), so
// classifying the banner as an InputRequest would make every screen carrying it
// read as not-ready and deadlock the send path.
//
// The leading glyph is optional and the quoting is tolerated in both the
// typographic and ASCII forms, so an emulator that renders the box-drawing
// bullet differently — or a codex build that drops it — still matches.
var codexPlanRefusalRE = regexp.MustCompile(
	`(?i)['‘’"]?/plan['‘’"]?\s+is\s+disabled\s+while\s+a\s+task\s+is\s+in\s+progress`,
)

// PermissionMode returns the posture read off the current screen, per
// turns.PermissionModeDetector's contract (a canonical rung on claude, a
// collaboration-axis value on codex). false = no readable signal. No control
// token, no readiness gate, valid mid-turn.
//
// It is a pure adapter consult in exactly the shape conversation.go:798 already
// uses for turns.BusyDetector: type-assert the adapter, read the current
// snapshot, and answer ("", false) when the adapter does not implement the
// capability (opencode, pi, generic paint no marker this could read).
//
// Because it neither takes the control token nor gates on readiness, it is safe
// to call at any time — including during an in-flight turn, when claude keeps
// its footer painted (see claudecode/busy_test.go:16). "false" means the SCREEN
// carries no signal (an onboarding wall, a modal covering the footer), never
// "readable, and not the mode you asked about".
func (c *Conversation) PermissionMode() (string, bool) {
	d, ok := c.adapter.(turns.PermissionModeDetector)
	if !ok {
		return "", false
	}
	return d.PermissionMode(c.ScreenSnapshot())
}

// SetPermissionMode cycles the harness to target and returns the FINAL
// OBSERVED posture on both the success and every failure path. That is why the
// signature is (string, error): after a failed switch the caller still needs to
// know where the session actually ended up, and a bare error cannot say.
//
// Reachable targets, per harness (permissionModeCapabilities):
//   - claude / claude-code: the five canonical rungs, wrapper.PermissionRungs()
//     — plan, manual, ask, auto, bypass.
//   - codex: the COLLABORATION axis only — "plan" and "default". codex's
//     permissions/sandbox axis is a launch knob this driver does not touch.
//   - every other harness: ErrPermissionModeUnsupported.
//
// Anything else — claude's launch-only "dontAsk", codex's "read-only" /
// "workspace-write" / "danger-full-access", or a canonical rung like "auto" on
// codex — returns ErrPermissionModeUnreachable. So does "bypass" on a claude
// session that was NOT launched bypass-enabled: claude's cycle is 4 rungs long
// there and bypass is simply not on the ring, so the call fast-fails without
// writing a single keystroke.
//
// # Preconditions
//
// The caller MUST already hold the control token (AcquireControl); otherwise
// SetPermissionMode returns ErrNoControl. It mirrors Send here and deliberately
// NOT DiscoverModels: controlQueue is an exclusive, FIFO, NON-REENTRANT mutex
// (control.go:17-29), so a self-acquiring driver would deadlock the natural
// caller sequence
//
//	release, _ := conv.AcquireControl(ctx)
//	defer release()
//	mode, err := conv.SetPermissionMode(ctx, "plan")
//
// to the context deadline. Requiring the token is also what makes the documented
// ErrPermissionModeBlockedByInput recovery possible at all — the caller KEEPS
// the token across resolve-then-retry:
//
//	var blocked *chat.PermissionModeBlockedError
//	if errors.As(err, &blocked) {
//	    _ = conv.Answer(ctx, blocked.Request.ID, chat.InputAnswer{OptionID: "proceed"})
//	    mode, err = conv.SetPermissionMode(ctx, target)
//	}
//
// Holding the token does not imply idleness, so an in-flight turn is checked
// separately and fast-fails ErrTurnInFlight, exactly as Send does
// (send.go:41-44). NOTE the gate is materially WEAKER ON CODEX: c.currentTurn is
// cleared by maybeIdleComplete (conversation.go:769), which consults
// turns.BusyDetector — implemented by claudecode and pi ONLY. On codex the turn
// clears on the idle-gap path alone and can never see harness-internal work such
// as MCP-server boot, so ErrTurnInFlight is a courtesy check there and the
// `/plan`-refusal detection (ErrCodexPlanRefusedBusy) is the load-bearing guard.
//
// Readiness is gated on waitReadyForSend before the first press, which inherits
// ErrInputPending for a request already surfaced to the client and ErrAuthRequired
// for a real onboarding wall. Mind the readiness SPLIT: the soft logged-out
// screens (test/corpus/auth/claude-code/not-logged-in-{churned,brewed}) PASS
// readiness — readyForInput wins first, "a real composer (even with a stale banner
// scrolled above) is never auth-gated" (ready.go:76-79) — so the switch proceeds
// on them. Only the real onboarding wall (the theme picker, ready.go:224-227)
// yields ErrAuthRequired.
//
// # Write ordering
//
// Holding the token serializes this driver against Send and Answer, but NOT
// against the watcher goroutine's automatic input resolution: tryAutoDismissCodex
// (input.go:220) and tryResolveInput (input.go:239) call c.write WITHOUT the
// token, so their keystrokes can interleave with the cycle. c.write takes no lock
// and this driver adds none. That is precisely why the press loop STOPS on a
// blocking request instead of pressing through one — and why it waits, rather
// than presses, while a policy is mid-resolve.
//
// # InputPolicy interaction, in both directions
//
// Switching DOWN toward manual starts producing KindApproval requests the client
// must now answer: a session that ran unattended under `auto` will begin blocking
// on Events() the moment it reaches a restrictive rung, so a policy (or a live
// answerer) has to be in place before the switch, not after.
//
// Switching UP toward bypass does the opposite: a configured ByKind entry for an
// approval kind goes silently DEAD, because the harness stops asking. It may also
// first raise claude's bypass ACCEPTANCE dialog — bypass sets no IS_SANDBOX=1, so
// claude still shows it (see cmd/harness-wrapper/testdata/usage.golden:22-30).
// That dialog is exactly what `--auto-accept` (cmd/harness-wrapper/flags.go:132)
// or an InputPolicy entry is for. Without one, the upward switch surfaces the
// dialog and returns ErrPermissionModeBlockedByInput rather than hanging.
//
// ACCEPTED RESIDUAL — a dontAsk session. Claude's native dontAsk launch reports
// the "manual" rung (it ties with claude's default in claude's own permissiveness
// rank table, and the rung ladder is a strict total order). So SetPermissionMode
// (ctx, "manual") on such a session sees start == target and returns early
// WITHOUT a keystroke: the session keeps auto-DENYING everything not pre-approved
// instead of surfacing approvals. Permissiveness-wise that is safe — equal rank,
// and strictly more restrictive in effect — but a caller whose InputPolicy expects
// KindApproval requests silently receives none. Distinguishing the two spellings
// would require turns.PermissionModeDetector to carry the native spelling
// alongside the rung, an interface change across every adapter; it is deliberately
// not worked around here.
//
// # Scope: process-local, NOT persisted
//
// PermissionMode is a LAUNCH knob replayed on resume (ReopenOptions.PermissionMode
// → launch.PermissionMode → wrapper.Config.PermissionMode), and the chat.Session
// record carries no observed rung. Persisting one would add a field to
// chat.Session and thereby to every Store implementation, so the decision is:
// process-local and documented. The consequence is explicit — the "never silently
// more permissive than it started" invariant holds WITHIN A SINGLE Conversation
// LIFETIME ONLY. Reopen resets the posture to the launch rung; a mid-session
// switch to "plan" does not survive it.
func (c *Conversation) SetPermissionMode(ctx context.Context, target string) (string, error) {
	select {
	case <-c.closed:
		return "", ErrClosed
	default:
	}

	// Harness/target gate first: it is pure, writes nothing, and needs no token,
	// so an unreachable target is rejected identically whether or not the caller
	// happens to hold one.
	targets, ok := permissionModeCapabilities(c.opts.Harness)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrPermissionModeUnsupported, c.opts.Harness)
	}
	if !containsString(targets, target) {
		return "", fmt.Errorf("%w: %q on %q", ErrPermissionModeUnreachable, target, c.opts.Harness)
	}

	if !c.queue.Held() {
		return "", ErrNoControl
	}

	c.mu.Lock()
	inFlight := c.currentTurn != nil
	c.mu.Unlock()
	if inFlight {
		return "", ErrTurnInFlight
	}

	ringLen, bypassOnRing := c.cycleRing()
	if target == permissionRungBypass && !bypassOnRing {
		// Definite non-bypass launch with no bypass-enabling flag: bypass is not
		// on this session's ring. Fail before writing anything — cycling would
		// just walk the 4-ring forever and leave the session somewhere else.
		return "", fmt.Errorf("%w: %q requires a bypass-enabled launch (--permission-mode=bypassPermissions or %s)",
			ErrPermissionModeUnreachable, target, wrapper.SkipPermissionsFlag)
	}

	if err := c.waitReadyForSend(ctx); err != nil {
		observed, _ := c.PermissionMode()
		return observed, err
	}

	start, _ := c.PermissionMode()
	if start == target {
		return start, nil
	}

	bound := 2 * ringLen
	final, err := c.driveToPermissionMode(ctx, target, bound)
	if err == nil {
		return final, nil
	}
	if !errors.Is(err, errPermissionModeBoundExhausted) {
		// Blocked by a modal, ctx/closed, a write failure, or codex's `/plan`
		// refusal. Restoration is not attempted through an open dialog (the ring
		// is unreachable there), and a cancelled context has no budget left to
		// restore under. Report the last observed posture with the cause.
		return final, err
	}

	// The target was never observed. Restore the STARTING posture by continuing
	// to cycle — the ring is a cycle, so the start is always reachable — under a
	// second, equal bound.
	restored, rerr := c.cyclePermissionMode(ctx, start, bound)
	if rerr == nil {
		return restored, fmt.Errorf("%w: %q not observed within %d presses (restored %q)",
			ErrPermissionModeSwitchFailed, target, bound, restored)
	}
	if !errors.Is(rerr, errPermissionModeBoundExhausted) {
		return restored, rerr
	}
	if wrapper.MorePermissive(restored, start) {
		return restored, fmt.Errorf("%w: started %q, left at %q while trying to reach %q",
			ErrPermissionModeIndeterminate, start, restored, target)
	}
	return restored, fmt.Errorf("%w: %q not observed within %d presses and %q not restored (left at %q)",
		ErrPermissionModeSwitchFailed, target, bound, start, restored)
}

// errPermissionModeBoundExhausted is the INTERNAL marker meaning "the press
// bound elapsed without observing the wanted posture". It never escapes
// SetPermissionMode: the caller-facing outcome depends on whether restoration
// then succeeded (ErrPermissionModeSwitchFailed) or not
// (ErrPermissionModeIndeterminate), which only SetPermissionMode can decide.
var errPermissionModeBoundExhausted = errors.New("chat: permission mode cycle bound exhausted")

// driveToPermissionMode performs the harness-appropriate switch to target.
// codex's "plan" is reached with the `/plan` slash command (with the
// refusal-retry budget); everything else is cycle-and-check via Shift+Tab.
func (c *Conversation) driveToPermissionMode(ctx context.Context, target string, bound int) (string, error) {
	if c.opts.Harness == "codex" && target == codexCollabPlan {
		return c.codexEnterPlan(ctx)
	}
	return c.cyclePermissionMode(ctx, target, bound)
}

// cyclePermissionMode is the CYCLE-AND-CHECK loop: press Shift+Tab, re-read the
// posture through turns.PermissionModeDetector, repeat until target is observed
// or the bound elapses. It NEVER counts presses to infer the mode — the ring
// length is only a bound, so a surprising ring order or a repaint that lands two
// modes later is settled by re-reading, not by arithmetic.
//
// Between every press it re-checks the input state and refuses to write into an
// open modal (see pressGate).
func (c *Conversation) cyclePermissionMode(ctx context.Context, target string, bound int) (string, error) {
	keys := shiftTabForHarness(c.opts.Harness, c.screen.Snapshot().Text)
	if len(keys) == 0 {
		// Defensive: permissionModeCapabilities already rejected every harness
		// without a Shift+Tab contract, so this is unreachable today.
		observed, _ := c.PermissionMode()
		return observed, fmt.Errorf("%w: %q has no Shift+Tab encoding", ErrPermissionModeUnsupported, c.opts.Harness)
	}

	last, _ := c.PermissionMode()
	for i := 0; i < bound; i++ {
		if last == target {
			return last, nil
		}
		if err := c.pressGate(ctx, last); err != nil {
			return last, err
		}
		if err := c.write(keys); err != nil {
			return last, fmt.Errorf("chat: set permission mode: write shift+tab: %w", err)
		}
		observed, err := c.awaitPostureChange(ctx, last)
		if observed != "" {
			last = observed
		}
		if err != nil {
			return last, err
		}
	}
	if last == target {
		return last, nil
	}
	return last, errPermissionModeBoundExhausted
}

// codexEnterPlan writes `/plan` + codex's submit key and waits for the
// collaboration axis to report "plan", retrying while codex answers with the
// "disabled while a task is in progress" refusal banner. The retry bound is
// codexPlanRetryAttempts SUBMISSIONS, never a wall clock — see that constant for
// why. On exhaustion it returns ErrCodexPlanRefusedBusy if a refusal was ever
// seen, and the ordinary bound-exhausted marker otherwise (so the caller's
// restore path runs).
//
// # ctx is the only wall clock
//
// Because the bound counts attempts, the CALLER'S ctx is the sole time limit.
// Worst-case wall time is therefore worth stating, and it is NOT parity with the
// old clock: every iteration also runs pressGate, which carries its own full
// permModeRenderBudget().
//
//   - typical refusal path (nothing pending, so pressGate returns immediately):
//     5 × 2s ≈ 10s at the default, which is the documented intent, and now exact
//     rather than approximate;
//   - worst case (a pending-but-unsurfaced request on every iteration):
//     5 × (2s pressGate + 2s await) ≈ 20s, up from the old ≈ 14s.
//
// Exhaustion costs no restore time on top of that: SetPermissionMode returns the
// refusal sentinel WITHOUT restoring, and in the no-refusal case cyclePermissionMode
// returns at its loop-top check because a refusal never moved the posture.
//
// A ctx shorter than that ends the drive early, which is why both raw
// error returns below re-wrap a ctx abort in ErrCodexPlanRefusedBusy once a
// refusal has been seen: the sentinel is the one error whose remedy is "wait for
// the task to finish", and a tight ctx must not erase it.
//
// # Why every attempt is a real submission
//
// A refused `/plan` produces NO posture change — codex paints no Plan marker and
// its detector reports the absence as ("default", true), never as unknown — so
// awaitPostureChange always runs to its full budget and the attempt count cannot
// be spent by cheap iterations. awaitPostureChange DOES return early on any
// posture change, and the one way to combine that with a refusal is an
// unreadable seed (`last == ""`, after which the first await returns the moment a
// posture becomes readable). On codex that is effectively unreachable:
// waitReadyForSend must pass first, which needs a painted composer, while the
// detector answers ("", false) only for a screen with no readable signal at all —
// mutually exclusive. And `last` never returns to "" afterwards, since only
// non-empty observations are assigned. So no "don't count cheap attempts" guard
// is added here: it would trade a hard, provably-terminating bound for a
// conditional one, to prevent a loss of precision (one of five attempts) rather
// than of termination. If codex's detector ever starts reporting unknown on a
// ready screen, the guard to add is "skip the increment when the iteration
// observed a posture change and matched no refusal, at most once per call".
func (c *Conversation) codexEnterPlan(ctx context.Context) (string, error) {
	refused := false
	last, _ := c.PermissionMode()

	for n := 0; ; n++ {
		if err := c.pressGate(ctx, last); err != nil {
			if refused && ctxAborted(err) {
				return last, fmt.Errorf("%w: %w", ErrCodexPlanRefusedBusy, err)
			}
			return last, err
		}
		screenText := c.screen.Snapshot().Text
		cmd := append([]byte("/plan"), submitKeyForHarness(c.opts.Harness, screenText)...)
		if err := c.write(cmd); err != nil {
			return last, fmt.Errorf("chat: set permission mode: write /plan: %w", err)
		}

		observed, err := c.awaitPostureChange(ctx, last)
		if observed != "" {
			last = observed
		}
		if err != nil {
			if refused && ctxAborted(err) {
				return last, fmt.Errorf("%w: %w", ErrCodexPlanRefusedBusy, err)
			}
			return last, err
		}
		if last == codexCollabPlan {
			return last, nil
		}
		if codexPlanRefusalRE.MatchString(c.screen.Snapshot().Text) {
			refused = true
		}
		if n+1 >= codexPlanRetryAttempts {
			if refused {
				return last, fmt.Errorf("%w (retried %d times)", ErrCodexPlanRefusedBusy, codexPlanRetryAttempts)
			}
			return last, errPermissionModeBoundExhausted
		}
	}
}

// ctxAborted reports whether err is the caller's ctx ending the drive, in either
// spelling. codexEnterPlan uses it to keep ErrCodexPlanRefusedBusy visible when
// the caller's ctx is tighter than the attempt bound: after the attempt bound
// replaced the wall clock, ctx is the ONLY clock left, and a sub-20s ctx would
// otherwise erase the one error whose remedy is "wait".
//
// It deliberately tests the RETURNED ERROR rather than ctx.Err(), which is the
// narrow form: a *PermissionModeBlockedError landing in the same instant as ctx
// expiry is then never mislabelled as a refusal.
func ctxAborted(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// pressGate reports whether it is safe to write a cycle keystroke NOW.
//
// Three states, and the distinction is the security-relevant part:
//   - no pending request        → nil, press away.
//   - pending AND surfaced      → *PermissionModeBlockedError. A modal the client
//     must answer is up (claude's bypass-acceptance dialog is the case this
//     exists for); the ring is unreachable through it and Shift+Tab must never be
//     typed into it.
//   - pending, NOT surfaced     → a policy/handler is mid-resolve. Wait for it to
//     clear rather than racing its keystrokes; if it never clears within the
//     render budget, report it as blocked with the request in hand.
//
// The budget is a HARD bound: see the expiry check below for why the deadline
// arm alone is not one.
func (c *Conversation) pressGate(ctx context.Context, observed string) error {
	budget := c.permModeRenderBudget()
	expiry := time.Now().Add(budget)
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	ticker := time.NewTicker(permModePollInterval(budget))
	defer ticker.Stop()

	for {
		c.mu.Lock()
		pending := c.currentInput
		surfaced := c.inputSurfaced
		c.mu.Unlock()

		if pending == nil {
			return nil
		}
		if surfaced {
			return &PermissionModeBlockedError{Request: toClientInputRequest(pending), Observed: observed}
		}

		// The select WAKES; the body DECIDES — see closedNow.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if closedNow(c.closed) {
			return ErrClosed
		}
		if time.Now().After(expiry) {
			// Auto-resolution never cleared it; treat it as blocking rather than
			// pressing into a dialog that is evidently still up.
			return &PermissionModeBlockedError{Request: toClientInputRequest(pending), Observed: observed}
		}

		select {
		case <-ctx.Done():
		case <-c.closed:
		case <-deadline.C:
		case <-ticker.C:
		}
	}
}

// closedNow reports whether ch is already closed, without blocking. It exists
// because of the shape shared by pressGate, awaitPostureChange and
// DiscoverModels's render-budget loop, documented once here rather than three
// times: the select's arms carry NO returns — they only WAKE the loop — and
// every terminal decision is taken in the loop body, in an explicit order.
//
// A select picks UNIFORMLY AT RANDOM among ready cases, so arms are not
// priorities. Once one poll body outlasts the tick — which permModePollInterval's
// quarter-of-budget rule makes true at the hermetic scale, where a 120×40
// ScreenSnapshot plus the detector costs more than a 10ms tick — ticker.C is
// ready on every pass, and any other ready arm loses a coin flip to it. The
// consequences were two live bugs: a "bounded" wait whose length was a geometric
// random variable with an unbounded tail, and a cancelled ctx that could be
// swallowed by the deadline arm and reported as an ordinary elapsed budget —
// which would in turn have made ErrCodexPlanRefusedBusy's ctx-abort wrap
// (codexEnterPlan) a coin flip.
//
// Deciding in the body fixes both, and fixes them in a stated ORDER: ctx and
// close abort first (an aborted drive must never be reported as a quiet
// success), then the budget. The checks sit at the END of the body rather than
// the top, which deliberately preserves the final read — work that completes on
// the last tick (a repaint that landed, a policy that resolved the request) is
// still honored before the loop gives up.
func closedNow(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// awaitPostureChange waits for the harness to repaint after one cycle keystroke,
// on a bounded budget. It returns the posture currently on screen — which the
// caller uses as the new "last" — and only errors for conditions that must abort
// the whole drive: ctx, close, or a blocking request that appeared mid-cycle.
//
// A budget that elapses WITHOUT a change is NOT an error: the loop simply presses
// again, and the outer bound is what stops it. That is the cycle-and-CHECK
// contract — never assume a press landed, never assume it did not.
//
// The budget is a HARD bound: see the expiry check below for why the deadline
// arm alone is not one.
func (c *Conversation) awaitPostureChange(ctx context.Context, prev string) (string, error) {
	budget := c.permModeRenderBudget()
	expiry := time.Now().Add(budget)
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	ticker := time.NewTicker(permModePollInterval(budget))
	defer ticker.Stop()

	for {
		if c.inputAwaitingClient() {
			if req := c.PendingInput(); req != nil {
				observed, _ := c.PermissionMode()
				if observed == "" {
					observed = prev
				}
				return observed, &PermissionModeBlockedError{Request: *req, Observed: observed}
			}
		}
		if observed, ok := c.PermissionMode(); ok && observed != prev {
			return observed, nil
		}

		// The select WAKES; the body DECIDES — see closedNow. Aborts are tested
		// ahead of the budget: a drive the caller cancelled must never come back
		// as a quiet "budget elapsed, no change", which is exactly what the old
		// deadline arm reported whenever it won the coin flip against ctx.Done().
		if ctx.Err() != nil {
			observed, _ := c.PermissionMode()
			return observed, ctx.Err()
		}
		if closedNow(c.closed) {
			observed, _ := c.PermissionMode()
			return observed, ErrClosed
		}
		if time.Now().After(expiry) {
			observed, _ := c.PermissionMode()
			return observed, nil
		}

		select {
		case <-ctx.Done():
		case <-c.closed:
		case <-deadline.C:
		case <-ticker.C:
		}
	}
}

// permissionModeCapabilities is the driver's ONLY harness switch — the mirror of
// pickerSupported (discover.go:183). It answers "which postures can
// SetPermissionMode reach on this harness?", and every rejection in the driver
// (unsupported harness, unreachable target, launch-only spelling) is decided
// from its answer. No screen parsing happens here or anywhere else in this file:
// that is turns.PermissionModeDetector's job.
//
//   - claude / claude-code: the five canonical rungs, taken live from
//     wrapper.PermissionRungs() so the vocabulary cannot drift.
//   - codex: the COLLABORATION axis only. codex's permissions axis
//     (read-only / workspace-write / danger-full-access) is a LAUNCH knob with
//     no in-TUI cycle, so those spellings — and every canonical rung other than
//     "plan" — are unreachable here.
//   - everything else (opencode, pi, generic): (nil, false).
func permissionModeCapabilities(harness string) (targets []string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude", chatClaudeCode:
		return wrapper.PermissionRungs(), true
	case "codex":
		return []string{codexCollabPlan, codexCollabDefault}, true
	default:
		return nil, false
	}
}

// cycleRing returns the cycle's length BOUND and whether "bypass" is on it.
//
// Working model, per the live claude TUI: the ring is 4 long normally
// (plan → manual → ask → auto) and 5 long when the session was launched
// bypass-enabled (… → bypass). The length is used ONLY as a bound — the loop
// always re-reads the posture — but bypass's presence is load-bearing, because
// it decides whether SetPermissionMode("bypass") fast-fails without writing.
//
// The launch posture is resolved through wrapper.EffectiveLaunchRung and
// wrapper.BypassEnablingFlags rather than by re-parsing argv here.
// Options.PermissionMode ALONE is not enough: argsWithHarnessPermissionMode
// (wrapper.go:595-615) suppresses injection entirely when --permission-mode or
// the skip-permissions flag is already in argv, so a caller launching with
// Options.Args = {"--permission-mode","bypassPermissions"} has
// Options.PermissionMode == "" and a bypass-enabled session.
//
// Three branches:
//   - definite bypass-class rung, or a bypass-enabling flag present → 5-ring,
//     bypass legal.
//   - definite non-bypass rung and no bypass-enabling flag → 4-ring, bypass
//     fast-fails.
//   - EffectiveLaunchRung returns "" (UNKNOWN — a trailing flag with no operand,
//     an unrecognized spelling) → NO fast-fail: assume the larger ring as the
//     bound and let cycle-and-check settle it. Unknown is never silently treated
//     as "not bypass-enabled".
//
// codex's collaboration axis is a plain 2-cycle with no launch-dependent
// variant, so it short-circuits ahead of all of this.
func (c *Conversation) cycleRing() (ringLen int, bypassOnRing bool) {
	if c.opts.Harness == "codex" {
		return 2, false
	}

	rung := wrapper.EffectiveLaunchRung(chatClaudeCode, c.opts.Args, c.opts.PermissionMode)
	if rung == permissionRungBypass {
		return 5, true
	}
	if argsContainFlag(c.opts.Args, wrapper.BypassEnablingFlags(chatClaudeCode)...) {
		return 5, true
	}
	if rung == "" {
		// Unknown launch posture: assume the larger ring and do NOT fast-fail.
		return 5, true
	}
	return 4, false
}

// permModeRenderBudget is the per-press repaint budget, honoring the unexported
// Options override the hermetic tests use to keep a deliberately-stuck fake from
// burning the bound at wall-clock speed.
func (c *Conversation) permModeRenderBudget() time.Duration {
	if c.opts.permModeRenderTimeout > 0 {
		return c.opts.permModeRenderTimeout
	}
	return defaultPermissionModeRenderTimeout
}

// permModePollInterval is the re-read cadence within one repaint budget. It
// mirrors pickerPollInterval (discover.go:52) but never exceeds a quarter of the
// budget, so a shrunk test budget still gets several reads.
func permModePollInterval(budget time.Duration) time.Duration {
	if quarter := budget / 4; quarter > 0 && quarter < pickerPollInterval {
		return quarter
	}
	return pickerPollInterval
}

// argsContainFlag reports whether argv carries any of flags, in either the bare
// ("--flag") or joined ("--flag=value") form.
func argsContainFlag(args []string, flags ...string) bool {
	for _, a := range args {
		for _, f := range flags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return true
			}
		}
	}
	return false
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
