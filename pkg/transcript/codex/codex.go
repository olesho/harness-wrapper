// Package codex reads Codex CLI session transcripts.
//
// Codex writes one JSONL per session at:
//
//	~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<timestamp>-<session-uuid>.jsonl
//
// Each line is one event. The lines this reader cares about have
// shape:
//
//	{"type":"response_item","payload":{"role":"assistant",
//	  "content":[{"type":"text","text":"..."}]}}
//
// Roles observed in practice: "user", "assistant", "system", "tool".
// Tool-call payloads are mapped to role "system" for now (the chat
// History view does not need the structural detail).
package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Reader implements transcript.Reader for Codex CLI.
type Reader struct {
	// SessionsRoot overrides the default ~/.codex/sessions/ location.
	// Empty means use the default.
	SessionsRoot string
}

// New constructs a Codex transcript Reader.
func New() *Reader { return &Reader{} }

// Read returns the ordered list of turns for the given Codex session
// UUID. workingDir is ignored — Codex indexes sessions by date/UUID,
// not by working directory.
func (r *Reader) Read(harnessSessionID, _ string) ([]transcript.Event, error) {
	if harnessSessionID == "" {
		return nil, fmt.Errorf("codex transcript: empty session id")
	}
	path, err := r.locate(harnessSessionID)
	if err != nil {
		return nil, err
	}
	return parseJSONL(path)
}

func (r *Reader) sessionsRoot() (string, error) {
	if r.SessionsRoot != "" {
		return r.SessionsRoot, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("codex transcript: resolve home: %w", err)
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// locate scans the sessions root for a file whose name contains the
// given session UUID. Codex's rollout filenames are
// rollout-<timestamp>-<uuid>.jsonl; we match on the suffix so any
// timestamp prefix works.
func (r *Reader) locate(sessionID string) (string, error) {
	root, err := r.sessionsRoot()
	if err != nil {
		return "", err
	}
	suffix := "-" + sessionID + ".jsonl"
	var found string
	werr := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if werr != nil {
		return "", fmt.Errorf("codex transcript: walk %s: %w", root, werr)
	}
	if found == "" {
		return "", fmt.Errorf("codex transcript: no session file for %s under %s", sessionID, root)
	}
	return found, nil
}

// The Codex rollout → Event parser (Events/ParseRollout + the tool-aware
// message/function_call/function_call_output handling) lives in parse_codex.go.

// parseJSONL reads a Codex rollout file and parses it via Events.
func parseJSONL(path string) ([]transcript.Event, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path located under the codex sessions root
	if err != nil {
		return nil, fmt.Errorf("codex transcript: open %s: %w", path, err)
	}
	evs, err := Events(data)
	if err != nil {
		return nil, fmt.Errorf("codex transcript: %s: %w", path, err)
	}
	return evs, nil
}
