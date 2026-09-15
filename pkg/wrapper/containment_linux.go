//go:build linux

package wrapper

import (
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// masterReader reads a contained session's PTY master, a blocking descriptor
// outside the netpoller (see internal/contain). Closing a blocking *os.File
// waits for a read already blocked on it instead of interrupting it, so a
// process the harness left holding the terminal without writing would keep
// that read, and Wait, open for good. Read therefore waits in poll(2) on the
// master and an eventfd, and stop signals the eventfd, which ends a waiting
// read and every later one with io.EOF. Reads go through the file's RawConn,
// so the descriptor cannot be closed and reused while one waits on it.
type masterReader struct {
	rc   syscall.RawConn
	wake int
}

// openWake creates the eventfd a masterReader waits on. It is made before the
// spawn, so that failing to make it refuses the launch.
func openWake() (int, error) {
	return unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
}

func newMasterReader(f *os.File, wake int) (*masterReader, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	return &masterReader{rc: rc, wake: wake}, nil
}

func (r *masterReader) Read(p []byte) (int, error) {
	var n int
	var rerr error
	err := r.rc.Read(func(fd uintptr) bool {
		fds := []unix.PollFd{
			{Fd: int32(fd), Events: unix.POLLIN},
			{Fd: int32(r.wake), Events: unix.POLLIN},
		}
		for {
			if _, err := unix.Poll(fds, -1); err != unix.EINTR {
				rerr = err
				break
			}
		}
		switch {
		case rerr != nil:
		case fds[1].Revents != 0:
			rerr = io.EOF
		default:
			for {
				if n, rerr = unix.Read(int(fd), p); rerr != unix.EINTR {
					break
				}
			}
			n = max(n, 0)
		}
		return true
	})
	if err != nil {
		return 0, err
	}
	return n, rerr
}

// stop ends a Read waiting on the master, and every later one.
func (r *masterReader) stop() {
	one := [8]byte{1}
	_, _ = unix.Write(r.wake, one[:])
}

// close releases the eventfd once no Read can run any more.
func (r *masterReader) close() { _ = unix.Close(r.wake) }
