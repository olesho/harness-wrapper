//go:build unix

// Package procgroup runs a harness in a process group of its own, so that
// stopping it reaches the processes it started too — those that stay in the
// group.
//
// A process-group ID is the leader's pid, and it can be reused once the
// leader has been reaped and the group has no member left. So the group is
// signalled only while that cannot have happened: while the leader has not
// been reaped, or, after, while the group still has members (a member keeps
// the ID from being reused). This is the rule wrapcore's groupTerminator
// follows; an adapter's kill after its harness exited would otherwise signal
// whichever group had since been given the ID.
package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killFn is syscall.Kill; tests replace it to observe signals.
var killFn = syscall.Kill

// Set makes cmd start in a process group of its own. Other SysProcAttr fields
// the caller set are kept; a cmd that starts a session of its own (Setsid) is
// already a group leader and is left as it is, since setpgid would fail for a
// session leader.
func Set(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	if cmd.SysProcAttr.Setsid {
		return
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
}

// Signal sends SIGTERM, or SIGKILL when kill, to cmd's group. A cmd that was
// not started in a group of its own is signalled alone, and only until it is
// reaped. A group whose leader has been reaped is signalled only while it
// still has members; once it is empty, nothing is sent.
func Signal(cmd *exec.Cmd, kill bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	if !ownGroup(cmd) {
		_ = cmd.Process.Signal(sig) // os.ErrProcessDone once reaped
		return
	}
	if leaderReaped(cmd) && !hasMembers(cmd.Process.Pid) {
		return // the group is gone; its ID may already name another group
	}
	_ = signalGroup(cmd.Process.Pid, sig)
}

// Empty reports whether no process is left in cmd's group (for a cmd not in
// a group of its own, whether the process has ended).
func Empty(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return true
	}
	if !ownGroup(cmd) {
		return leaderReaped(cmd)
	}
	return !hasMembers(cmd.Process.Pid)
}

// ownGroup reports whether cmd was started as the leader of a new group, so
// that the group's ID is the leader's pid.
func ownGroup(cmd *exec.Cmd) bool {
	a := cmd.SysProcAttr
	return a != nil && (a.Setsid || (a.Setpgid && a.Pgid == 0)) && cmd.Process.Pid > 1
}

// leaderReaped reports whether cmd's process has ended as far as os.Process
// knows: Wait has reaped it, or it is gone. os.Process tracks this itself, so
// the check is safe to make concurrently with Wait.
func leaderReaped(cmd *exec.Cmd) bool {
	return errors.Is(cmd.Process.Signal(syscall.Signal(0)), os.ErrProcessDone)
}

// hasMembers reports whether process group pgid has any member. EPERM means
// members exist that this process may not signal.
func hasMembers(pgid int) bool {
	err := killFn(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 || pgid == syscall.Getpgrp() {
		return syscall.EINVAL
	}
	return killFn(-pgid, sig)
}

// ExitSignal names the signal that ended a process, "" when none.
func ExitSignal(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
