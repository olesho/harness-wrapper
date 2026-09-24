//go:build !unix

package chat

import (
	"os"
	"os/exec"
)

func setStreamProcAttr(*exec.Cmd) {}

func signalStreamGroup(cmd *exec.Cmd, _ bool) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func streamExitSignal(*os.ProcessState) string { return "" }
