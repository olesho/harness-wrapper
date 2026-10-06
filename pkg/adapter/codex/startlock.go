package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// startLockFile is the lock codex's starts take turns at, in the scratch
// root.
const startLockFile = "codex-start.lock"

// startLockPoll is how often a start waiting for the lock tries it again.
const startLockPoll = 20 * time.Millisecond

// startLock waits its turn to start an app-server in the environment whose
// scratch root is scratch, until ctx ends. Its release, which the start calls
// once codex answered initialize, may be called more than once.
//
// codex 0.160.0's app-servers started together on a fresh CODEX_HOME fail to
// initialize its state database (openai/codex#50290); one starting after
// another has initialized the home does not. A pin move migrates the
// databases as codex starts, so every start takes its turn, not only the
// first.
func startLock(ctx context.Context, scratch string) (release func(), err error) {
	f, err := os.OpenFile(filepath.Join(scratch, startLockFile), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // a file of the agent's scratch root
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(startLockPoll):
		}
	}
}
