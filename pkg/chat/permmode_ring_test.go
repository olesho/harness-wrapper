package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// PTY-driven half of the permission-mode driver's coverage. Unlike
// permmode_test.go's writeStdin seam, these spawn a REAL process over a REAL
// pty, so the Shift+Tab bytes travel the whole path: shiftTabForHarness →
// wrapper stdin → the fake's raw-mode reader, which only advances when it reads
// exactly fakeharness.ShiftTabCSI9_2u (Builder.AwaitShiftTab). If the production
// writer ever stops emitting that sequence, the fake never advances and these
// fail loudly — the same contract pin AwaitSubmit gives the send path.
//
// The ring is a TABLE: length and order are data, so the 4-ring and the
// bypass-enabled 5-ring are the same code, and a surprising live capture is a
// one-line edit rather than a new test.

// modeRingScript builds a scripted mode ring: an initial frame at ring[start],
// then `presses` alternations of "block until Shift+Tab" → "repaint the next
// ring position". It ends holding at the prompt like a real interactive harness.
//
// Steps are raw fakeharness.Step values rather than Builder calls because the
// Builder's semantic frames model TURNS (busy/marker/reply), and what this needs
// is a settled composer whose only moving part is the footer.
func modeRingScript(harness string, ring []string, start, presses int) fakeharness.Script {
	steps := []fakeharness.Step{
		{Frame: &fakeharness.Frame{Screen: ringFrame(harness, ring[start])}},
	}
	for k := 1; k <= presses; k++ {
		steps = append(
			steps,
			fakeharness.Step{WaitInput: &fakeharness.WaitInput{
				UntilRegex: shiftTabRegex(),
				Label:      "shift-tab",
			}},
			fakeharness.Step{Frame: &fakeharness.Frame{Screen: ringFrame(harness, ring[(start+k)%len(ring)])}},
		)
	}
	steps = append(steps, fakeharness.Step{Hold: &fakeharness.Hold{}})
	return fakeharness.Script{Harness: harness, Steps: steps}
}

// shiftTabRegex is the pattern Builder.AwaitShiftTab installs. It is produced by
// the Builder itself so the two cannot drift: a scenario built either way waits
// for exactly the bytes the production writer emits.
func shiftTabRegex() string {
	s := fakeharness.New("claude-code").AwaitShiftTab().Build()
	return s.Steps[0].WaitInput.UntilRegex
}

// ringFrame paints a settled composer for one ring position, harness-appropriate.
func ringFrame(harness, mode string) string {
	if harness == "codex" {
		return strings.Join(codexModeScreen(mode), "\n") + "\n"
	}
	return strings.Join(claudeModeScreen(mode), "\n") + "\n"
}

// A live-ish ring, driven over a real PTY: from every starting position, reach
// every reachable target and have PermissionMode agree. Parameterized by ring
// length AND order, so both claude ring lengths and codex's 2-cycle share one
// body.
func TestSetPermissionMode_RingOverPTY(t *testing.T) {
	for _, rc := range []struct {
		name    string
		harness string
		ring    []string
		mutate  func(*Options)
	}{
		{
			name:    "claude-4-ring",
			harness: chatClaudeCode,
			ring:    []string{"plan", "manual", "ask", "auto"},
			// A definite non-bypass launch: 4-ring, bypass off the ring.
			mutate: func(o *Options) { o.PermissionMode = "plan" },
		},
		{
			name:    "claude-5-ring-bypass-enabled",
			harness: chatClaudeCode,
			ring:    []string{"plan", "manual", "ask", "auto", "bypass"},
			// argv-carried, so Options.PermissionMode stays empty — the case
			// argsWithHarnessPermissionMode's suppression rule creates.
			mutate: func(o *Options) { o.Args = []string{"--permission-mode=bypassPermissions"} },
		},
		{
			name:    "codex-2-cycle",
			harness: "codex",
			ring:    []string{codexCollabDefault, codexCollabPlan},
			mutate:  func(o *Options) {},
		},
	} {
		for start := range rc.ring {
			for _, target := range rc.ring {
				if rc.harness == "codex" && target == codexCollabPlan {
					// Entering codex plan mode goes through `/plan`, not the
					// cycle; the hermetic suite covers that path (the fake here
					// only scripts Shift+Tab).
					continue
				}
				t.Run(rc.name+"/"+rc.ring[start]+"→"+target, func(t *testing.T) {
					// A full sweep is enough for any single target; the driver's
					// own bound is 2×ringLen, and any press past the script's
					// last WaitInput would hang the fake — which is exactly how
					// an over-pressing driver would be caught.
					conv := openFake(t, modeRingScript(rc.harness, rc.ring, start, len(rc.ring)), rc.mutate)

					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()

					// Wait for the first frame so the starting posture is on
					// screen before the switch is asked for.
					waitForPosture(t, conv, rc.ring[start], 10*time.Second)

					release, err := conv.AcquireControl(ctx)
					if err != nil {
						t.Fatalf("AcquireControl: %v", err)
					}
					defer release()

					mode, err := conv.SetPermissionMode(ctx, target)
					if err != nil {
						t.Fatalf("SetPermissionMode(%q) from %q = (%q, %v)", target, rc.ring[start], mode, err)
					}
					if mode != target {
						t.Errorf("returned mode = %q, want %q", mode, target)
					}
					if live, ok := conv.PermissionMode(); !ok || live != target {
						t.Errorf("PermissionMode() = (%q, %v), want (%q, true)", live, ok, target)
					}
				})
			}
		}
	}
}

