//go:build unix

package claudecode

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup starts claude in a process group of its own, so Stop
// reaches the tools it runs too — those that stay in it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends SIGTERM, or SIGKILL when kill, to claude's group.
func signalGroup(cmd *exec.Cmd, kill bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// groupEmpty reports whether no process is left in claude's group.
func groupEmpty(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return true
	}
	return errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
}

// exitSignal names the signal that ended the process, "" when none.
func exitSignal(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
