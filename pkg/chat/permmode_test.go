package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Hermetic tests for the permission-mode driver. They drive a real screen
// emulator and the real adapters through the cheap c.writeStdin seam
// (conversation.go:178 — the same seam input_test.go:38 and quit_test.go:21
// already use), so a full switch cycle runs with no PTY and no child process:
// the fake's write handler repaints the screen exactly the way the harness
// would, and the driver reads it back through turns.PermissionModeDetector.
//
// The PTY-driven half — a real process, real Shift+Tab bytes over a real
// terminal — lives in permmode_ring_test.go.

// --- screen vocabulary ---------------------------------------------------

// claudeFooters is the live claude-code footer line for each canonical rung,
// copied from the shapes pinned in claudecode/permmode.go (captured from
// claude-code 2.1.217). "manual mode" deliberately carries NO "(shift+tab to
// cycle)" hint, matching the real footer.
var claudeFooters = map[string]string{
	"plan":   "⏸ plan mode on (shift+tab to cycle) · ← for agents",
	"manual": "⏸ manual mode on · ← for agents",
	"ask":    "⏵⏵ accept edits on (shift+tab to cycle) · ← for agents",
	"auto":   "⏵⏵ auto mode on (shift+tab to cycle) · ← for agents",
	"bypass": "⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents",
}

// claudeModeScreen is a settled claude composer painting the footer for rung.
// It carries the "Claude Code" header and the "❯" composer so readyForInput
// passes, which SetPermissionMode gates on before its first press.
func claudeModeScreen(rung string) []string {
	return []string{"Claude Code", "", "❯ ", "", claudeFooters[rung]}
}

// codexModeScreen is a settled codex composer. Codex paints a marker ONLY in
// Plan mode, right-aligned in the hint row's gutter, so "default" is the
// absence of one — exactly the asymmetry codex.collaborationMode reads.
func codexModeScreen(mode string) []string {
	lines := []string{"Codex", "", "› ", ""}
	if mode == codexCollabPlan {
		lines = append(lines, strings.Repeat(" ", 60)+"Plan mode (shift+tab to cycle)")
	}
	return lines
}

// codexPlanRefusalScreen is codex refusing the slash command while its startup
// MCP boot still counts as a task in progress. The composer is ALREADY painted
// (that is the whole trap: prompt-readiness does not gate this), so the screen
// stays readable and readyForInput keeps returning true.
func codexPlanRefusalScreen() []string {
	return []string{
		"Codex",
		"",
		"■ '/plan' is disabled while a task is in progress.",
		"",
		"› ",
		"",
	}
}

// bypassAcceptScreen is claude's bypass-acceptance dialog, the shape pinned in
// claudecode/input_test.go:24 (bypassScreen) and
// cmd/harness-wrapper/permission_pin_test.go:13. claudecode.DetectInput
// classifies it BLOCKING; the footer is gone, so the mode marker is unreadable
// while it is up — which is precisely why the ring is unreachable through it.
func bypassAcceptScreen() []string {
	return []string{
		"WARNING: Claude Code running in Bypass Permissions mode",
		"",
		"By proceeding, you accept all risks.",
		"",
		"❯ 1. Yes, I accept",
		"  2. No, exit",
		"",
	}
}

// paint repaints the emulator: clear + home, then each line with a CRLF so the
// cursor returns to column 0 (a bare "\n" would stair-step).
func paint(sc *screen.Screen, lines []string) {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	_, _ = sc.Write([]byte(b.String()))
}

// --- the hermetic fake ---------------------------------------------------

// permModeFake stands in for the harness's stdin: it records every keystroke
// the driver writes and repaints the screen the way the real TUI would.
//
// ring/idx model the harness's own permission-mode cycle. advance is the knob
// the scenarios vary: the healthy fake steps one position per Shift+Tab, a
// STUCK fake ignores the press entirely (bound exhaustion), and a RATCHET fake
// walks upward and then refuses to come back down (the indeterminate case).
type permModeFake struct {
	mu sync.Mutex

	conv    *Conversation
	sc      *screen.Screen
	harness string

	ring []string
	idx  int

	writes    [][]byte
	shiftTabs int
	planCmds  int

	// advance maps the current ring index to the next one on a Shift+Tab.
	// nil means "step forward by one", the healthy cycle.
	advance func(idx int) int

	// onPress runs after the ring index moved and before the repaint, so a
	// scenario can substitute a different screen (a modal) for this frame.
	// Returning false suppresses the normal mode repaint.
	onPress func(f *permModeFake) bool

	// onPlan runs instead of the normal repaint when the driver submits
	// "/plan". Returning false suppresses the normal repaint.
	onPlan func(f *permModeFake) bool
}

