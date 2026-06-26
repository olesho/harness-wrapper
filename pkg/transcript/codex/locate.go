package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// sessionMetaPayload is the payload of a rollout's leading session_meta
// envelope. Codex 0.142 stopped printing the "codex resume <uuid>" hint to
// the screen, so this on-disk record — written at session start — is the
// version-independent anchor for recovering the session id.
type sessionMetaPayload struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
}

// LocateLatestSession returns the session UUID of the most recently modified
// rollout whose session_meta cwd matches workingDir. It is the disk-based
// fallback used when the screen-scrape session-id extractor finds nothing
// (e.g. Codex 0.142+, which no longer renders the resume hint). Returns
// ("", false) when workingDir is empty or no rollout matches.
//
// Paths are compared after canonicalDir, which resolves symlinks before
// cleaning, so a symlinked workingDir still matches the recorded cwd. This is
// not cosmetic: the wrapper hands us os.Getwd() (e.g. /tmp/x) while Codex
// records its resolved cwd (/private/tmp/x on macOS, where /tmp→/private/tmp),
// and a plain filepath.Clean leaves those two spellings unequal — silently
// defeating the lookup for any run under a symlinked path. Empty, malformed,
// or non-session_meta rollouts are skipped rather than treated as errors —
// a single bad file must never starve the lookup.
func (r *Reader) LocateLatestSession(workingDir string) (string, bool) {
	if workingDir == "" {
		return "", false
	}
	want := canonicalDir(workingDir)

	root, err := r.sessionsRoot()
	if err != nil {
		return "", false
	}

	var (
		bestID  string
		bestMod int64
		found   bool
	)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // skip unreadable subtrees rather than aborting the walk
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		meta, ok := readSessionMeta(path)
		if !ok || meta.SessionID == "" || canonicalDir(meta.Cwd) != want {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if mod := info.ModTime().UnixNano(); !found || mod > bestMod {
			bestID, bestMod, found = meta.SessionID, mod, true
		}
		return nil
	})

	return bestID, found
}

// canonicalDir returns a directory path suitable for comparing two spellings
// that may name the same location. It resolves symlinks (so /tmp/x and the
// macOS-resolved /private/tmp/x compare equal) and, since EvalSymlinks already
// returns a cleaned path, that result is used directly. EvalSymlinks requires
// the path to exist; when it errors (path gone, permission, etc.) we fall back
// to a plain Clean so a still-valid lexical match is not lost.
func canonicalDir(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// readSessionMeta reads only the first line of a rollout and, if it is a
// session_meta envelope, returns its payload. Returns ok=false for empty,
// unreadable, malformed, or non-session_meta files.
func readSessionMeta(path string) (sessionMetaPayload, bool) {
	f, err := os.Open(path) //nolint:gosec // path is under the codex sessions root
	if err != nil {
		return sessionMetaPayload{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// session_meta carries the full base_instructions blob, so the first line
	// can be large; lift the scanner's token limit well above the default 64KiB.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	if !sc.Scan() {
		return sessionMetaPayload{}, false
	}

	var env Envelope
	if err := json.Unmarshal(sc.Bytes(), &env); err != nil || env.Type != "session_meta" {
		return sessionMetaPayload{}, false
	}
	var meta sessionMetaPayload
	if err := json.Unmarshal(env.Payload, &meta); err != nil {
		return sessionMetaPayload{}, false
	}
	return meta, true
}