// waitForPosture blocks until the adapter reads want off the live screen.
func waitForPosture(t *testing.T, conv *Conversation, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	notify, unsubscribe := conv.screen.Subscribe()
	defer unsubscribe()
	for {
		if got, ok := conv.PermissionMode(); ok && got == want {
			return
		}
		select {
		case <-notify:
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			got, ok := conv.PermissionMode()
			t.Fatalf("timed out waiting for posture %q; screen reads (%q, %v)", want, got, ok)
		}
	}
}

// Reopen resets the posture to the LAUNCH rung: the observed mode is
// process-local and deliberately not persisted (persisting it would add a field
// to chat.Session and thereby to every Store implementation). So the "never
// silently more permissive than it started" invariant holds within ONE
// Conversation lifetime only — this pins the documented consequence.
func TestSetPermissionMode_NotPersistedAcrossReopen(t *testing.T) {
	// Launch at "auto"; one Shift+Tab lands on "plan". The resume hint rides on
	// both frames so the session id is captured for Reopen.
	ring := []string{"auto", "plan"}
	script := modeRingScript(chatClaudeCode, ring, 0, 1)
	// Splice the resume hint into every frame so the raw line tap sees it.
	for i := range script.Steps {
		if f := script.Steps[i].Frame; f != nil {
			f.Screen += "  claude --resume 11111111-2222-3333-4444-555555555555\n"
		}
	}

	store := newFakeStore()
	conv := openFake(t, script, func(o *Options) {
		o.Store = store
		o.PermissionMode = "auto"
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	waitForPosture(t, conv, "auto", 10*time.Second)

	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	mode, err := conv.SetPermissionMode(ctx, "plan")
	if err != nil {
		t.Fatalf("SetPermissionMode(plan) = (%q, %v)", mode, err)
	}
	release()

	sessionID := conv.session.ID
	rec, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if rec.HarnessSessionID == "" {
		t.Skip("fake harness session id not captured; Reopen has nothing to resume")
	}
	if err := conv.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen with PermissionMode UNSET: the harness relaunches at its own
	// default, and the mid-session switch is gone.
	reopened, err := Reopen(ctx, ReopenOptions{
		SessionID:  sessionID,
		BinaryPath: conv.opts.BinaryPath,
		Env:        conv.opts.Env,
		Store:      store,
		Cols:       120,
		Rows:       40,
		idleGap:    testIdleGap,
		markerGap:  testMarkerGap,
	})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer closeCancel()
		_ = reopened.Close(closeCtx)
	})

	waitForPosture(t, reopened, "auto", 10*time.Second)
	if mode, ok := reopened.PermissionMode(); !ok || mode != "auto" {
		t.Errorf("after Reopen PermissionMode() = (%q, %v), want the launch posture (auto, true)", mode, ok)
	}
}
