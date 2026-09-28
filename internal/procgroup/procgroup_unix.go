//go:build unix

// Package procgroup runs a harness in a process group of its own, so that
// stopping it reaches the processes it started too — those that stay in the
// group.
package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Set makes cmd start in a process group of its own.
func Set(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Signal sends SIGTERM, or SIGKILL when kill, to cmd's group.
func Signal(cmd *exec.Cmd, kill bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// Empty reports whether no process is left in cmd's group.
func Empty(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return true
	}
	return errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
}

// ExitSignal names the signal that ended a process, "" when none.
func ExitSignal(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
