//go:build linux

package contain

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// OpenPTYPair opens a pseudoterminal pair from raw descriptors: /dev/ptmx with
// O_NOCTTY|O_CLOEXEC, unlocked, and the slave taken with TIOCGPTPEER rather
// than a path lookup of /dev/pts/N. Both ends are blocking descriptors outside
// Go's netpoller — the contained path never uses os.OpenFile or creack/pty's
// pty.Open — and the caller wraps the master in an *os.File only after the
// spawn, with os.NewFile, which leaves a blocking descriptor unregistered.
//
// It runs on an ordinary thread: Landlock fixes a file's rights when the file
// is opened, so a master opened on the restricted thread would deny the
// wrapper's later resize calls.
func OpenPTYPair() (master, slave int, err error) {
	master, err = unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, -1, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
		_ = unix.Close(master)
		return -1, -1, fmt.Errorf("unlock pty: %w", err)
	}
	r, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(master), unix.TIOCGPTPEER,
		uintptr(unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC))
	if e != 0 {
		_ = unix.Close(master)
		return -1, -1, fmt.Errorf("open pty peer (TIOCGPTPEER): %w", e)
	}
	return master, int(r), nil
}
