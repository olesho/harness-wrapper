package claudecode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Locate returns the path of the transcript Claude Code keeps for session
// sessionID when launched in workingDir with env, by the rules the launch
// adapter reads it by (pkg/turns/harness/claudecode): under
// $CLAUDE_CONFIG_DIR/projects when env sets it — the last occurrence, trimmed,
// resolved against workingDir when relative, since claude takes it verbatim
// from its own cwd — and under ~/.claude/projects otherwise; in the directory
// named for the realpath of workingDir, or for workingDir as given.
//
// env is the harness's launch environment; nil means it inherited this
// process's, as exec treats a nil Env. A transcript that does not exist is an
// error wrapping fs.ErrNotExist.
func Locate(sessionID, workingDir string, env []string) (string, error) {
	root, err := launchProjectsRoot(sessionID, workingDir, env)
	if err != nil {
		return "", err
	}
	path, err := transcriptPath(root, sessionID, workingDir)
	if err != nil {
		return "", err
	}
	return path, nil
}

// Follow returns a Follower for the transcript of Claude Code session
// sessionID, launched in workingDir with env (see Locate), resuming from the
// checkpoint the caller last committed — the zero Checkpoint starts at the
// beginning of the file.
//
// The transcript need not exist yet: claude creates it with its first entry,
// and until then the follower waits where claude will write it, Poll reporting
// fs.ErrNotExist. Each entry becomes the events Read gives for it; an entry
// that is not JSON, or a user or assistant entry whose message cannot be read,
// becomes a SourceError instead.
func Follow(sessionID, workingDir string, env []string, from transcript.Checkpoint) (*transcript.Follower, error) {
	root, err := launchProjectsRoot(sessionID, workingDir, env)
	if err != nil {
		return nil, err
	}
	path, err := transcriptPath(root, sessionID, workingDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return transcript.NewFollower(path, sessionID, from, decodeRecord)
}

// launchProjectsRoot is the projects directory claude writes to when
// launched in workingDir with env; see Locate.
func launchProjectsRoot(sessionID, workingDir string, env []string) (string, error) {
	if sessionID == "" {
		return "", errors.New("claudecode transcript: empty session id")
	}
	if workingDir == "" {
		return "", errors.New("claudecode transcript: empty working dir")
	}
	if env == nil {
		env = os.Environ()
	}
	if dir := strings.TrimSpace(envLookup(env, "CLAUDE_CONFIG_DIR")); dir != "" {
		return transcript.ResolveHarnessPath(filepath.Join(dir, "projects"), workingDir), nil
	}
	root, err := (&Reader{}).projectsRoot()
	if err != nil {
		return "", fmt.Errorf("claudecode transcript: %w", err)
	}
	return root, nil
}

// envLookup returns the value of key in an os.Environ()-style "K=V" slice,
// or "" if absent; the LAST occurrence wins, as exec resolves duplicates. It
// mirrors the launch adapter's, which this package cannot import.
func envLookup(env []string, key string) string {
	prefix := key + "="
	val := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			val = kv[len(prefix):]
		}
	}
	return val
}