func (f *permModeFake) mode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ring[f.idx]
}

func (f *permModeFake) counts() (shiftTabs, planCmds int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shiftTabs, f.planCmds
}

// repaint paints the current ring position for the fake's harness.
func (f *permModeFake) repaint() {
	f.mu.Lock()
	mode := f.ring[f.idx]
	f.mu.Unlock()
	if f.harness == "codex" {
		paint(f.sc, codexModeScreen(mode))
		return
	}
	paint(f.sc, claudeModeScreen(mode))
}

// write is the c.writeStdin seam. It runs on the DRIVER's goroutine, so every
// repaint it performs is visible to the very next PermissionMode() read — which
// keeps these tests deterministic without sleeps.
func (f *permModeFake) write(p []byte) (int, error) {
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	f.mu.Unlock()

	switch {
	case string(p) == shiftTabCSI9_2u:
		f.mu.Lock()
		f.shiftTabs++
		if f.advance != nil {
			f.idx = f.advance(f.idx)
		} else {
			f.idx = (f.idx + 1) % len(f.ring)
		}
		f.mu.Unlock()
		if f.onPress != nil && !f.onPress(f) {
			return len(p), nil
		}
		f.repaint()
	case strings.HasPrefix(string(p), "/plan"):
		f.mu.Lock()
		f.planCmds++
		f.mu.Unlock()
		if f.onPlan != nil {
			if !f.onPlan(f) {
				return len(p), nil
			}
		} else {
			// A healthy codex accepts the slash command and enters Plan mode.
			f.mu.Lock()
			for i, m := range f.ring {
				if m == codexCollabPlan {
					f.idx = i
					break
				}
			}
			f.mu.Unlock()
		}
		f.repaint()
	default:
		// A menu answer (an InputPolicy or a client Answer resolving a modal)
		// or any other keystroke: no mode change, just repaint the composer so
		// the dialog is gone.
		f.repaint()
	}
	return len(p), nil
}

// newPermModeConv assembles a Conversation wired to a real screen and a real
// adapter but no process: opts.Harness picks the adapter, the fake owns stdin.
// The control token is NOT taken — tests that need it call AcquireControl, so
// the ErrNoControl precondition stays honest.
func newPermModeConv(t *testing.T, opts Options, ring []string, startIdx int) (*Conversation, *permModeFake) {
	t.Helper()
	if opts.Harness == "" {
		opts.Harness = chatClaudeCode
	}
	if opts.permModeRenderTimeout == 0 {
		// Short enough that the bound-exhaustion scenarios (2×ringLen presses,
		// twice over) finish in well under a second.
		opts.permModeRenderTimeout = 40 * time.Millisecond
	}
	adapter, err := resolveAdapter(opts.Harness)
	if err != nil {
		t.Fatalf("resolveAdapter(%q): %v", opts.Harness, err)
	}
	sc := screen.New(120, 40)
	conv := &Conversation{
		opts:         opts,
		adapter:      adapter,
		screen:       sc,
		eventCh:      make(chan ConversationEvent, 16),
		closed:       make(chan struct{}),
		inputStateCh: make(chan struct{}, 1),
		queue:        newControlQueue(),
	}
	fake := &permModeFake{conv: conv, sc: sc, harness: opts.Harness, ring: ring, idx: startIdx}
	conv.writeStdin = fake.write
	if len(ring) > 0 {
		fake.repaint()
	}
	return conv, fake
}

// withControl acquires the control token for the duration of the test, the way
// a real caller does. It is deliberately NOT folded into newPermModeConv: the
// whole point of the precondition is that the DRIVER never acquires it.
func withControl(t *testing.T, conv *Conversation) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	t.Cleanup(release)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

var (
	claudeRing4 = []string{"plan", "manual", "ask", "auto"}
	claudeRing5 = []string{"plan", "manual", "ask", "auto", "bypass"}
)

// --- passive read --------------------------------------------------------

