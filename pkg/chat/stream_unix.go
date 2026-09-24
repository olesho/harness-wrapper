//go:build unix

package chat

import (
	"os"
	"os/exec"
	"syscall"
)

// setStreamProcAttr starts claude in a process group of its own, so Close
// reaches the tools it runs too — those that stay in it (ADR-009).
func setStreamProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalStreamGroup sends SIGTERM, or SIGKILL when kill, to claude's group.
func signalStreamGroup(cmd *exec.Cmd, kill bool) {
	if cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// streamExitSignal names the signal that ended the process, "" when none.
func streamExitSignal(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
