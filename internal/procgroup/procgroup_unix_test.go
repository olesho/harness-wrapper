//go:build unix

package procgroup

import (
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"
)

// recordKills routes group signals through the real kill and records them.
func recordKills(t *testing.T) func() []syscall.Signal {
	t.Helper()
	var mu sync.Mutex
	var sent []syscall.Signal
	killFn = func(pid int, sig syscall.Signal) error {
		if pid < 0 && sig != 0 {
			mu.Lock()
			sent = append(sent, sig)
			mu.Unlock()
		}
		return syscall.Kill(pid, sig)
	}
	t.Cleanup(func() { killFn = syscall.Kill })
	return func() []syscall.Signal {
		mu.Lock()
		defer mu.Unlock()
		return append([]syscall.Signal(nil), sent...)
	}
}

func TestSetKeepsCallerAttributes(t *testing.T) {
	cmd := exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Noctty: true, Pgid: 7}
	Set(cmd)
	if a := cmd.SysProcAttr; !a.Noctty || !a.Setpgid || a.Pgid != 0 {
		t.Fatalf("Set replaced or kept the wrong attributes: %+v", a)
	}

	cmd = exec.Command("true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	Set(cmd)
	if a := cmd.SysProcAttr; !a.Setsid || a.Setpgid {
		t.Fatalf("a session leader must not also ask for setpgid: %+v", a)
	}

	cmd = exec.Command("true")
	Set(cmd)
	if a := cmd.SysProcAttr; a == nil || !a.Setpgid {
		t.Fatalf("Set on a bare cmd: %+v", a)
	}
}

// TestSignalAfterReapSendsNothing: once the leader is reaped and its group is
// empty, the group's ID may be reused, so a late Signal sends nothing.
func TestSignalAfterReapSendsNothing(t *testing.T) {
	sent := recordKills(t)
	cmd := exec.Command("sleep", "30")
	Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if Empty(cmd) {
		t.Fatal("Empty while the leader runs")
	}
	Signal(cmd, true)
	_ = cmd.Wait()
	if got := ExitSignal(cmd.ProcessState); got != "killed" {
		t.Fatalf("leader ended by %q, want killed", got)
	}
	if got := sent(); len(got) != 1 || got[0] != syscall.SIGKILL {
		t.Fatalf("signals before reap: %v", got)
	}
	if !Empty(cmd) {
		t.Fatal("group not empty after its only member was reaped")
	}
	Signal(cmd, false)
	Signal(cmd, true)
	if got := sent(); len(got) != 1 {
		t.Fatalf("signalled a reaped, empty group: %v", got)
	}
}

// TestSignalReachesMembersAfterLeaderReaped: a member left behind keeps the
// group (and its ID) alive, so the group is still signalled.
func TestSignalReachesMembersAfterLeaderReaped(t *testing.T) {
	sent := recordKills(t)
	cmd := exec.Command("sh", "-c", "sleep 30 </dev/null >/dev/null 2>&1 & echo started")
	Set(cmd)
	if _, err := cmd.Output(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for Empty(cmd) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if Empty(cmd) {
		t.Fatal("the background member is not in the group")
	}
	Signal(cmd, true)
	if got := sent(); len(got) != 1 || got[0] != syscall.SIGKILL {
		t.Fatalf("a group with a member left was not signalled: %v", got)
	}
}

// TestNoOwnGroupSignalsProcessOnly: a cmd started without its own group is
// never signalled as a group.
func TestNoOwnGroupSignalsProcessOnly(t *testing.T) {
	sent := recordKills(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if Empty(cmd) {
		t.Fatal("Empty while the process runs")
	}
	Signal(cmd, true)
	_ = cmd.Wait()
	Signal(cmd, true)
	if got := sent(); len(got) != 0 {
		t.Fatalf("group signals for a cmd without a group: %v", got)
	}
	if !Empty(cmd) {
		t.Fatal("not Empty after the process was reaped")
	}
}