// PermissionMode is a pure adapter consult: it takes no token, gates on no
// readiness, and answers ("", false) for a harness whose adapter does not
// implement turns.PermissionModeDetector.
func TestPermissionMode_AdapterConsult(t *testing.T) {
	for _, tc := range []struct {
		harness string
		lines   []string
		want    string
		wantOK  bool
	}{
		{chatClaudeCode, claudeModeScreen("plan"), "plan", true},
		{chatClaudeCode, claudeModeScreen("bypass"), "bypass", true},
		{chatClaudeCode, []string{"Claude Code", "", "❯ "}, "", false},
		{"codex", codexModeScreen(codexCollabPlan), codexCollabPlan, true},
		{"codex", codexModeScreen(codexCollabDefault), codexCollabDefault, true},
		{"pi", claudeModeScreen("plan"), "", false},
		{"opencode", claudeModeScreen("plan"), "", false},
		{"generic", claudeModeScreen("plan"), "", false},
	} {
		t.Run(tc.harness+"/"+tc.want, func(t *testing.T) {
			conv, _ := newPermModeConv(t, Options{Harness: tc.harness}, nil, 0)
			paint(conv.screen, tc.lines)
			got, ok := conv.PermissionMode()
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("PermissionMode() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// --- control token -------------------------------------------------------

// The driver requires the token and NEVER acquires it: a caller that forgot
// gets ErrNoControl rather than a switch performed behind another holder's back.
func TestSetPermissionMode_RequiresControlToken(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{Harness: chatClaudeCode}, claudeRing4, 3)

	mode, err := conv.SetPermissionMode(testCtx(t), "plan")
	if !errors.Is(err, ErrNoControl) {
		t.Fatalf("SetPermissionMode = (%q, %v), want ErrNoControl", mode, err)
	}
	if st, _ := fake.counts(); st != 0 {
		t.Errorf("wrote %d Shift+Tab presses without the token, want 0", st)
	}
}

// The natural caller sequence — hold the token across the WHOLE
// blocked → Answer → retry recovery — must not deadlock. It only works because
// SetPermissionMode never calls AcquireControl itself: controlQueue is
// non-reentrant, so a self-acquiring driver would block here until ctx expired.
func TestSetPermissionMode_BlockedThenAnswerThenRetry_NoDeadlock(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{Harness: chatClaudeCode}, claudeRing5, 3) // start: auto

	var req *turns.InputRequest
	// The first press lands on "bypass" and raises the acceptance dialog.
	fake.onPress = func(f *permModeFake) bool {
		if f.mode() != "bypass" || req != nil {
			return true
		}
		paint(f.sc, bypassAcceptScreen())
		r, ok := claudecode.DetectInput(f.sc.Snapshot().Text)
		if !ok {
			t.Errorf("claudecode.DetectInput did not classify the bypass acceptance screen")
			return false
		}
		req = r
		f.conv.handleInputRequested(r)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)

		mode, err := conv.SetPermissionMode(ctx, "bypass")
		var blocked *PermissionModeBlockedError
		if !errors.As(err, &blocked) {
			t.Errorf("SetPermissionMode = (%q, %v), want *PermissionModeBlockedError", mode, err)
			return
		}
		if !errors.Is(err, ErrPermissionModeBlockedByInput) {
			t.Errorf("errors.Is(err, ErrPermissionModeBlockedByInput) = false for %v", err)
		}

		// The carried request is the CLIENT-facing shape, identical to what
		// PendingInput reports — which is what makes Answer able to consume it.
		pending := conv.PendingInput()
		if pending == nil {
			t.Errorf("PendingInput() = nil while blocked")
			return
		}
		if blocked.Request.ID != pending.ID {
			t.Errorf("blocked.Request.ID = %q, want %q (PendingInput)", blocked.Request.ID, pending.ID)
		}
		if len(blocked.Request.Options) != len(pending.Options) {
			t.Errorf("blocked.Request has %d options, PendingInput has %d",
				len(blocked.Request.Options), len(pending.Options))
			return
		}
		for i := range pending.Options {
			if blocked.Request.Options[i] != pending.Options[i] {
				t.Errorf("Options[%d] = %+v, want %+v", i, blocked.Request.Options[i], pending.Options[i])
			}
		}

		// Resolve it the documented way — still holding the token — then retry.
		if err := conv.Answer(ctx, blocked.Request.ID, InputAnswer{OptionID: "1"}); err != nil {
			t.Errorf("Answer: %v", err)
			return
		}
		conv.handleInputResolved(req)

		mode, err = conv.SetPermissionMode(ctx, "bypass")
		if err != nil {
			t.Errorf("retry SetPermissionMode: (%q, %v), want success", mode, err)
		}
		if mode != "bypass" {
			t.Errorf("retry final mode = %q, want bypass", mode)
		}
	}()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("SetPermissionMode / Answer / retry deadlocked — the driver must not acquire the control token itself")
	}
}

// --- blocking input mid-cycle -------------------------------------------

// The security-relevant case: the bypass acceptance dialog appears mid-cycle.
// The driver must stop pressing IMMEDIATELY — never Shift+Tab into an open
// modal — and hand the caller the request it needs to answer.
func TestSetPermissionMode_StopsOnBypassAcceptanceDialog(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{Harness: chatClaudeCode}, claudeRing5, 0) // start: plan
	withControl(t, conv)

	fake.onPress = func(f *permModeFake) bool {
		paint(f.sc, bypassAcceptScreen())
		r, ok := claudecode.DetectInput(f.sc.Snapshot().Text)
		if !ok {
			t.Errorf("DetectInput did not classify the bypass acceptance screen")
			return false
		}
		f.conv.handleInputRequested(r)
		return false
	}

	mode, err := conv.SetPermissionMode(testCtx(t), "auto")

	var blocked *PermissionModeBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("SetPermissionMode = (%q, %v), want *PermissionModeBlockedError", mode, err)
	}
	if blocked.Request.Kind == "" {
		t.Errorf("blocked.Request.Kind is empty; want the classified dialog kind")
	}
	if st, _ := fake.counts(); st != 1 {
		t.Errorf("wrote %d Shift+Tab presses, want exactly 1 — the driver pressed into an open modal", st)
	}
	// Restoration is NOT attempted through a modal: the ring is unreachable
	// there, so the error carries the last observed posture instead.
	if mode != blocked.Observed {
		t.Errorf("returned mode %q != blocked.Observed %q", mode, blocked.Observed)
	}
}

