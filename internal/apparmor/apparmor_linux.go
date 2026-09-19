//go:build linux

package apparmor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ErrUnavailable is wrapped by every reason the socket layer cannot be used.
var ErrUnavailable = errors.New("apparmor: socket layer unavailable")

const (
	enabledPath = "/sys/module/apparmor/parameters/enabled"
	threadAttr  = "/proc/thread-self/attr/apparmor/"
	stackCmd    = "stack " + ProfileName
	// maxPolicyBytes bounds the installed source read; a generated profile is
	// a few hundred bytes per root.
	maxPolicyBytes = 1 << 20
)

// Installed returns the socket layer the installed profile source describes,
// after checking AppArmor is enabled and the source can be trusted: a regular
// file owned by root and writable by no one else, in a directory with the
// same ownership. It cannot tell, unprivileged, whether the profile is loaded
// or whether the kernel mediates pathname socket connects; the caller proves
// both by stacking the profile onto a disposable thread (StackCurrentThread)
// and connecting.
func Installed() (*Layer, error) {
	b, err := os.ReadFile(enabledPath)
	if err != nil || strings.TrimSpace(string(b)) != "Y" {
		return nil, fmt.Errorf("%w: AppArmor is not enabled on this kernel", ErrUnavailable)
	}
	src, err := readTrusted(PolicyPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	roots, err := ParseRoots(src)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return &Layer{Roots: roots}, nil
}

// readTrusted reads path, refusing a symlink, a non-regular file, or a file
// or parent directory that anyone but root owns or may write.
func readTrusted(path string) ([]byte, error) {
	if err := rootOnly(filepath.Dir(path), unix.O_DIRECTORY); err != nil {
		return nil, err
	}
	// O_NONBLOCK: a FIFO in its place must fail the type check, not hang.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, fmt.Errorf("%s is not installed; generate it with harness-wrapper contain-apparmor-profile and load it with apparmor_parser -r %s", path, path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	if err := checkRootOnly(fd, path, unix.S_IFREG); err != nil {
		return nil, err
	}
	return io.ReadAll(io.LimitReader(f, maxPolicyBytes))
}

func rootOnly(path string, flags int) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|flags, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	return checkRootOnly(fd, path, unix.S_IFDIR)
}

func checkRootOnly(fd int, path string, kind uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	switch {
	case st.Mode&unix.S_IFMT != kind:
		return fmt.Errorf("%s is not a %s", path, map[uint32]string{unix.S_IFREG: "regular file", unix.S_IFDIR: "directory"}[kind])
	case st.Uid != 0:
		return fmt.Errorf("%s is owned by uid %d, not root: its roots cannot be trusted", path, st.Uid)
	case st.Mode&0o022 != 0:
		return fmt.Errorf("%s is writable by group or others (mode %#o): its roots cannot be trusted", path, st.Mode&0o7777)
	}
	return nil
}

// StackCurrentThread stacks the profile onto the calling thread, at once and
// irreversibly, and confirms the thread's label now includes it in enforce
// mode. The caller must hold runtime.LockOSThread, never unlock it, and let
// the thread exit afterwards.
func StackCurrentThread() error {
	if err := writeAttr("current"); err != nil {
		return err
	}
	b, err := os.ReadFile(threadAttr + "current")
	if err != nil {
		return fmt.Errorf("%w: read the stacked label: %w", ErrUnavailable, err)
	}
	if !labelHas(string(b), ProfileName) {
		return fmt.Errorf("%w: after stacking %s the label is %q, not the profile in enforce mode", ErrUnavailable, ProfileName, strings.TrimSpace(string(b)))
	}
	return nil
}

// StackOnExec makes the calling thread's next exec stack the profile onto the
// new program. The caller must hold runtime.LockOSThread and never unlock
// it. It allocates only the attribute path and makes no descriptor calls but
// the open, write and close of that attribute, so it is safe on a thread whose
// descriptor table is private.
func StackOnExec() error { return writeAttr("exec") }

func writeAttr(which string) error {
	fd, err := unix.Open(threadAttr+which, unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("%w: open the thread's AppArmor %s attribute: %w", ErrUnavailable, which, err)
	}
	_, err = unix.Write(fd, []byte(stackCmd))
	_ = unix.Close(fd)
	if errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("%w: profile %s is not loaded; load it with apparmor_parser -r %s", ErrUnavailable, ProfileName, PolicyPath)
	}
	if err != nil {
		return fmt.Errorf("%w: %q to the thread's AppArmor %s attribute: %w", ErrUnavailable, stackCmd, which, err)
	}
	return nil
}

// CheckConfined reports an error unless process pid runs with the profile in
// its label in enforce mode.
func CheckConfined(pid int) error {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/attr/apparmor/current")
	if err != nil {
		return fmt.Errorf("%w: read the harness's AppArmor label: %w", ErrUnavailable, err)
	}
	if !labelHas(string(b), ProfileName) {
		return fmt.Errorf("%w: the harness started with AppArmor label %q, not under %s in enforce mode", ErrUnavailable, strings.TrimSpace(string(b)), ProfileName)
	}
	return nil
}
