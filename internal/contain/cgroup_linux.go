//go:build linux

package contain

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// cgroupRoot is where the unified cgroup v2 hierarchy must be mounted.
const cgroupRoot = "/sys/fs/cgroup"

// cgroupPrefix names every session cgroup this package creates, so crash
// recovery never touches a cgroup it did not make.
const cgroupPrefix = "hwc-"

// emptyBudget bounds how long teardown waits for a killed cgroup to report
// "populated 0". cgroup.kill SIGKILLs every member at once, so the wait is
// normally well under a millisecond; only a process stuck in uninterruptible
// sleep can outlast it, and then the state is kept rather than deleted under
// it.
const emptyBudget = 30 * time.Second

// sessionCgroup is a per-session cgroup v2 that contains the harness and
// every descendant: the harness is created inside it (CLONE_INTO_CGROUP), and
// the domain denies every write under cgroupfs and the user manager's
// sockets, so no descendant can leave it.
type sessionCgroup struct {
	path string
	fd   int // O_RDONLY|O_DIRECTORY, for SysProcAttr.CgroupFD
}

// ownCgroup returns the wrapper's own cgroup directory.
func ownCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join(cgroupRoot, rest), nil
		}
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

// clone3Available distinguishes a seccomp-filtered clone3 (ENOSYS) from an
// available one (EINVAL for a zero-sized argument). It creates no process.
func clone3Available() bool {
	_, _, e := unix.Syscall(unix.SYS_CLONE3, 0, 0, 0)
	return e != unix.ENOSYS
}

// probeSupervision reports the cgroup under which session cgroups can be
// created, or why none can: supervision needs the unified hierarchy, write
// access to the wrapper's own cgroup (a unit or scope with Delegate=yes, or
// root in a container with a writable cgroup namespace) and an unfiltered
// clone3.
func probeSupervision() (string, string) {
	var sfs unix.Statfs_t
	if err := unix.Statfs(cgroupRoot, &sfs); err != nil {
		return "", fmt.Sprintf("%s is not mounted: %v", cgroupRoot, err)
	}
	if sfs.Type != unix.CGROUP2_SUPER_MAGIC {
		return "", fmt.Sprintf("%s is not a unified cgroup v2 hierarchy", cgroupRoot)
	}
	own, err := ownCgroup()
	if err != nil {
		return "", err.Error()
	}
	if err := unix.Access(filepath.Join(own, "cgroup.procs"), unix.W_OK); err != nil {
		return "", fmt.Sprintf("the wrapper's cgroup %s is not delegated to it (%v); run under a unit or scope with Delegate=yes", own, err)
	}
	if !clone3Available() {
		return "", "clone3 is filtered by seccomp, so CLONE_INTO_CGROUP is unavailable"
	}
	return own, ""
}

// probeSupervisionFn indirects probeSupervision for DisableSupervisionForTest.
var probeSupervisionFn = probeSupervision

// DisableSupervisionForTest makes every launch see a host that delegates no
// cgroup, until the returned function runs. Tests only.
func DisableSupervisionForTest() (restore func()) {
	prev := probeSupervisionFn
	probeSupervisionFn = func() (string, string) { return "", "supervision disabled by a test" }
	return func() { probeSupervisionFn = prev }
}

// newSessionCgroup creates the session cgroup beneath the wrapper's own. It
// returns (nil, reason) when the host delegates no cgroup: supervision is then
// "none", which is reported, never silent.
func newSessionCgroup(stateID string) (*sessionCgroup, string, error) {
	own, reason := probeSupervisionFn()
	if own == "" {
		return nil, reason, nil
	}
	suffix, err := newID()
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(own, cgroupPrefix+stateID+"-"+suffix[:6])
	if err := unix.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.EROFS) {
			return nil, fmt.Sprintf("cannot create a cgroup beneath %s: %v", own, err), nil
		}
		return nil, "", fmt.Errorf("create session cgroup: %w", err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Rmdir(path)
		return nil, "", fmt.Errorf("open session cgroup: %w", err)
	}
	return &sessionCgroup{path: path, fd: fd}, "", nil
}

func (c *sessionCgroup) closeFD() {
	if c != nil && c.fd >= 0 {
		_ = unix.Close(c.fd)
		c.fd = -1
	}
}

// kill SIGKILLs every process in the cgroup (cgroup.kill, safe against
// concurrent forks).
func (c *sessionCgroup) kill() error { return killCgroup(c.path) }

func killCgroup(path string) error {
	err := os.WriteFile(filepath.Join(path, "cgroup.kill"), []byte("1"), 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// populated reads "populated" from cgroup.events.
func populated(path string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return false, err
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "populated "); ok {
			return v == "1", nil
		}
	}
	return false, errors.New("cgroup.events has no populated key")
}

// waitEmpty blocks until cgroup.events reports "populated 0", woken by
// inotify (kernfs notifies on every change of the file), or until ctx ends.
// An absent cgroup is empty: a cgroup with live processes cannot be removed.
func waitEmpty(ctx context.Context, path string) error {
	ifd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(ifd) }()
	if _, err := unix.InotifyAddWatch(ifd, filepath.Join(path, "cgroup.events"), unix.IN_MODIFY); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	buf := make([]byte, 4096)
	for {
		pop, err := populated(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !pop {
			return nil
		}
		wait := 100 * time.Millisecond
		if dl, ok := ctx.Deadline(); ok {
			wait = min(wait, time.Until(dl))
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("cgroup %s still populated: %w", path, err)
		}
		fds := []unix.PollFd{{Fd: int32(ifd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, max(1, int(wait/time.Millisecond))); err != nil && !errors.Is(err, unix.EINTR) {
			return err
		}
		for {
			if _, err := unix.Read(ifd, buf); err != nil {
				break
			}
		}
	}
}

// remove deletes the cgroup directory, which the kernel allows only once it
// is empty. Sub-cgroups go first; the domain forbids creating them, but a
// cgroup made before containment applied would still be removed.
func removeCgroup(path string) error {
	var subs []string
	_ = filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != path {
			subs = append(subs, p)
		}
		return nil
	})
	for i := len(subs) - 1; i >= 0; i-- {
		_ = unix.Rmdir(subs[i])
	}
	err := unix.Rmdir(path)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

// validRecordedCgroup reports whether path looks like a session cgroup this
// package created: recovery kills only those.
func validRecordedCgroup(path string) bool {
	clean := filepath.Clean(path)
	return clean == path && strings.HasPrefix(clean, cgroupRoot+"/") &&
		strings.HasPrefix(filepath.Base(clean), cgroupPrefix)
}

// recoverCgroup ends a recorded session cgroup left by an earlier launch:
// kill, wait for "populated 0", remove. ENOENT at any step means done.
func recoverCgroup(ctx context.Context, path string) error {
	if !validRecordedCgroup(path) {
		return fmt.Errorf("recorded cgroup %q is not a session cgroup", path)
	}
	if err := killCgroup(path); err != nil {
		return fmt.Errorf("kill %s: %w", path, err)
	}
	wctx, cancel := context.WithTimeout(ctx, emptyBudget)
	defer cancel()
	if err := waitEmpty(wctx, path); err != nil {
		return err
	}
	return removeCgroup(path)
}