// With an InputPolicy that resolves the dialog, the same cycle completes: the
// policy's keystrokes clear the modal and the driver keeps going.
func TestSetPermissionMode_InputPolicyResolvesDialog(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{
		Harness: chatClaudeCode,
		// claudecode classifies the bypass-acceptance screen under the same
		// "trust_prompt" kind as the folder-trust dialog (claudecode.go:205).
		InputPolicy: &InputPolicy{ByKind: map[string]Disposition{
			"trust_prompt": {Kind: DispositionAnswer, OptionID: "1"},
		}},
	}, claudeRing5, 3) // start: auto
	withControl(t, conv)

	raised := 0
	fake.onPress = func(f *permModeFake) bool {
		if f.mode() != "bypass" || raised > 0 {
			return true
		}
		raised++
		paint(f.sc, bypassAcceptScreen())
		r, ok := claudecode.DetectInput(f.sc.Snapshot().Text)
		if !ok {
			t.Errorf("DetectInput did not classify the bypass acceptance screen")
			return false
		}
		// The policy answers inside handleInputRequested; the fake's default
		// write arm repaints the (now bypass) composer, then we clear the
		// pending request exactly as the adapter's InputResolved event would.
		f.conv.handleInputRequested(r)
		f.conv.handleInputResolved(r)
		return false
	}

	mode, err := conv.SetPermissionMode(testCtx(t), "bypass")
	if err != nil {
		t.Fatalf("SetPermissionMode = (%q, %v), want success once the policy resolves the dialog", mode, err)
	}
	if mode != "bypass" {
		t.Errorf("final mode = %q, want bypass", mode)
	}
	if raised != 1 {
		t.Errorf("acceptance dialog raised %d times, want 1", raised)
	}
}

// --- bound exhaustion ----------------------------------------------------

// A harness that ignores every press: the driver must give up at the bound,
// restore the starting posture, and say so with ErrPermissionModeSwitchFailed.
func TestSetPermissionMode_BoundExhausted_RestoresStart(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{
		Harness:        chatClaudeCode,
		PermissionMode: "auto",
	}, claudeRing4, 3) // start: auto, and the fake never advances
	fake.advance = func(idx int) int { return idx }
	withControl(t, conv)

	mode, err := conv.SetPermissionMode(testCtx(t), "plan")
	if !errors.Is(err, ErrPermissionModeSwitchFailed) {
		t.Fatalf("SetPermissionMode = (%q, %v), want ErrPermissionModeSwitchFailed", mode, err)
	}
	if mode != "auto" {
		t.Errorf("final mode = %q, want the starting posture auto", mode)
	}
	if live, _ := conv.PermissionMode(); live != "auto" {
		t.Errorf("session left at %q, want the starting posture auto", live)
	}
	if st, _ := fake.counts(); st != 2*len(claudeRing4) {
		t.Errorf("pressed %d times, want the 2×ringLen bound (%d)", st, 2*len(claudeRing4))
	}
}

