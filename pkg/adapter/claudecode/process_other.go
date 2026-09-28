//go:build !unix

package claudecode

import (
	"os"
	"os/exec"
)

func setProcessGroup(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, _ bool) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func groupEmpty(cmd *exec.Cmd) bool {
	return cmd == nil || cmd.ProcessState != nil
}

func exitSignal(*os.ProcessState) string { return "" }
