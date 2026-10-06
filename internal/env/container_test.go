package env

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeRuntime writes a stand-in for docker/podman that echoes its argv on one
// line, then copies its stdin to stdout.
func fakeRuntime(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake runtime")
	}
	path := filepath.Join(t.TempDir(), "fake-docker")
	script := "#!/bin/sh\necho \"ARGS: $*\"\ncat\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Exec must forward opts.Stdin and ask the runtime to keep stdin open (-i);
// without it a stdin-fed command inside the container reads nothing.
func TestContainerExecForwardsStdin(t *testing.T) {
	ws := NewContainerWorkspace(fakeRuntime(t), "cid", t.TempDir())
	in := "policy: body\n"
	res, err := ws.Exec(context.Background(), []string{"sh", "-c", "cat"}, &ExecOpts{Stdin: &in})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	first, rest, _ := strings.Cut(res.Stdout, "\n")
	if !strings.Contains(" "+first+" ", " -i ") {
		t.Errorf("runtime argv %q lacks -i", first)
	}
	if rest != in {
		t.Errorf("stdin reached runtime as %q, want %q", rest, in)
	}
}

// Without Stdin the exec stays non-interactive.
func TestContainerExecNoStdinNoInteractive(t *testing.T) {
	ws := NewContainerWorkspace(fakeRuntime(t), "cid", t.TempDir())
	res, err := ws.Exec(context.Background(), []string{"true"}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	first, _, _ := strings.Cut(res.Stdout, "\n")
	if strings.Contains(" "+first+" ", " -i ") {
		t.Errorf("runtime argv %q has -i without stdin", first)
	}
}
