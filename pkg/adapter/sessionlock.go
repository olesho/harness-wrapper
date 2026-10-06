package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// locksDir is where an agent's Session locks live, under its scratch root.
const locksDir = "sessions"

// sessionLock is what a Host holds of a Session while its harness runs, so
// that no other Host starts the Session's harness meanwhile: two harness
// processes on one Session write its record from two sides, and claude
// resumes a session another claude holds into the same transcript. It is an
// flock(2) on a file named for the Session, which the kernel lets go of when
// the Host's process ends, however it ends.
type sessionLock struct{ f *os.File }

// lockSession takes the lock of Session id under scratch, or refuses with
// open_failed session_in_use while another Host holds it.
func lockSession(scratch, id string) (*sessionLock, error) {
	dir := filepath.Join(scratch, locksDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("adapter: session lock: %w", err)
	}
	sum := sha256.Sum256([]byte(id))
	f, err := os.OpenFile(filepath.Join(dir, hex.EncodeToString(sum[:])+".lock"), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // a file of the agent's scratch root, named by a digest
	if err != nil {
		return nil, fmt.Errorf("adapter: session lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &contract.Error{
				Code: contract.CodeOpenFailed, Reason: contract.OpenSessionInUse,
				Message: fmt.Sprintf("session %q is open in another Host", id),
			}
		}
		return nil, fmt.Errorf("adapter: session lock: %w", err)
	}
	return &sessionLock{f: f}, nil
}

// release lets the lock go; releasing nil, or twice, does nothing.
func (l *sessionLock) release() {
	if l != nil {
		_ = l.f.Close()
	}
}
