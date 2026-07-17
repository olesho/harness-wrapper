package env

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
)

// Test-only container workspace — a minimal docker/podman-based Workspace
// implementation for in-guest e2e testing. Uses plain docker run/exec/cp over
// os/exec (no SDK, hermetic, suitable for CI).

// containerSeq gives create a monotonic fallback name without needing a clock.
var containerSeq int64

// DetectContainerRuntime reports whether docker or podman is available on the
// system, returning the command name ("docker" or "podman"), or "" if neither
// is found.
func DetectContainerRuntime() string {
	for _, rt := range []string{"docker", "podman"} {
		cmd := exec.Command(rt, "--version")
		if err := cmd.Run(); err == nil {
			return rt
		}
	}
	return ""
}

// ContainerWorkspace is a test-only Workspace implementation that drives a
// container via docker run/exec/cp.
type ContainerWorkspace struct {
	runtime     string
	containerID string
	tmpDir      string
}

// NewContainerWorkspace wires up a workspace around an already-running
// container. CreateContainerWorkspace is the usual entry point.
func NewContainerWorkspace(runtime, containerID, tmpDir string) *ContainerWorkspace {
	return &ContainerWorkspace{runtime: runtime, containerID: containerID, tmpDir: tmpDir}
}

func (c *ContainerWorkspace) Exec(ctx context.Context, argv []string, opts *ExecOpts) (ExecResult, error) {
	cwd := "/repo"
	var env map[string]string
	if opts != nil {
		if opts.Cwd != "" {
			cwd = opts.Cwd
		}
		env = opts.Env
	}

	// Build the docker/podman exec command with env vars.
	args := []string{"exec", "-w", cwd}
	for k, v := range env {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, c.containerID)
	args = append(args, argv...)

	cmd := exec.CommandContext(ctx, c.runtime, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return ExecResult{}, ctx.Err()
	}
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return ExecResult{}, err
		}
		code = exitErr.ExitCode()
	}
	return ExecResult{Code: code, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

func (c *ContainerWorkspace) Upload(ctx context.Context, hostPath, guestPath string) error {
	// docker/podman cp to copy the file in.
	return exec.CommandContext(ctx, c.runtime, "cp", hostPath, c.containerID+":"+guestPath).Run()
}

func (c *ContainerWorkspace) Download(ctx context.Context, guestPath, hostPath string) error {
	// docker/podman cp to copy the file back.
	return exec.CommandContext(ctx, c.runtime, "cp", c.containerID+":"+guestPath, hostPath).Run()
}

func (c *ContainerWorkspace) GuestPath(kind PathKind) string {
	switch kind {
	case PathRepo:
		return "/repo"
	case PathHome:
		return "/home"
	default:
		return "/tmp"
	}
}

func (c *ContainerWorkspace) HostAlias(hostURL string) string {
	// In docker/podman, host.docker.internal / host.containers.internal reach the
	// host; for simplicity in tests, just rewrite localhost.
	return strings.ReplaceAll(hostURL, "localhost", "host.docker.internal")
}

func (c *ContainerWorkspace) Destroy(ctx context.Context, _ Outcome) error {
	// Stop and remove the container, then clean up the temp dir. Best-effort.
	_ = exec.CommandContext(ctx, c.runtime, "stop", c.containerID).Run()
	_ = exec.CommandContext(ctx, c.runtime, "rm", c.containerID).Run()
	if c.tmpDir != "" {
		_ = os.RemoveAll(c.tmpDir)
	}
	return nil
}

// CreateContainerWorkspace boots a container for spec and returns a workspace
// bound to it.
func CreateContainerWorkspace(ctx context.Context, spec WorkspaceSpec) (*ContainerWorkspace, error) {
	runtime := DetectContainerRuntime()
	if runtime == "" {
		return nil, fmt.Errorf("container-workspace: docker or podman is not available")
	}

	tmpDir, err := os.MkdirTemp("", "mh-container-")
	if err != nil {
		return nil, err
	}
	name := spec.Name
	if name == "" {
		name = fmt.Sprintf("meta-harness-test-%d", atomic.AddInt64(&containerSeq, 1))
	}
	image := spec.Image
	if image == "" {
		image = "node:latest"
	}

	out, err := exec.CommandContext(ctx, runtime,
		"run", "-d", "-i", "--name", name, "--rm", image,
		"tail", "-f", "/dev/null",
	).Output()
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, err
	}
	containerID := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return NewContainerWorkspace(runtime, containerID, tmpDir), nil
}