// A harness that ratchets UPWARD and will not come back down: the switch fails,
// restoration fails, and the session is left strictly more permissive than it
// started. That is the one outcome that must never be reported as a plain
// failure — it is ErrPermissionModeIndeterminate.
func TestSetPermissionMode_RatchetUp_Indeterminate(t *testing.T) {
	// ring: plan(0) → auto(1) → bypass(2) → bypass … ; "manual" is never shown.
	ring := []string{"plan", "auto", "bypass"}
	conv, fake := newPermModeConv(t, Options{
		Harness:        chatClaudeCode,
		PermissionMode: "bypass", // bypass-enabled launch: 5-ring bound
	}, ring, 0)
	fake.advance = func(idx int) int {
		if idx+1 >= len(ring) {
			return len(ring) - 1 // stuck at the top
		}
		return idx + 1
	}
	withControl(t, conv)

	mode, err := conv.SetPermissionMode(testCtx(t), "manual")
	if !errors.Is(err, ErrPermissionModeIndeterminate) {
		t.Fatalf("SetPermissionMode = (%q, %v), want ErrPermissionModeIndeterminate", mode, err)
	}
	if mode != "bypass" {
		t.Errorf("final mode = %q, want the last observed posture bypass", mode)
	}
	if !wrapper.MorePermissive(mode, "plan") {
		t.Errorf("wrapper.MorePermissive(%q, plan) = false; the scenario no longer models a silent escalation", mode)
	}
}

// --- ring length ---------------------------------------------------------

// The ring is 5 long — and bypass is on it — whenever the session was launched
// bypass-enabled, resolved through wrapper.EffectiveLaunchRung /
// BypassEnablingFlags rather than by re-parsing argv here. Options.PermissionMode
// alone is NOT enough: argsWithHarnessPermissionMode suppresses injection when
// argv already carries the flag, so the argv-carried cases below have
// Options.PermissionMode == "" and a bypass-enabled session. Returning
// ErrPermissionModeUnreachable for them is the exact regression this path exists
// to prevent.
func TestSetPermissionMode_RingLengthTable(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		args         []string
		wantRing     int
		wantBypassOK bool
	}{
		{"options-permission-mode-bypass", "bypass", nil, 5, true},
		{"argv-separated", "", []string{"--permission-mode", "bypassPermissions"}, 5, true},
		{"argv-joined", "", []string{"--permission-mode=bypassPermissions"}, 5, true},
		{"argv-skip-permissions-flag", "", []string{wrapper.SkipPermissionsFlag}, 5, true},
		{"argv-trailing-flag-unknown", "", []string{"--permission-mode"}, 5, true},
		{"plain-non-bypass-launch", "plan", nil, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conv, fake := newPermModeConv(t, Options{
				Harness:        chatClaudeCode,
				PermissionMode: tc.mode,
				Args:           tc.args,
			}, claudeRing5, 0)
			withControl(t, conv)

			ringLen, bypassOK := conv.cycleRing()
			if ringLen != tc.wantRing || bypassOK != tc.wantBypassOK {
				t.Errorf("cycleRing() = (%d, %v), want (%d, %v)", ringLen, bypassOK, tc.wantRing, tc.wantBypassOK)
			}

			mode, err := conv.SetPermissionMode(testCtx(t), "bypass")
			if tc.wantBypassOK {
				if errors.Is(err, ErrPermissionModeUnreachable) {
					t.Fatalf("SetPermissionMode(bypass) = (%q, %v); a bypass-enabled launch must not be rejected as unreachable", mode, err)
				}
				if err != nil {
					t.Fatalf("SetPermissionMode(bypass) = (%q, %v), want success", mode, err)
				}
				if mode != "bypass" {
					t.Errorf("final mode = %q, want bypass", mode)
				}
				return
			}
			if !errors.Is(err, ErrPermissionModeUnreachable) {
				t.Fatalf("SetPermissionMode(bypass) = (%q, %v), want ErrPermissionModeUnreachable", mode, err)
			}
			if st, _ := fake.counts(); st != 0 {
				t.Errorf("wrote %d keystrokes before the fast-fail, want 0", st)
			}
		})
	}
}

// --- target gates --------------------------------------------------------

