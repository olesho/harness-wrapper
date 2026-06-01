// Package opencode implements the harness.Profile for the OpenCode CLI.
//
// SCOPE (resume + session-id only): OpenCode has NO shell hooks (its callbacks
// are in-process JS/TS plugins), so no HookProvider is registered — correct, not
// a gap. Its `run --format json` stream schema and `session export` JSON are
// documented only in third-party sources and have a known export round-trip bug
// (anomalyco/opencode #21941), and cannot be validated against a real CLI here,
// so the StreamParser and Exporter are DEFERRED rather than guessed (a wrong
// field name would silently capture nothing). Added once validatable.
//
// Registered now: Resumer (`--session <id>`, documented) and a tolerant
// SessionIDExtractor (the stream events carry a sessionID), both low-risk.
//
// Importing this package registers the "opencode" profile (see init).
package opencode

import (
	"encoding/json"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

// Profile is the OpenCode CLI harness profile.
type Profile struct{}

// Name implements harness.Profile.
func (Profile) Name() string { return "opencode" }

// Resolve populates Resume + SessionID (see package doc; Hooks/Stream/Export are
// deferred). ctx is unused.
func (Profile) Resolve(_ harness.ResolveContext) harness.ResolvedProfile {
	return harness.ResolvedProfile{
		Resume:    resumer{},
		SessionID: sessionIDExtractor{},
	}
}

// resumer produces OpenCode's resume prefix: `--session <id>` (documented).
type resumer struct{}

func (resumer) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--session", sessionID}
}

// sessionIDExtractor recovers the session id from a line of `opencode run
// --format json` output, where events carry a session id. Tolerant: it tries
// the documented camelCase `sessionID` then the snake_case fallback, and skips
// any non-JSON / non-matching line — so it is safe to call on every raw output
// line even though the exact stream schema is unverified.
type sessionIDExtractor struct{}

func (sessionIDExtractor) ExtractSessionID(line string) (string, bool) {
	var ev struct {
		SessionID  string `json:"sessionID"`
		SessionID2 string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return "", false
	}
	if ev.SessionID != "" {
		return ev.SessionID, true
	}
	if ev.SessionID2 != "" {
		return ev.SessionID2, true
	}
	return "", false
}

func init() {
	harness.Register("opencode", Profile{})
}
