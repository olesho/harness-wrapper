// Package claudecode reads Claude Code session transcripts.
//
// Claude Code writes one JSONL per session at:
//
//	~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl
//
// where <encoded-cwd> is the absolute working directory with every
// NON-ALPHANUMERIC character replaced by '-' (so /Users/.../harness-wrapper
// becomes "-Users-...-harness-wrapper", and ~/.loom/x becomes
// "-Users-oleh--loom-x" — the '.' is encoded too; see EncodedCWD).
//
// Schema (excerpt) — the keys this reader consumes:
//
//	{"type":"user",      "message":{"role":"user",      "content":"text..."},
//	 "sessionId":"...", "timestamp":"2026-05-14T...Z"}
//
//	{"type":"assistant", "message":{"role":"assistant", "content":[
//	    {"type":"text","text":"..."}]}, "timestamp":"..."}
//
// Other line types (permission-mode, file-history-snapshot, attachment,
// system, ai-title) are skipped.
package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Reader implements transcript.Reader for Claude Code.
type Reader struct {
	// ProjectsRoot overrides the default ~/.claude/projects/ location.
	// Empty means use the default.
	//
	// This reader deliberately stays ENVIRONMENT-BLIND: it never reads
	// CLAUDE_CONFIG_DIR itself, because a library path that silently changes
	// behaviour with the process environment would pick the wrong process's
	// view when one process drives several profiled agents. The caller sets
	// this instead — the turns adapter
	// (pkg/turns/harness/claudecode.Adapter.ConfigureFromEnv) derives it from
	// the HARNESS LAUNCH env's CLAUDE_CONFIG_DIR as <dir>/projects.
	ProjectsRoot string
}

// New constructs a Claude Code transcript Reader.
func New() *Reader { return &Reader{} }

// Read returns the canonical Event stream for the given Claude Code session
// UUID. workingDir is required: Claude Code indexes transcripts by working
// directory.
func (r *Reader) Read(harnessSessionID, workingDir string) ([]transcript.Event, error) {
	if harnessSessionID == "" {
		return nil, fmt.Errorf("claudecode transcript: empty session id")
	}
	if workingDir == "" {
		return nil, fmt.Errorf("claudecode transcript: empty working dir")
	}
	path, err := r.locate(harnessSessionID, workingDir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is located under projectsRoot
	if err != nil {
		return nil, fmt.Errorf("claudecode transcript: read %s: %w", path, err)
	}
	return Events(data)
}

// ReadUsage returns best-effort token accounting for the given Claude Code
// session, or (nil, nil) when the transcript carried no usage. It reuses the
// same locate flow as Read, so *Reader satisfies transcript.UsageReader.
func (r *Reader) ReadUsage(harnessSessionID, workingDir string) (*transcript.Usage, error) {
	path, err := r.locate(harnessSessionID, workingDir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is located under projectsRoot
	if err != nil {
		return nil, fmt.Errorf("claudecode transcript: read %s: %w", path, err)
	}
	return UsageFromJSONL(data)
}

// claudeCWDSanitize matches every character Claude Code rewrites when it names
// a project dir: anything that is not an ASCII letter or digit. Crucially this
// includes '.', so a path under ~/.loom encodes the dot too. Replacing only '/'
// produced "-Users-oleh-.loom-..." while Claude writes "-Users-oleh--loom-...",
// so the project dir was never found and transcripts came back empty for every
// run under ~/.loom (the fleet/daemon worktree root). Hyphens map to themselves,
// matching Claude's behavior of leaving existing '-' in place (no collapsing).
var claudeCWDSanitize = regexp.MustCompile(`[^A-Za-z0-9]`)

// EncodedCWD returns the directory-name-encoding Claude Code uses for project
// paths: every non-alphanumeric character (including '/', '.', '_') becomes '-'
// (a leading slash yields a leading hyphen). Exposed for tests and for callers
// that map a working directory to its on-disk transcript slot.
func EncodedCWD(workingDir string) string {
	return claudeCWDSanitize.ReplaceAllString(workingDir, "-")
}

func (r *Reader) projectsRoot() (string, error) {
	if r.ProjectsRoot != "" {
		return r.ProjectsRoot, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("claudecode transcript: resolve home: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

func (r *Reader) locate(sessionID, workingDir string) (string, error) {
	root, err := r.projectsRoot()
	if err != nil {
		return "", err
	}
	// Claude Code derives the project dir from the REALPATH of the cwd (it
	// resolves symlinks before encoding), so a symlinked working dir yields a
	// slug that never matches unless we resolve too. macOS is the common trap:
	// temp dirs live under /var and /tmp, both symlinks into /private, so a cwd
	// of /var/folders/... is written as "-private-var-folders-...". Try the
	// resolved slug first, then fall back to the path as given — covering a
	// harness that did NOT resolve, and a since-removed dir EvalSymlinks can't
	// stat.
	candidates := make([]string, 0, 2)
	if resolved, rerr := filepath.EvalSymlinks(workingDir); rerr == nil && resolved != workingDir {
		candidates = append(candidates, resolved)
	}
	candidates = append(candidates, workingDir)

	var firstPath string
	var firstErr error
	for _, wd := range candidates {
		path := filepath.Join(root, EncodedCWD(wd), sessionID+".jsonl")
		if _, statErr := os.Stat(path); statErr == nil {
			return path, nil
		} else if firstPath == "" {
			firstPath, firstErr = path, statErr
		}
	}
	return "", fmt.Errorf("claudecode transcript: %s: %w", firstPath, firstErr)
}

// The Claude line→Event parser (Events/userLineEvents/assistantLineEvents) lives
// in parse_claude.go — a port of loomcli's tool-aware claude parser.
