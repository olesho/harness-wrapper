//go:build unix

package wrapper

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// descendantsOf returns the PIDs, other than pid itself, that currently belong
// to pid's process group. It shells out to ps because that is the only portable
// way to enumerate a group across the BSD and Linux builds this package runs on.
func descendantsOf(t *testing.T, pid int) []int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "pid=,pgid=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		p, err1 := strconv.Atoi(fields[0])
		g, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || g != pid || p == pid {
			continue
		}
		pids = append(pids, p)
	}
	return pids
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitAllGone polls until every pid is gone or the deadline passes, returning
// whichever pids are still alive.
func waitAllGone(pids []int, within time.Duration) []int {
	deadline := time.Now().Add(within)
	for {
		var left []int
		for _, p := range pids {
			if alive(p) {
				left = append(left, p)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitProcExit reaps cmd and reports whether it exited within the timeout.
// It reaps rather than polling with signal 0, which a zombie answers.
func waitProcExit(cmd *exec.Cmd, within time.Duration) bool {
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(within):
		return false
	}
}

// startSleepTree launches a shell that spawns two long-lived children and
// returns the session plus the descendant PIDs observed in its process group.
//
// The children ignore SIGHUP (an ignored disposition is inherited across exec),
// which is what makes this a regression test rather than a tautology: without
// it, the kernel's SIGHUP to the controlling terminal's foreground group when
// the session leader dies reaps the children whether or not the wrapper
// signals the group. Real tool subprocesses — a `go test` run under a node
// harness — outlive that hangup too, which is the whole incident.
func startSleepTree(t *testing.T, ctx context.Context) (*Session, []int) {
	t.Helper()
	sess, err := Start(ctx, Config{
		BinaryPath: "/bin/sh",
		Args:       []string{"-c", `trap "" HUP; sleep 300 & sleep 300`},
		Stdout:     io.Discard,
		WaitDelay:  500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	pgid := sess.PID()
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	var kids []int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if kids = descendantsOf(t, sess.PID()); len(kids) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(kids) < 2 {
		t.Fatalf("expected at least 2 descendants in group %d, saw %v", sess.PID(), kids)
	}
	return sess, kids
}

// TestContextCancelReapsDescendants is the regression this file exists for:
// before termination became group-scoped, cancelling the context killed the
// harness and left every tool subprocess it had spawned running.
func TestContextCancelReapsDescendants(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sess, kids := startSleepTree(t, ctx)

	cancel()
	if _, err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if left := waitAllGone(kids, 5*time.Second); len(left) > 0 {
		t.Errorf("descendants %v survived context cancel; want the whole group reaped", left)
	}
}

// TestStopReapsDescendants covers the graceful path through terminateAndWait.
func TestStopReapsDescendants(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, kids := startSleepTree(t, ctx)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := sess.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if left := waitAllGone(kids, 5*time.Second); len(left) > 0 {
		t.Errorf("descendants %v survived Stop; want the whole group reaped", left)
	}
}

// TestGroupTerminatorRefusesDangerousGroups asserts the self-protection: a
// group that resolves to 0, 1, or the wrapper's own must never be signalled as
// a group, only the single process.
func TestGroupTerminatorRefusesDangerousGroups(t *testing.T) {
	cases := map[string]int{
		"own group":  syscall.Getpgrp(),
		"group zero": 0,
		"init group": 1,
	}
	for name, pgid := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", "-c", "sleep 300")
			if err := cmd.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = cmd.Process.Kill() }()

			var groupCalls []int
			origGetpgid, origKill := getpgidFn, killFn
			getpgidFn = func(int) (int, error) { return pgid, nil }
			killFn = func(pid int, sig syscall.Signal) error {
				if pid < 0 {
					groupCalls = append(groupCalls, pid)
				}
				return origKill(pid, sig)
			}
			defer func() { getpgidFn, killFn = origGetpgid, origKill }()

			g := &groupTerminator{cmd: cmd}
			if err := g.signal(syscall.SIGTERM); err != nil {
				t.Fatalf("signal: %v", err)
			}
			if len(groupCalls) != 0 {
				t.Fatalf("signalled groups %v; want the per-process fallback and no group signal", groupCalls)
			}
			if !waitProcExit(cmd, 5*time.Second) {
				t.Errorf("process %d survived the fallback signal", cmd.Process.Pid)
			}
		})
	}
}

// TestGroupTerminatorFallsBackWhenGetpgidFails covers the other fallback: an
// unresolvable group must still signal the process.
func TestGroupTerminatorFallsBackWhenGetpgidFails(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	origGetpgid := getpgidFn
	getpgidFn = func(int) (int, error) { return 0, syscall.ESRCH }
	defer func() { getpgidFn = origGetpgid }()

	if err := (&groupTerminator{cmd: cmd}).signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if !waitProcExit(cmd, 5*time.Second) {
		t.Errorf("process %d survived the fallback signal", cmd.Process.Pid)
	}
}

// TestGroupTerminatorSwallowsGone asserts a signal to a process that is
// already gone is not an error: it must never change a run's classification.
func TestGroupTerminatorSwallowsGone(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := (&groupTerminator{cmd: cmd}).signal(syscall.SIGTERM); err != nil {
		t.Errorf("signal on a reaped process = %v, want nil", err)
	}
}

// TestGroupTerminatorNilProcess guards the pre-start window where cmd.Process
// is still nil.
func TestGroupTerminatorNilProcess(t *testing.T) {
	if err := (&groupTerminator{cmd: &exec.Cmd{}}).signal(syscall.SIGTERM); err != nil {
		t.Errorf("signal with no process = %v, want nil", err)
	}
}

// TestGroupTerminatorNeverSignalsAnEmptiedGroup pins the recycling guard: once
// the leader is reaped, a group ID stays ours only while the group has members.
// An emptied group's ID may already name somebody else's group, so a late
// SIGKILL must not be sent to it.
func TestGroupTerminatorNeverSignalsAnEmptiedGroup(t *testing.T) {
	const fakePgid = 424242
	var sent []syscall.Signal
	origGetpgid, origKill := getpgidFn, killFn
	getpgidFn = func(pid int) (int, error) { return pid, nil }
	killFn = func(pid int, sig syscall.Signal) error {
		if pid != -fakePgid {
			return origKill(pid, sig)
		}
		if sig == 0 {
			return syscall.ESRCH // the group has emptied
		}
		sent = append(sent, sig)
		return nil
	}
	defer func() { getpgidFn, killFn = origGetpgid, origKill }()

	g := &groupTerminator{cmd: &exec.Cmd{Process: &os.Process{Pid: fakePgid}}}
	if err := g.signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM before the reap: %v", err)
	}
	g.finish(0) // the leader is reaped; termination was requested, grace is over
	if err := g.signal(syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL after the reap: %v", err)
	}
	if len(sent) != 1 || sent[0] != syscall.SIGTERM {
		t.Fatalf("group signals = %v, want only the SIGTERM sent while the leader was unreaped", sent)
	}
}
