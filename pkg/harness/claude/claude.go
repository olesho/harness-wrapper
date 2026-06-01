// Package claude implements the harness.Profile for the Claude Code CLI.
//
// P1 provides the resume capabilities: line-based session-id extraction (from
// Claude's stream-json "system:init" event) and the --resume arg prefix. Later
// phases add the StreamParser/HookProvider/TranscriptReader capabilities.
//
// Importing this package registers the "claude" profile (see init); callers
// that want it available via harness.For should blank-import it (or
// pkg/harness/all, which imports every built-in).
package claude

import (
	"encoding/json"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

// Profile is the Claude Code harness profile.
type Profile struct{}

// Name implements harness.Profile.
func (Profile) Name() string { return "claude" }

// Resolve implements harness.Profile. Claude's capabilities are all statically
// available (documented + firm — no runtime probe needed), so it unconditionally
// populates SessionID + Resume + Stream + Hooks. ctx is unused for these.
func (Profile) Resolve(_ harness.ResolveContext) harness.ResolvedProfile {
	return harness.ResolvedProfile{
		SessionID: sessionIDExtractor{},
		Resume:    resumer{},
		Stream:    streamParser{},
		Hooks:     hookProvider{},
	}
}

// sessionIDExtractor parses Claude's stream-json "system:init" event.
type sessionIDExtractor struct{}

// systemInit is the minimal shape of Claude's stream-json system/init line,
// which is the only place the assigned session_id appears.
type systemInit struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

// ExtractSessionID returns the session_id when line is a
// {"type":"system","subtype":"init","session_id":...} event, else ("", false).
// Non-JSON / non-init lines (including ANSI-polluted ones) fail json.Unmarshal
// and are skipped, so this is safe to call on every raw output line.
func (sessionIDExtractor) ExtractSessionID(line string) (string, bool) {
	var ev systemInit
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return "", false
	}
	if ev.Type != "system" || ev.Subtype != "init" || ev.SessionID == "" {
		return "", false
	}
	return ev.SessionID, true
}

// resumer produces Claude's --resume prefix.
type resumer struct{}

// ResumeArgs returns the resume-specific arg prefix for sessionID; the caller
// appends its own policy flags (-p, output format, budget, prompt). Returns nil
// for an empty id (cold start).
func (resumer) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--resume", "--session-id", sessionID}
}

func init() {
	harness.Register("claude", Profile{})
}
