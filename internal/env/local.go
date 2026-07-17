package env

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// The Local provisioner (design §3 shipped implementations).
//
// The degenerate provisioner: host == guest. Exec is os/exec; Upload/Download
// are filesystem copies. No image is booted — a per-workspace base directory
// (named by the DETERMINISTIC spec.Name for crash recovery) stands in for the
// machine, with repo / .home / tmp subdirs.

var subdir = map[PathKind]string{
	PathRepo: "repo",
	PathHome: ".home",
	PathTmp:  "tmp",
}

type localWorkspace struct {
	base string
	spec WorkspaceSpec

	mu        sync.Mutex
	destroyed bool
}

func (w *localWorkspace) Exec(ctx context.Context, argv []string, opts *ExecOpts) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, errors.New("local.exec: empty argv")
	}
	cwd := w.GuestPath(PathRepo)
	if opts != nil && opts.Cwd != "" {
		cwd = opts.Cwd
	}

	// argv is passed as a list (no shell), so no argument can inject an extra
	// command — no shell metacharacter is ever interpreted here. CommandContext
	// kills the process when ctx is cancelled.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	if opts != nil && opts.Env != nil {
		// Overlay the caller's env onto the host env — env crosses via opts.Env.
		env := os.Environ()
		for k, v := range opts.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	if opts != nil && opts.Stdin != nil {
		cmd.Stdin = strings.NewReader(*opts.Stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		return ExecResult{}, ctx.Err()
	}
	if err != nil {
		// A non-zero exit resolves with the code (batch model); only a
		// spawn/execution error (e.g. command not found) is a hard failure.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return ExecResult{Code: exitErr.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}, nil
		}
		return ExecResult{}, err
	}
	return ExecResult{Code: 0, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

func (w *localWorkspace) Upload(_ context.Context, hostPath, guestPath string) error {
	// Recursive copy preserving mode bits (executable flags) and directory trees
	// such as .git.
	if err := mkParent(guestPath); err != nil {
		return err
	}
	return copyPath(hostPath, guestPath)
}

func (w *localWorkspace) Download(_ context.Context, guestPath, hostPath string) error {
	if err := mkParent(hostPath); err != nil {
		return err
	}
	return copyPath(guestPath, hostPath)
}

func (w *localWorkspace) GuestPath(kind PathKind) string {
	return filepath.Join(w.base, subdir[kind])
}

func (w *localWorkspace) HostAlias(hostURL string) string {
	// Degenerate host == guest: host URLs are already guest-reachable.
	return hostURL
}

func (w *localWorkspace) Destroy(_ context.Context, outcome Outcome) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.destroyed {
		return nil // idempotent: double-destroy is a no-op.
	}
	w.destroyed = true
	if ShouldKeep(w.spec.Retention, outcome) {
		return nil // kept for debugging.
	}
	return os.RemoveAll(w.base)
}

func mkParent(p string) error {
	dir := filepath.Dir(p)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// copyPath recursively copies src to dst, preserving file modes.
func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.Remove(dst)
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	default:
		return copyFile(src, dst, info.Mode().Perm())
	}
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

type localProvisioner struct {
	root string
}

func (p *localProvisioner) Name() string { return "local" }

func (p *localProvisioner) Preflight(_ context.Context) error {
	// Host-side, zero resources: the base root must be creatable. mkdir is
	// idempotent and cheap; a permission error surfaces here, before Create.
	return os.MkdirAll(p.root, 0o755)
}

func (p *localProvisioner) Create(_ context.Context, spec WorkspaceSpec) (Workspace, error) {
	base := filepath.Join(p.root, spec.Name)
	// Crash recovery: a leftover from a crashed run under the SAME deterministic
	// name is removed before recreate (orche sandboxName pattern).
	if err := os.RemoveAll(base); err != nil {
		return nil, err
	}
	for _, kind := range []PathKind{PathRepo, PathHome, PathTmp} {
		if err := os.MkdirAll(filepath.Join(base, subdir[kind]), 0o755); err != nil {
			return nil, err
		}
	}
	return &localWorkspace{base: base, spec: spec}, nil
}

// Local constructs the local provisioner. An empty root defaults to an OS-temp
// subdir; pass an explicit root for hermetic tests.
func Local(root string) Provisioner {
	if root == "" {
		root = filepath.Join(os.TempDir(), "meta-harness-env")
	}
	return &localProvisioner{root: root}
}
