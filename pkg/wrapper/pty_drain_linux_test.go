package wrapper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/internal/landlock"
)

// TestLeftoverTerminalHolderCannotHoldWaitOpen: a harness that exits on its
// own may leave behind a process that ignores the hangup and keeps writing to
// the terminal, so the output never reaches its end. The supervisor gives it
// the drain budget, then closes the master — the output goroutine stops at its
// next read — and records that the drain was cut short. Waiting for the end of
// output without a budget would keep Wait open for as long as the leftover
// runs.
//
// Linux only, because only Linux keeps the terminal open for such a leftover.
// On darwin the session leader's exit revokes its controlling terminal, so the
// master read ends at once while the leftover is still running.
func TestLeftoverTerminalHolderCannotHoldWaitOpen(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "leftover.pid")
	var log traceLog
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := Start(ctx, Config{
		BinaryPath: "/bin/sh",
		// The harness ignores the hangup BEFORE forking the leftover, which
		// inherits that; a trap set inside the leftover could come too late,
		// since the hangup arrives the moment the harness exits.
		Args:   []string{"-c", `trap "" HUP; (while :; do echo tick; sleep 0.1; done) & echo $! > "$1"; exit 0`, "harness", pidFile},
		Stdout: io.Discard,
		Trace:  &log,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The leftover stays in the harness's process group, which outlives its
	// leader while the leftover runs.
	pgid := s.PID()
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		_, _ = s.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return while a leftover process held the terminal")
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read leftover pid: %v", err)
	}
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid <= 1 || !alive(pid) {
		t.Fatalf("leftover %q is not running, so it held nothing open", strings.TrimSpace(string(b)))
	}
	if got := log.fields(t, "pty_closed")["output_drained"]; got != false {
		t.Fatalf("pty_closed output_drained = %v, want false while a leftover process held the terminal", got)
	}
}

// TestContainedLeftoverTerminalHolderCannotHoldWaitOpen is the same for a
// contained session, whose master is a blocking descriptor outside the
// netpoller: closing it does not end a read already blocked on it, so the
// supervisor has to wake that read itself. The leftover here holds the
// terminal silently — output would end the blocked read on its own — and,
// without cgroup supervision, outlives the harness.
func TestContainedLeftoverTerminalHolderCannotHoldWaitOpen(t *testing.T) {
	if _, err := landlock.Probe(); err != nil {
		if v := os.Getenv("HW_LANDLOCK_REQUIRE_ABI"); v != "" && v != "0" {
			t.Fatalf("Landlock ABI 9 required: %v", err)
		}
		t.Skipf("Landlock ABI 9 unavailable: %v", err)
	}
	sh, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(contain.RegisterTestProfile(contain.TestProfile{Harness: "sh", ExecDirs: []string{filepath.Dir(sh)}}))
	t.Cleanup(contain.DisableSupervisionForTest())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	pidFile := filepath.Join(wd, "leftover.pid")
	var log traceLog
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := Start(ctx, Config{
		Harness:     "sh",
		BinaryPath:  sh,
		Args:        []string{"-c", `trap "" HUP; (while :; do sleep 0.1; done) & echo $! > "$1"; exit 0`, "harness", pidFile},
		WorkingDir:  wd,
		Env:         []string{"PATH=/usr/bin:/bin"},
		Stdout:      io.Discard,
		Trace:       &log,
		Containment: &Containment{Kind: ContainmentLandlock},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := s.PID()
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	waited := make(chan struct{})
	go func() {
		defer close(waited)
		_, _ = s.Wait()
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return while a leftover process held the contained session's terminal")
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read leftover pid: %v", err)
	}
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid <= 1 || !alive(pid) {
		t.Fatalf("leftover %q is not running, so it held nothing open", strings.TrimSpace(string(b)))
	}
	if got := log.fields(t, "pty_closed")["output_drained"]; got != false {
		t.Fatalf("pty_closed output_drained = %v, want false while a leftover process held the terminal", got)
	}
}
