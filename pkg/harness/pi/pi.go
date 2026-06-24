// Package pi implements the harness.Profile for the pi coding agent
// (@earendil-works/pi-coding-agent, binary "pi" — github.com/earendil-works/pi).
//
// Scope (session-id + resume + stream): pi's headless mode (`pi -p --mode json`)
// emits a JSON event stream whose FIRST stdout line is the session header
//
//	{"type":"session","version":3,"id":"<uuid>","timestamp":"…","cwd":"…"}
//
// so SessionID parses the assigned id from that line, Resume produces the
// `--session <id>` prefix that re-opens it, and Stream parses the per-message
// events into canonical transcript events (see parse_stream.go). All three are
// validated against pi 0.76.0 (cerebras/gpt-oss-120b live capture). This is
// DISTINCT from pkg/turns/harness/pi (the interactive TUI turn adapter).
//
// Hooks is deliberately NOT implemented: pi has no documented shell-hook
// contract (it uses an extension model), so no HookProvider is registered — the
// orchestrator never installs pi hooks on a guess.
//
// Importing this package registers the "pi" profile (see init); blank-import
// pkg/harness/all to register every built-in.
package pi

import (
	"encoding/json"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

// Profile is the pi coding-agent harness profile.
type Profile struct{}

// Name implements harness.Profile.
func (Profile) Name() string { return "pi" }

// Resolve populates the session-id + resume + stream capabilities. All are
// statically available from pi's `--mode json` output and resume flags (no
// runtime probe needed), so Resolve unconditionally returns them. ctx is unused.
func (Profile) Resolve(_ harness.ResolveContext) harness.ResolvedProfile {
	return harness.ResolvedProfile{
		SessionID: sessionIDExtractor{},
		Resume:    resumer{},
		Stream:    streamParser{},
	}
}

// sessionIDExtractor parses pi's `--mode json` session-header line — the only
// place the assigned session id is surfaced on the headless stream.
type sessionIDExtractor struct{}

// sessionHeader is the minimal shape of pi's first `--mode json` line.
type sessionHeader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ExtractSessionID returns the id when line is pi's session header
// ({"type":"session",…,"id":…}), else ("", false). Non-JSON / non-session lines
// (including ANSI-polluted ones) fail json.Unmarshal or the type guard and are
// skipped, so this is safe to call on every raw output line.
func (sessionIDExtractor) ExtractSessionID(line string) (string, bool) {
	var h sessionHeader
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		return "", false
	}
	if h.Type != "session" || h.ID == "" {
		return "", false
	}
	return h.ID, true
}

// resumer produces pi's resume prefix.
type resumer struct{}

// ResumeArgs returns pi's resume-by-id prefix for sessionID; the caller appends
// its own policy flags (-p, --mode json, prompt). Returns nil for an empty id
// (cold start).
//
// A known session resumes by id with `--session <id>` ("Use a specific session
// file or partial session ID"). The sibling `--session-id <id>` form ("creating
// it if missing") is for assigning/forking an id, so a plain resume uses
// --session, mirroring how the claude profile keeps resume separate from fork.
func (resumer) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--session", sessionID}
}

func init() {
	harness.Register("pi", Profile{})
}
