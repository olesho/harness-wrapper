package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// sessionMetaPayload is the payload of a rollout's leading session_meta
// envelope. Codex 0.142 stopped printing the "codex resume <uuid>" hint to
// the screen, so this on-disk record — written at session start — is the
// version-independent anchor for recovering the session id.
type sessionMetaPayload struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	// Timestamp is when the session started. The envelope's own timestamp
	// is when the line was written, which Codex 0.142 did only once the
	// first prompt arrived.
	Timestamp string `json:"timestamp"`
	// Source is who started the session: a launcher's name ("cli", "exec",
	// "vscode"), or {"subagent": …} for a session Codex started for itself.
	Source json.RawMessage `json:"source"`
}

// started returns when the session started: the payload's timestamp, or the
// envelope's — written later — when the payload has none.
func (m sessionMetaPayload) started(envelope string) (time.Time, bool) {
	for _, ts := range []string{m.Timestamp, envelope} {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// subagent reports whether Codex started the session for itself — a
// /review, an agent another session spawned — which codex-rs records as a
// source of {"subagent": …}. Such a session is its parent launch's child,
// never a launch of its own.
func (m sessionMetaPayload) subagent() bool {
	var src map[string]json.RawMessage
	if json.Unmarshal(m.Source, &src) != nil {
		return false
	}
	_, ok := src["subagent"]
	return ok
}

// LocateLaunchSession returns the id of the Codex session a launch in
// workingDir at launchedAt started: the one rollout whose session_meta names
// workingDir and a session start no earlier than the launch, leaving out the
// sessions Codex starts for itself (subagent). It is the disk-based fallback
// used when the screen-scrape session-id extractor finds nothing (e.g. Codex
// 0.142+, which no longer renders the resume hint).
//
// It returns ("", false) when there is no such rollout — the session may not
// have written one yet — and when there is more than one. Nothing on disk
// ties a rollout to the process that wrote it, so two sessions started in
// one directory after the launch cannot be told apart, and choosing one (the
// newest, as LocateLatestSession does) can hand a conversation another's
// transcript. The start bound is what rules out every session that directory
// saw before the launch; a zero launchedAt sets none, so any other session
// recorded for workingDir makes the answer ambiguous.
//
// The bound is compared at whole seconds, so a start recorded with a coarser
// clock than the launch's still counts, and Codex's <YYYY>/<MM>/<DD> day
// directories from well before the launch are not read at all. Paths are
// compared as in LocateLatestSession; unreadable, empty, malformed and
// non-session_meta rollouts are skipped.
func (r *Reader) LocateLaunchSession(workingDir string, launchedAt time.Time) (string, bool) {
	if workingDir == "" {
		return "", false
	}
	want := canonicalDir(workingDir)

	root, err := r.sessionsRoot()
	if err != nil {
		return "", false
	}

	since := launchedAt.Truncate(time.Second)
	// A day directory is named for the session's LOCAL start date, which is
	// at most a day either side of its UTC date; one more day of margin.
	year, month, day := launchedAt.UTC().Date()
	firstDay := time.Date(year, month, day-2, 0, 0, 0, 0, time.UTC)

	var (
		found     string
		ambiguous bool
	)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // skip unreadable subtrees rather than aborting the walk
		}
		if d.IsDir() {
			if date, ok := dayDir(root, path); ok && !launchedAt.IsZero() && date.Before(firstDay) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		meta, envelopeTS, ok := readSessionMetaAt(path)
		if !ok || meta.SessionID == "" || meta.subagent() || canonicalDir(meta.Cwd) != want {
			return nil
		}
		if !launchedAt.IsZero() {
			start, ok := meta.started(envelopeTS)
			if !ok || start.Before(since) {
				return nil
			}
		}
		switch {
		case found == "":
			found = meta.SessionID
		case meta.SessionID != found:
			ambiguous = true
			return filepath.SkipAll
		}
		return nil
	})

	if ambiguous || found == "" {
		return "", false
	}
	return found, true
}

// dayDir parses path, a directory under root, as a Codex day directory,
// <YYYY>/<MM>/<DD>.
func dayDir(root, path string) (time.Time, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return time.Time{}, false
	}
	day, err := time.Parse("2006/01/02", filepath.ToSlash(rel))
	return day, err == nil
}

// LocateLatestSession returns the session UUID of the most recently modified
// rollout whose session_meta cwd matches workingDir. Returns ("", false) when
// workingDir is empty or no rollout matches.
//
// The newest rollout in a directory is not necessarily a given launch's: an
// earlier session there is the newest until the launch's own session writes
// its rollout, and a session started there later by someone else is the
// newest after. To find the session a launch started, use
// LocateLaunchSession, which refuses to guess.
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
	meta, _, ok := readSessionMetaAt(path)
	return meta, ok
}

// readSessionMetaAt is readSessionMeta that also returns the envelope's
// timestamp.
func readSessionMetaAt(path string) (sessionMetaPayload, string, bool) {
	f, err := os.Open(path) //nolint:gosec // path is under the codex sessions root
	if err != nil {
		return sessionMetaPayload{}, "", false
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	// session_meta carries the full base_instructions blob, so the first line
	// can be large; lift the scanner's token limit well above the default 64KiB.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	if !sc.Scan() {
		return sessionMetaPayload{}, "", false
	}

	var env Envelope
	if err := json.Unmarshal(sc.Bytes(), &env); err != nil || env.Type != "session_meta" {
		return sessionMetaPayload{}, "", false
	}
	var meta sessionMetaPayload
	if err := json.Unmarshal(env.Payload, &meta); err != nil {
		return sessionMetaPayload{}, "", false
	}
	return meta, env.Timestamp, true
}
