//go:build !unix

package wrapper

import (
	"errors"
	"os"
	"syscall"
)

// resolveSessionGroup reports no signalable group on platforms without POSIX
// process groups, so the wrapper signals the harness process alone — exactly
// what it did on every platform before group-scoped termination. Keeping the
// same helpers on both sides keeps procgroup.go and session.go free of build
// tags.
func resolveSessionGroup(int) int { return 0 }

// killGroup is never reached here: resolveSessionGroup yields no group.
func killGroup(int, syscall.Signal) error { return errors.ErrUnsupported }

// groupHasMembers has no group to report on.
func groupHasMembers(int) bool { return false }

// ignoreProcessGone maps "the target is already gone" onto success.
func ignoreProcessGone(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
