//go:build unix

package wrapper

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// executing reports whether pid is a process that is still RUNNING. A zombie —
// exited, waiting for its parent to reap it, holding no CPU — does not count.
// Signal 0 cannot tell the two apart (a zombie answers it), so this asks ps for
// the state letter; ps exits non-zero when the pid is gone altogether.
func executing(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	st := strings.TrimSpace(string(out))
	return st != "" && !strings.HasPrefix(st, "Z")
}

// groupMembersExecuting returns the executing members of process group pgid.
func groupMembersExecuting(t *testing.T, pgid int) []int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,stat=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		p, err1 := strconv.Atoi(f[0])
		g, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || g != pgid || strings.HasPrefix(f[2], "Z") {
			continue
		}
		pids = append(pids, p)
	}
	return pids
}

// escalationScript is a harness leader that spawns a child ignoring SIGTERM and
// SIGHUP, waits for it, and — unless leaderIgnoresTerm — exits on SIGTERM
// itself. The child writes its pid to "$1" only AFTER installing its handlers,
// so a test that has read the pid knows the child will survive a SIGTERM.
func escalationScript(leaderIgnoresTerm bool) string {
	leaderTrap := `trap 'exit 0' TERM`
	if leaderIgnoresTerm {
		leaderTrap = `trap '' TERM`
	}
	return leaderTrap + `; /bin/sh -c 'trap "" TERM HUP; echo $$ > "$1"; echo child-ready; exec sleep 300' child "$1" & wait; wait`
}

// startEscalationFixture starts escalationScript and returns the session and
// the acknowledged child's pid. The whole group is SIGKILLed at cleanup, saved
// while the leader is alive, so a regression under test cannot strand the
// fixture's own descendants.
func startEscalationFixture(t *testing.T, ctx context.Context, cfg Config, leaderIgnoresTerm bool) (*Session, int) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cfg.BinaryPath = "/bin/sh"
	cfg.Args = []string{"-c", escalationScript(leaderIgnoresTerm), "parent", ready}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	s, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := s.PID()
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(ready)
		if child, _ := strconv.Atoi(strings.TrimSpace(string(b))); child > 1 {
			return s, child
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child never acknowledged its signal handlers")
	return nil, 0
}

// TestEscalatesAfterLeaderExit is the reproduction from the plan review: the
// leader exits on SIGTERM, the child ignores TERM and HUP. Both termination
// paths must still SIGKILL the child once the grace period has passed — the
// escalation used to be disarmed by the leader's reap (cancel) or never sent
// at all once the leader had gone (Stop).
func TestEscalatesAfterLeaderExit(t *testing.T) {
	for _, mode := range []string{"cancel", "stop"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := filepath.Join(t.TempDir(), "ready")
			s, err := Start(ctx, Config{BinaryPath: "/bin/sh", Args: []string{"-c", `trap 'exit 0' TERM; /bin/sh -c 'trap "" TERM HUP; echo $$ > "$1"; exec sleep 300' child "$1" & wait`, "parent", ready}, Stdout: io.Discard, WaitDelay: 100 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			// Save the group while its leader is alive; clean up our own fixture
			// even when the regression under review strands its descendant.
			pgid := s.PID()
			t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
			var child int
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				b, _ := os.ReadFile(ready)
				child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
				if child > 1 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if child <= 1 {
				t.Fatal("child never acknowledged signal handlers")
			}
			if mode == "cancel" {
				cancel()
			} else {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer stopCancel()
				if err := s.Stop(stopCtx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Wait(); err != nil {
				t.Fatal(err)
			}
			if left := waitAllGone([]int{child}, time.Second); len(left) != 0 {
				t.Fatalf("TERM/HUP-ignoring child %d survived leader exit and grace period", child)
			}
		})
	}
}

// TestWaitReturnsAfterDescendantCleanup pins what Wait promises for a
// requested termination: it returns only once the grace period has run its
// course and the TERM-ignoring child has been KILLed — not the moment the
// leader is reaped. So the child is already not executing when Wait returns,
// and Wait took at least the grace period (TERM first, KILL after WaitDelay,
// not an immediate KILL).
func TestWaitReturnsAfterDescendantCleanup(t *testing.T) {
	const grace = 400 * time.Millisecond
	for _, mode := range []string{"cancel", "stop"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, child := startEscalationFixture(t, ctx, Config{WaitDelay: grace}, false)

			start := time.Now()
			if mode == "cancel" {
				cancel()
			} else {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer stopCancel()
				if err := s.Stop(stopCtx); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			}
			if _, err := s.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			elapsed := time.Since(start)
			if executing(child) {
				t.Fatalf("child %d still executing when Wait returned after %v; Wait must cover descendant cleanup", child, elapsed)
			}
			if elapsed < grace-50*time.Millisecond {
				t.Errorf("Wait returned after %v, before the %v grace period; the child must get SIGTERM's grace before SIGKILL", elapsed, grace)
			}
		})
	}
}

