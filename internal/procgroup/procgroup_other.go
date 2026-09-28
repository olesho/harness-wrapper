//go:build !unix

// Package procgroup runs a harness in a process group of its own; where
// there are none, it reaches the harness's process alone.
package procgroup

import (
	"os"
	"os/exec"
)

// Set does nothing: there are no process groups.
func Set(*exec.Cmd) {}

// Signal kills cmd's process.
func Signal(cmd *exec.Cmd, _ bool) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Empty reports whether cmd's process ended.
func Empty(cmd *exec.Cmd) bool {
	return cmd == nil || cmd.ProcessState != nil
}

// ExitSignal is always "".
func ExitSignal(*os.ProcessState) string { return "" }
