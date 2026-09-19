//go:build linux

package contain

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/olesho/harness-wrapper/internal/apparmor"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"golang.org/x/sys/unix"
)

// socketLayer returns the AppArmor socket layer a launch stacks when the
// kernel's Landlock ABI predates RESOLVE_UNIX (ADR-005), after proving it
// works here: the installed profile source is trusted, and a disposable thread
// that stacks the profile is refused a connect to a pathname socket outside
// its roots. That one test covers every reason the layer could be ineffective
// — profile not loaded, loaded in complain mode, or a kernel whose AppArmor
// does not mediate pathname socket connects — without privilege.
func socketLayer(abi int) (*apparmor.Layer, error) {
	layer, err := installedSocketLayer()
	if err == nil {
		err = socketSelfTest(layer)
	}
	if err != nil {
		return nil, fmt.Errorf("kernel Landlock ABI %d predates RESOLVE_UNIX (ABI %d), so pathname UNIX socket connects need the AppArmor socket layer: %w",
			abi, landlock.ResolveUnixABI, err)
	}
	return layer, nil
}

// installedSocketLayer is apparmor.Installed. A test replaces it to present a
// source that no longer matches the loaded profile, which needs root to stage
// for real.
var installedSocketLayer = apparmor.Installed

// socketSelfTest listens on a pathname socket outside every root, stacks the
// profile onto a disposable thread and connects from it: only EACCES passes.
func socketSelfTest(layer *apparmor.Layer) error {
	dir, err := selfTestDir(layer)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "s")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fmt.Errorf("self-test listener: %w", err)
	}
	defer func() { _ = ln.Close() }()

	var stackErr, connErr error
	onDisposableThread(func() {
		if stackErr = apparmor.StackCurrentThread(layer.Profile); stackErr == nil {
			connErr = connectUnix(path)
		}
	})
	switch {
	case stackErr != nil:
		return stackErr
	case connErr == nil:
		return fmt.Errorf("%w: a thread under %s connected to %s, outside its roots: this kernel's AppArmor does not mediate pathname UNIX socket connects",
			apparmor.ErrUnavailable, layer.Profile, path)
	case errors.Is(connErr, unix.EACCES):
		return nil
	default:
		return fmt.Errorf("%w: self-test connect to %s under %s: %w", apparmor.ErrUnavailable, path, layer.Profile, connErr)
	}
}

// selfTestDir creates a private directory outside every root of layer.
func selfTestDir(layer *apparmor.Layer) (string, error) {
	for _, base := range []string{os.TempDir(), "/tmp", "/dev/shm", "/var/tmp"} {
		canonical, err := filepath.EvalSymlinks(base)
		if err != nil || layer.Covers(canonical) {
			continue
		}
		if dir, err := os.MkdirTemp(canonical, "hw-aa-"); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("%w: no writable directory outside the profile's roots %v to self-test in", apparmor.ErrUnavailable, layer.Roots)
}

// connectUnix connects a fresh stream socket to path and closes it.
func connectUnix(path string) error {
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Connect(fd, &unix.SockaddrUnix{Name: path})
}

// checkSocketRoots refuses a writable grant outside the socket layer's roots:
// the profile would deny the harness's writes there.
func checkSocketRoots(layer *apparmor.Layer, writable []*pinned) error {
	for _, w := range writable {
		if w != nil && !layer.Covers(w.canonical) {
			return refuse(StagePaths,
				"writable grant %s lies outside the AppArmor socket layer's roots %v, where %s denies writes; add a root that covers it (harness-wrapper contain-apparmor-profile) and reload the profile",
				w.canonical, layer.Roots, layer.Profile)
		}
	}
	return nil
}