// The launch-only spellings and the cross-axis rungs all funnel through the one
// unreachable path; harnesses with no cycle at all are unsupported.
func TestSetPermissionMode_TargetGates(t *testing.T) {
	for _, tc := range []struct {
		harness string
		target  string
		want    error
	}{
		// claude: "dontAsk" is a launch-only --permission-mode value with no
		// canonical rung, so it is not on the ring.
		{chatClaudeCode, "dontAsk", ErrPermissionModeUnreachable},
		{chatClaudeCode, "", ErrPermissionModeUnreachable},
		{chatClaudeCode, "acceptEdits", ErrPermissionModeUnreachable},
		// codex: the permissions/sandbox axis is a LAUNCH knob with no in-TUI
		// cycle, and every canonical rung other than "plan" is off-axis.
		{"codex", "read-only", ErrPermissionModeUnreachable},
		{"codex", "workspace-write", ErrPermissionModeUnreachable},
		{"codex", "danger-full-access", ErrPermissionModeUnreachable},
		{"codex", "manual", ErrPermissionModeUnreachable},
		{"codex", "ask", ErrPermissionModeUnreachable},
		{"codex", "auto", ErrPermissionModeUnreachable},
		{"codex", "bypass", ErrPermissionModeUnreachable},
		// harnesses with no permission-mode cycle at all.
		{"opencode", "plan", ErrPermissionModeUnsupported},
		{"pi", "plan", ErrPermissionModeUnsupported},
		{"generic", "plan", ErrPermissionModeUnsupported},
	} {
		t.Run(tc.harness+"/"+tc.target, func(t *testing.T) {
			conv, fake := newPermModeConv(t, Options{Harness: tc.harness}, nil, 0)
			withControl(t, conv)

			mode, err := conv.SetPermissionMode(testCtx(t), tc.target)
			if !errors.Is(err, tc.want) {
				t.Fatalf("SetPermissionMode(%q) = (%q, %v), want %v", tc.target, mode, err, tc.want)
			}
			if st, plans := fake.counts(); st != 0 || plans != 0 {
				t.Errorf("wrote %d Shift+Tab / %d /plan before the gate, want 0/0", st, plans)
			}
		})
	}
}

// permissionModeCapabilities is the driver's only harness switch; freeze the
// target sets it hands out.
func TestPermissionModeCapabilities(t *testing.T) {
	for _, tc := range []struct {
		harness string
		want    []string
		wantOK  bool
	}{
		{"claude", wrapper.PermissionRungs(), true},
		{"claude-code", wrapper.PermissionRungs(), true},
		{"  Claude-Code  ", wrapper.PermissionRungs(), true},
		{"codex", []string{"plan", "default"}, true},
		{"opencode", nil, false},
		{"pi", nil, false},
		{"generic", nil, false},
		{"", nil, false},
	} {
		got, ok := permissionModeCapabilities(tc.harness)
		if ok != tc.wantOK || strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("permissionModeCapabilities(%q) = (%v, %v), want (%v, %v)", tc.harness, got, ok, tc.want, tc.wantOK)
		}
	}
}

// --- round trip ----------------------------------------------------------

// From ANY starting posture to EVERY reachable target, over both claude ring
// lengths and codex's 2-cycle: the switch lands and PermissionMode agrees.
func TestSetPermissionMode_RoundTrip(t *testing.T) {
	for _, rc := range []struct {
		name    string
		harness string
		ring    []string
		opts    Options
		targets []string
	}{
		{"claude-4-ring", chatClaudeCode, claudeRing4, Options{Harness: chatClaudeCode, PermissionMode: "plan"}, claudeRing4},
		{"claude-5-ring", chatClaudeCode, claudeRing5, Options{Harness: chatClaudeCode, PermissionMode: "bypass"}, claudeRing5},
		{"codex-2-cycle", "codex", []string{codexCollabDefault, codexCollabPlan}, Options{Harness: "codex"}, []string{codexCollabDefault, codexCollabPlan}},
	} {
		for start := range rc.ring {
			for _, target := range rc.targets {
				t.Run(rc.name+"/"+rc.ring[start]+"→"+target, func(t *testing.T) {
					conv, _ := newPermModeConv(t, rc.opts, rc.ring, start)
					withControl(t, conv)

					mode, err := conv.SetPermissionMode(testCtx(t), target)
					if err != nil {
						t.Fatalf("SetPermissionMode(%q) from %q = (%q, %v)", target, rc.ring[start], mode, err)
					}
					if mode != target {
						t.Errorf("returned mode = %q, want %q", mode, target)
					}
					if live, ok := conv.PermissionMode(); !ok || live != target {
						t.Errorf("PermissionMode() = (%q, %v) after the switch, want (%q, true)", live, ok, target)
					}
				})
			}
		}
	}
}

// --- codex /plan ---------------------------------------------------------

// codex refuses "/plan" while a task is in progress — MCP-server boot counts,
// with the composer already painted. The driver retries, and the banner must
// NOT deadlock readyForInput: classifying it in codex.DetectInput would make
// every screen carrying it read as not-ready.
func TestSetPermissionMode_CodexPlanRefusalThenClears(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{Harness: "codex"},
		[]string{codexCollabDefault, codexCollabPlan}, 0)
	withControl(t, conv)

	const refusals = 2
	seen := 0
	fake.onPlan = func(f *permModeFake) bool {
		seen++
		if seen <= refusals {
			paint(f.sc, codexPlanRefusalScreen())
			// The refusal screen must stay READY — otherwise the send path
			// would deadlock behind a banner that never clears on its own.
			if !readyForInput("codex", f.sc.Snapshot().Text) {
				t.Errorf("codex refusal banner made readyForInput false; that would deadlock the send path")
			}
			return false
		}
		f.mu.Lock()
		f.idx = 1 // plan
		f.mu.Unlock()
		return true
	}

	mode, err := conv.SetPermissionMode(testCtx(t), codexCollabPlan)
	if err != nil {
		t.Fatalf("SetPermissionMode(plan) = (%q, %v), want success after the refusals clear", mode, err)
	}
	if mode != codexCollabPlan {
		t.Errorf("final mode = %q, want plan", mode)
	}
	if _, plans := fake.counts(); plans != refusals+1 {
		t.Errorf("submitted /plan %d times, want %d (retry per refusal)", plans, refusals+1)
	}
}