// TestEscalatesWhenLeaderIgnoresTerm is the leader-stays-alive control: the
// leader ignores SIGTERM as well, so escalation happens while it is still
// running. Both the leader and the child must be gone after the grace period.
func TestEscalatesWhenLeaderIgnoresTerm(t *testing.T) {
	for _, mode := range []string{"cancel", "stop"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s, child := startEscalationFixture(t, ctx, Config{WaitDelay: 200 * time.Millisecond}, true)
			leader := s.PID()

			if mode == "cancel" {
				cancel()
			} else {
				stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer stopCancel()
				if err := s.Stop(stopCtx); err != nil {
					t.Fatalf("Stop: %v", err)
				}
			}
			if _, err := s.Wait(); err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if left := waitNotExecuting([]int{leader, child}, 2*time.Second); len(left) != 0 {
				t.Fatalf("processes %v still executing after a TERM-ignoring leader's grace period", left)
			}
		})
	}
}

// TestClassifierTerminationReapsDescendants covers the third termination
// trigger: a terminal classification makes the supervisor terminate the run
// itself (terminateAndWait), with no Stop call and no cancellation.
func TestClassifierTerminationReapsDescendants(t *testing.T) {
	classifier := ClassifierFunc(func(in ClassifierInput) Classification {
		if strings.Contains(in.RecentOutput, "child-ready") {
			return Classification{Status: StatusBlockedByCost, Class: ErrBilling, Reason: "test: terminal", Terminal: true}
		}
		return Classification{}
	})
	s, child := startEscalationFixture(t, context.Background(), Config{
		WaitDelay:    300 * time.Millisecond,
		IdleQuiet:    100 * time.Millisecond,
		IdleClassify: 200 * time.Millisecond,
		Classifier:   classifier,
	}, false)

	res, err := s.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Status != StatusBlockedByCost {
		t.Fatalf("Status = %q, want %q (the terminal classification)", res.Status, StatusBlockedByCost)
	}
	if left := waitNotExecuting([]int{child}, time.Second); len(left) != 0 {
		t.Fatalf("TERM/HUP-ignoring child %d survived a classifier-driven termination", child)
	}
}

// TestCancelAroundStartupLeavesNoDescendants cancels at varying points while
// the harness is still starting: before, while, or after the child installs its
// handlers. Whatever the interleaving, Wait must return and leave nothing of
// the group executing.
func TestCancelAroundStartupLeavesNoDescendants(t *testing.T) {
	for i, delay := range []time.Duration{0, time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 60 * time.Millisecond} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := filepath.Join(t.TempDir(), "ready")
			s, err := Start(ctx, Config{
				BinaryPath: "/bin/sh",
				Args:       []string{"-c", escalationScript(false), "parent", ready},
				Stdout:     io.Discard,
				WaitDelay:  150 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			pgid := s.PID()
			t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
			time.Sleep(delay)
			cancel()

			done := make(chan struct{})
			go func() { _, _ = s.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("Wait did not return after cancellation during startup")
			}
			if left := groupMembersExecuting(t, pgid); len(left) != 0 {
				t.Fatalf("group %d members %v still executing after Wait (cancel %v after Start)", pgid, left, delay)
			}
		})
	}
}

// TestStartWithCancelledContextStartsNothing: a context that is already done
// never launches the harness, so there is nothing to terminate or leak.
func TestStartWithCancelledContextStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := Start(ctx, Config{BinaryPath: "/bin/sh", Args: []string{"-c", "sleep 300"}, Stdout: io.Discard})
	if err == nil {
		_ = s.Stop(context.Background())
		t.Fatalf("Start with a cancelled context launched pid %d", s.PID())
	}
}

// waitNotExecuting polls until none of pids is executing, returning those that
// still are at the deadline.
func waitNotExecuting(pids []int, within time.Duration) []int {
	deadline := time.Now().Add(within)
	for {
		var left []int
		for _, p := range pids {
			if executing(p) {
				left = append(left, p)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(20 * time.Millisecond)
	}
}
