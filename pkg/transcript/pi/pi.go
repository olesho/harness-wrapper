// Package pi reads pi coding-agent session transcripts
// (@earendil-works/pi-coding-agent, binary "pi" —
// github.com/earendil-works/pi).
//
// pi writes one JSONL per session at:
//
//	<config>/sessions/--<cwd-slug>--/<timestamp>_<uuid>.jsonl
//
// where <config> defaults to ~/.pi/agent (overridable via the
// PI_CODING_AGENT_DIR environment variable) and <cwd-slug> is the
// session's working directory with path separators rendered as hyphens
// and wrapped in "--" … "--" (e.g. /home/u/proj → --home-u-proj--).
//
// Schema notes — pi session format v3:
//
//   - The first line is a header object carrying metadata only:
//     {"type":"session","version":3,"id":"<uuid>","timestamp":"…","cwd":"…"}.
//     The reader skips it (but uses its "id" to confirm a located file).
//   - Subsequent lines are tagged by a top-level "type". Only
//     "type":"message" lines carry conversation content; every other
//     type (model_change, thinking_level_change, compaction,
//     branch_summary, custom, custom_message, label, session_info) is
//     skipped.
//   - A message line nests an object under "message" with a "role"
//     ("user" | "assistant" | "toolResult") and a "content" field that is
//     either a plain string or an array of typed blocks
//     ([{"type":"text","text":"…"}, {"type":"image",…}, …]). The reader
//     accepts both and concatenates the text blocks.
package pi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Reader implements transcript.Reader for the pi coding agent.
type Reader struct {
	// Root overrides the pi agent config directory (the ~/.pi/agent
	// equivalent under which sessions/ lives). Empty means consult
	// PI_CODING_AGENT_DIR and then fall back to ~/.pi/agent.
	Root string
}

// New constructs a pi transcript Reader.
func New() *Reader { return &Reader{} }

// Read returns the ordered list of turns for the given pi session UUID.
// workingDir is optional: when set it lets Read jump straight to the
// per-cwd session directory; when empty (or when the slug guess misses)
// Read walks every session directory and confirms the match by header.
func (r *Reader) Read(harnessSessionID, workingDir string) ([]transcript.Turn, error) {
	if harnessSessionID == "" {
		return nil, fmt.Errorf("pi transcript: empty session id")
	}
	path, err := r.locate(harnessSessionID, workingDir)
	if err != nil {
		return nil, err
	}
	return parseJSONL(path)
}

func (r *Reader) configDir() (string, error) {
	if r.Root != "" {
		return r.Root, nil
	}
	if env := os.Getenv("PI_CODING_AGENT_DIR"); env != "" {
		return env, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("pi transcript: resolve home: %w", err)
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

func (r *Reader) sessionsDir() (string, error) {
	cfg, err := r.configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "sessions"), nil
}

// locate resolves the session file for sessionID. It first probes the
// per-cwd slug directory (a cheap guess) and, failing that, walks every
// sessions/*/ directory. In both cases a filename-contains-id match is
// confirmed against the file's header "id" so shared-substring IDs across
// directories cannot produce a false positive.
func (r *Reader) locate(sessionID, workingDir string) (string, error) {
	sessionsDir, err := r.sessionsDir()
	if err != nil {
		return "", err
	}

	if workingDir != "" {
		slugDir := filepath.Join(sessionsDir, slugForCwd(workingDir))
		if path, found, err := findInDir(slugDir, sessionID); err != nil {
			return "", err
		} else if found {
			return path, nil
		}
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return "", fmt.Errorf("pi transcript: read %s: %w", sessionsDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if path, found, err := findInDir(filepath.Join(sessionsDir, e.Name()), sessionID); err != nil {
			return "", err
		} else if found {
			return path, nil
		}
	}
	return "", fmt.Errorf("pi transcript: no session file for %s under %s", sessionID, sessionsDir)
}

// findInDir looks for a session file in a single directory whose name
// contains sessionID and whose header "id" confirms the match. Returns
// ("", false, nil) when the directory is absent or holds no match.
func findInDir(dir, sessionID string) (string, bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("pi transcript: read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") || !strings.Contains(name, sessionID) {
			continue
		}
		path := filepath.Join(dir, name)
		if confirmHeader(path, sessionID) {
			return path, true, nil
		}
	}
	return "", false, nil
}

// confirmHeader opens path, reads the first line, and returns true when
// its "id" matches sessionID.
func confirmHeader(path, sessionID string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	if !sc.Scan() {
		return false
	}
	var hdr struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(sc.Bytes(), &hdr); err != nil {
		return false
	}
	return hdr.ID == sessionID
}

// slugForCwd renders a working directory the way pi names its per-cwd
// session directory: path separators become hyphens, wrapped in "--".
// Best-effort — locate() falls back to a full walk when the guess misses,
// so leading/trailing-separator quirks across pi versions are harmless.
func slugForCwd(cwd string) string {
	trimmed := strings.Trim(filepath.ToSlash(filepath.Clean(cwd)), "/")
	return "--" + strings.ReplaceAll(trimmed, "/", "-") + "--"
}

// jsonlLine is the subset of a session line the reader inspects.
type jsonlLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp,omitempty"`
	Message   *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message,omitempty"`
}

func parseJSONL(path string) ([]transcript.Turn, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("pi transcript: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	out := make([]transcript.Turn, 0, 32)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var ln jsonlLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			return nil, fmt.Errorf("pi transcript: parse line %d in %s: %w", lineNo, path, err)
		}
		// Only message lines carry conversation content; skip the
		// session header and all control/metadata entry types.
		if ln.Type != "message" || ln.Message == nil {
			continue
		}
		role := normalizeRole(ln.Message.Role)
		if role == "" {
			continue
		}
		text := extractText(ln.Message.Content)
		if text == "" {
			continue
		}
		var ts time.Time
		if ln.Timestamp != "" {
			ts, _ = time.Parse(time.RFC3339, ln.Timestamp)
		}
		out = append(out, transcript.Turn{
			Role:      role,
			Text:      text,
			Timestamp: ts,
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("pi transcript: scan %s: %w", path, err)
	}
	return out, nil
}

// normalizeRole maps pi's roles to the transcript vocabulary. Tool
// results fold into "system", matching how the other readers treat
// non-user/assistant content.
func normalizeRole(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "toolResult", "tool", "system":
		return "system"
	}
	return ""
}

// extractText pulls displayable text out of a message "content" field,
// which pi encodes either as a bare JSON string or as an array of typed
// blocks. Only "text" blocks contribute; tool-call / image blocks are
// dropped. Multiple text blocks are joined with a blank line.
func extractText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	switch content[0] {
	case '"':
		var s string
		if err := json.Unmarshal(content, &s); err == nil {
			return s
		}
		return ""
	case '[':
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(content, &blocks); err != nil {
			return ""
		}
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}
