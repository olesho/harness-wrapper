package harnesscore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// hookMarkerPrefix tags loom-managed hook commands so the ensure can identify,
// refresh, or remove its own entries without disturbing the user's hooks. It is
// rendered as a trailing shell comment (inert at execution) followed by the
// owner string (review #5).
const hookMarkerPrefix = "harness-wrapper-hook:"

// IsManagedHookCommand reports whether a rendered hook command is one loom owns
// (by the marker), so a merge can replace/remove it idempotently.
func IsManagedHookCommand(command string) bool {
	return strings.Contains(command, hookMarkerPrefix)
}

// RenderHookCommand builds the hook command string written into a harness's
// config. It is ALWAYS a POSIX shell command with a pre-exec env guard so a
// left-in-place entry is inert on a non-wrapper run (review #5):
//
//	sh -c 'test -n "$HW_EVENT_SPOOL" || exit 0; exec <loomArgv> <harness> <arg>' # harness-wrapper-hook:<owner>
//
// With HW_EVENT_SPOOL unset the guard exits 0 WITHOUT touching the binary, so a
// stale/moved loom can never break someone else's run. loomArgv is the loom
// binary path + subcommand (e.g. {"/abs/loom","hooks"}); every interpolated
// value is POSIX-single-quoted, so spaces/quotes in the path are safe.
func RenderHookCommand(loomArgv []string, harnessName, arg, owner string) string {
	tokens := make([]string, 0, len(loomArgv)+2)
	tokens = append(tokens, loomArgv...)
	tokens = append(tokens, harnessName, arg)
	quoted := make([]string, len(tokens))
	for i, tok := range tokens {
		quoted[i] = posixSingleQuote(tok)
	}
	inner := `test -n "$HW_EVENT_SPOOL" || exit 0; exec ` + strings.Join(quoted, " ")
	return "sh -c " + posixSingleQuote(inner) + " # " + hookMarkerPrefix + owner
}

// posixSingleQuote wraps s in single quotes for POSIX sh, escaping embedded
// single quotes as the standard '\” sequence. The empty string becomes ”.
func posixSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// WithLockedFile runs fn under an exclusive flock so concurrent same-worktree
// ensures don't clobber each other, then writes fn's result atomically.
//
// The flock is taken on a STABLE SIDECAR (`<targetPath>.lock`, never renamed) —
// flock is inode-based, so locking the file we then temp+rename-replace would
// drop the guard (the new inode is unlocked). fn receives the current file
// bytes (nil if absent) and returns the new content, or (nil, nil) to indicate
// "no change" (nothing is written). The target itself is written via temp+rename
// for torn-read safety.
func WithLockedFile(targetPath string, fn func(existing []byte) ([]byte, error)) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return fmt.Errorf("harness: create config dir: %w", err)
	}
	lockPath := targetPath + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // sidecar lock in the worktree config dir
	if err != nil {
		return fmt.Errorf("harness: open lock %s: %w", lockPath, err)
	}
	defer func() { _ = lf.Close() }()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("harness: flock %s: %w", lockPath, err)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()

	existing, err := os.ReadFile(targetPath) //nolint:gosec // worktree config path
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("harness: read %s: %w", targetPath, err)
	}
	newContent, err := fn(existing)
	if err != nil {
		return err
	}
	if newContent == nil {
		return nil // no change requested
	}
	return atomicWriteFile(targetPath, newContent)
}

// atomicWriteFile writes data to a uniquely-named temp file in the target's
// directory and renames it into place (atomic on the same filesystem).
//
// The target may be a file the user owns (a worktree's settings.json), so an
// existing file keeps its permission bits, and a symlinked target is followed
// and its destination replaced — renaming over the link itself would silently
// turn it into a regular file (e.g. detaching a dotfiles-managed config). The
// temp is created O_EXCL|O_NOFOLLOW, so nothing already at its name is written
// through, and the file and then its directory are fsynced so a nil return
// means the new content is durable.
func atomicWriteFile(path string, data []byte) error {
	mode := os.FileMode(0o600)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
		if fi, err := os.Stat(path); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, time.Now().UnixNano(), os.Getpid())
	if err := writeTempSynced(tmp, data, mode); err != nil {
		return fmt.Errorf("harness: write temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("harness: commit %s: %w", path, err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("harness: sync dir of %s: %w", path, err)
	}
	return nil
}

// writeTempSynced creates path exclusively and never through a symlink,
// writes data, sets mode exactly (the create mode is filtered by the umask)
// and fsyncs. The file is removed again if any step fails.
func writeTempSynced(path string, data []byte, mode os.FileMode) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode) //nolint:gosec // temp beside a worktree config path
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err == nil {
		if err = f.Chmod(mode); err == nil {
			err = f.Sync()
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
