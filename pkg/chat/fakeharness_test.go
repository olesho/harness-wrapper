package chat

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// This file wires the scriptable fake harness (cmd/fakeharness) into PTY-driven
// integration tests. Unlike the adapter-level corpus tests, these spawn a REAL
// process over a REAL pty via Open, so they exercise the genuine screen
// emulator, turn watcher, and idle-completion timers — the timing-sensitive
// completion path that unit tests calling maybeIdleComplete directly cannot
// reach.

// TestMain cleans up the one fake-harness binary built for the package run.
func TestMain(m *testing.M) {
	code := m.Run()
	fakeharness.Cleanup()
	os.Exit(code)
}

// buildFakeHarness compiles cmd/fakeharness once per test binary and returns its
// path. Tests skip (rather than fail) when the Go toolchain is unavailable, so
// the suite degrades gracefully in minimal environments.
func buildFakeHarness(t *testing.T) string {
	t.Helper()
	path, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}
	return path
}

// Shrunk completion windows so PTY-driven tests run in ~1s instead of ~10s.
// They are applied per-Conversation via the unexported Options overrides (not a
// global), so concurrent watcher goroutines read them race-free. The invariant
// under test (a flicker must not complete; only a settled prompt may) holds at
// any scale, as long as fixture frame delays stay below testMarkerGap.
const (
	testIdleGap   = 500 * time.Millisecond
	testMarkerGap = 120 * time.Millisecond
)

// openFake spawns the fake harness driving the given script and returns an open
// Conversation. The script is delivered via a temp file referenced by the
// FAKEHARNESS_SCRIPT env var; Env is the full environment (wrapper.Start
// replaces, not merges) so the child keeps PATH/TERM.
func openFake(t *testing.T, script fakeharness.Script) *Conversation {
	t.Helper()
	bin := buildFakeHarness(t)

	data, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(scriptPath, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	conv, err := Open(context.Background(), Options{
		Harness:    script.Harness,
		BinaryPath: bin,
		Env:        append(os.Environ(), fakeharness.EnvVar+"="+scriptPath),
		Store:      newFakeStore(),
		Cols:       120,
		Rows:       40,
		idleGap:    testIdleGap,
		markerGap:  testMarkerGap,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conv.Close(ctx)
	})
	return conv
}

// sendOneTurn acquires control, sends text, and releases control after the send
// returns (the in-flight turn continues independently).
func sendOneTurn(t *testing.T, conv *Conversation, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	defer release()
	if _, err := conv.Send(ctx, text); err != nil {
		t.Fatalf("Send(%q): %v", text, err)
	}
}

// waitForTerminalTurn drains conversation events until an assistant turn reaches
// a terminal state (complete or errored), or the timeout fires.
func waitForTerminalTurn(t *testing.T, conv *Conversation, timeout time.Duration) Turn {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-conv.Events():
			if ev.Type == EventTurn && ev.Turn.Role == RoleAssistant &&
				(ev.Turn.State == TurnStateComplete || ev.Turn.State == TurnStateErrored) {
				return ev.Turn
			}
		case <-deadline:
			t.Fatalf("timed out after %s waiting for a terminal assistant turn", timeout)
		}
	}
}