// A refusal that never clears exhausts the retry budget and surfaces as
// ErrCodexPlanRefusedBusy — distinct from a plain switch failure, because the
// remedy is "wait for the task to finish", not "try a different mode".
func TestSetPermissionMode_CodexPlanRefusedBusy(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{Harness: "codex"},
		[]string{codexCollabDefault, codexCollabPlan}, 0)
	withControl(t, conv)

	fake.onPlan = func(f *permModeFake) bool {
		paint(f.sc, codexPlanRefusalScreen())
		return false
	}

	// Shrink the retry budget by bounding the context instead of the clock: the
	// budget is a package constant, so the assertion here is that a permanently
	// refusing codex never reports success and never silently switches.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	mode, err := conv.SetPermissionMode(ctx, codexCollabPlan)
	if !errors.Is(err, ErrCodexPlanRefusedBusy) {
		t.Fatalf("SetPermissionMode(plan) = (%q, %v), want ErrCodexPlanRefusedBusy", mode, err)
	}
	if mode == codexCollabPlan {
		t.Errorf("final mode = plan although codex refused every /plan")
	}
	if _, plans := fake.counts(); plans < 2 {
		t.Errorf("submitted /plan %d times, want at least 2 retries under the budget", plans)
	}
}

// Leaving plan mode goes back through the shared Shift+Tab cycle, not /plan.
func TestSetPermissionMode_CodexLeavePlanCycles(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{Harness: "codex"},
		[]string{codexCollabDefault, codexCollabPlan}, 1) // start: plan
	withControl(t, conv)

	mode, err := conv.SetPermissionMode(testCtx(t), codexCollabDefault)
	if err != nil {
		t.Fatalf("SetPermissionMode(default) = (%q, %v), want success", mode, err)
	}
	if mode != codexCollabDefault {
		t.Errorf("final mode = %q, want default", mode)
	}
	st, plans := fake.counts()
	if st != 1 {
		t.Errorf("wrote %d Shift+Tab presses, want 1", st)
	}
	if plans != 0 {
		t.Errorf("submitted /plan %d times while LEAVING plan mode, want 0", plans)
	}
}

// --- turn in flight ------------------------------------------------------

// The passive read works mid-turn (claude keeps the footer painted while it
// works — see claudecode/busy_test.go:16) but the SWITCH fast-fails, exactly as
// Send does.
func TestSetPermissionMode_TurnInFlight(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{Harness: chatClaudeCode}, claudeRing4, 3)
	withControl(t, conv)

	// A busy claude frame: spinner + "esc to interrupt" footer, with the mode
	// marker still painted alongside.
	paint(conv.screen, []string{
		"Claude Code",
		"",
		"✶ Cerebrating… (3s · ↓ 1.2k tokens)",
		"",
		"❯ ",
		"  ⏵⏵ esc to interrupt",
		claudeFooters["auto"],
	})

	if mode, ok := conv.PermissionMode(); !ok || mode != "auto" {
		t.Errorf("PermissionMode() mid-turn = (%q, %v), want (auto, true) — the footer is painted during a turn", mode, ok)
	}

	conv.mu.Lock()
	conv.currentTurn = &Turn{ID: "t1", State: TurnStatePending}
	conv.mu.Unlock()

	mode, err := conv.SetPermissionMode(testCtx(t), "plan")
	if !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("SetPermissionMode = (%q, %v), want ErrTurnInFlight", mode, err)
	}
	if st, _ := fake.counts(); st != 0 {
		t.Errorf("wrote %d keystrokes during an in-flight turn, want 0", st)
	}
}

// The codex variant documents (and pins) the WEAKER gate: c.currentTurn is
// cleared by maybeIdleComplete, which consults turns.BusyDetector — implemented
// by claudecode and pi ONLY. codex therefore has no busy signal at all, so the
// check still fires when a turn IS registered, but harness-internal work (MCP
// boot) is invisible to it. That is why /plan-refusal detection, not
// ErrTurnInFlight, is the load-bearing guard on codex.
func TestSetPermissionMode_CodexTurnInFlightIsCourtesyOnly(t *testing.T) {
	conv, fake := newPermModeConv(t, Options{Harness: "codex"},
		[]string{codexCollabDefault, codexCollabPlan}, 0)
	withControl(t, conv)

	if _, ok := conv.adapter.(turns.BusyDetector); ok {
		t.Fatal("codex now implements turns.BusyDetector; the weaker-gate rationale in " +
			"SetPermissionMode's doc comment must be revisited")
	}

	conv.mu.Lock()
	conv.currentTurn = &Turn{ID: "t1", State: TurnStatePending}
	conv.mu.Unlock()

	mode, err := conv.SetPermissionMode(testCtx(t), codexCollabPlan)
	if !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("SetPermissionMode = (%q, %v), want ErrTurnInFlight for a REGISTERED turn", mode, err)
	}

	// …but with no turn registered — the state codex sits in while its MCP
	// servers boot, because nothing clears or sets currentTurn from the screen
	// — the gate lets the switch straight through.
	conv.mu.Lock()
	conv.currentTurn = nil
	conv.mu.Unlock()
	if _, err := conv.SetPermissionMode(testCtx(t), codexCollabPlan); err != nil {
		t.Fatalf("SetPermissionMode with no registered turn = %v, want success", err)
	}
	if _, plans := fake.counts(); plans == 0 {
		t.Error("no /plan submitted; the courtesy gate should not have blocked this")
	}
}

// --- readiness split -----------------------------------------------------

// The soft logged-out screens PASS readiness — readyForInput wins first, "a real
// composer (even with a stale banner scrolled above) is never auth-gated"
// (ready.go:76-79) — so the switch proceeds on them. Only the real onboarding
// WALL (the theme picker) yields ErrAuthRequired.
func TestSetPermissionMode_ReadinessSplit(t *testing.T) {
	for _, tc := range []struct {
		corpus   string
		wantMode string
		wantOK   bool
		wantErr  error
	}{
		{"not-logged-in-churned", "manual", true, nil},
		{"not-logged-in-brewed", "manual", true, nil},
		{"theme-picker", "", false, ErrAuthRequired},
	} {
		t.Run(tc.corpus, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(authCorpusRoot, "claude-code", tc.corpus, "screen.txt"))
			if err != nil {
				t.Fatalf("read corpus screen: %v", err)
			}
			conv, fake := newPermModeConv(t, Options{Harness: chatClaudeCode}, claudeRing4, 0)
			paint(conv.screen, strings.Split(strings.TrimRight(string(raw), "\n"), "\n"))
			withControl(t, conv)

			mode, ok := conv.PermissionMode()
			if mode != tc.wantMode || ok != tc.wantOK {
				t.Errorf("PermissionMode() = (%q, %v), want (%q, %v)", mode, ok, tc.wantMode, tc.wantOK)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = conv.SetPermissionMode(ctx, "plan")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("SetPermissionMode = %v, want %v", err, tc.wantErr)
				}
				if st, _ := fake.counts(); st != 0 {
					t.Errorf("wrote %d keystrokes into an onboarding wall, want 0", st)
				}
				return
			}
			// The soft-banner screens are ready, so the driver PROCEEDS: it
			// presses (the fake then repaints its own ring) rather than
			// short-circuiting on the stale banner.
			if errors.Is(err, ErrAuthRequired) {
				t.Fatalf("SetPermissionMode returned ErrAuthRequired on a screen that passes readiness")
			}
			if st, _ := fake.counts(); st == 0 {
				t.Error("wrote no keystrokes; the switch should have proceeded past the stale banner")
			}
		})
	}
}

// --- helpers -------------------------------------------------------------

func TestArgsContainFlag(t *testing.T) {
	args := []string{"--model", "opus", "--permission-mode=bypassPermissions"}
	if !argsContainFlag(args, "--permission-mode") {
		t.Error("joined --permission-mode=… not detected")
	}
	if !argsContainFlag([]string{wrapper.SkipPermissionsFlag}, wrapper.BypassEnablingFlags(chatClaudeCode)...) {
		t.Error("skip-permissions flag not detected")
	}
	if argsContainFlag(args, "--dangerously-skip-permissions") {
		t.Error("false positive for an absent flag")
	}
}

func TestPermModePollInterval(t *testing.T) {
	if got := permModePollInterval(10 * time.Second); got != pickerPollInterval {
		t.Errorf("permModePollInterval(10s) = %v, want %v", got, pickerPollInterval)
	}
	if got := permModePollInterval(40 * time.Millisecond); got != 10*time.Millisecond {
		t.Errorf("permModePollInterval(40ms) = %v, want 10ms", got)
	}
}
