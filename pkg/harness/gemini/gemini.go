// Package gemini implements the harness.Profile for the Gemini CLI.
//
// VERIFIED capabilities (from the gemini-cli docs): resume by id
// (`gemini --resume <uuid>`) and shell hooks whose stdin payload carries
// session_id + transcript_path on every event, configured in a settings.json
// with the SAME two-level shape as Claude. Gemini's live stream-json schema is
// NOT verifiable from the docs (only proposed in issues), so this profile does
// NOT register a StreamParser/SessionIDExtractor — it would be a guess. Gemini
// is therefore hooks-only here; the session id is recovered from the hook
// payloads. A stream parser can be added once the schema is confirmed.
//
// Importing this package registers the "gemini" profile (see init).
package gemini

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	tgemini "github.com/olesho/harness-wrapper/pkg/transcript/gemini"
)

// Profile is the Gemini CLI harness profile.
type Profile struct{}

// Name implements harness.Profile.
func (Profile) Name() string { return "gemini" }

// Resolve populates the verified capabilities (Resume + Hooks). Stream/SessionID
// are intentionally nil (unverified stream-json schema — see package doc).
func (Profile) Resolve(_ harness.ResolveContext) harness.ResolvedProfile {
	return harness.ResolvedProfile{
		Resume: resumer{},
		Hooks:  hookProvider{},
	}
}

// resumer produces Gemini's resume prefix: `--resume <id>` (documented).
type resumer struct{}

func (resumer) ResumeArgs(sessionID string) []string {
	if sessionID == "" {
		return nil
	}
	return []string{"--resume", sessionID}
}

// Gemini hook subcommand args, mapped from Gemini's native hook event names.
const (
	argSessionStart = "session-start"
	argStop         = "stop"
	argSessionEnd   = "session-end"
	argYieldGuard   = "yield-guard"
)

const hookOwner = "loom"

// hookProvider implements harness.HookProvider for Gemini.
type hookProvider struct{}

// StaticHookProvider lets the fired hook subprocess obtain the parser without
// running Resolve.
func (Profile) StaticHookProvider() harness.HookProvider { return hookProvider{} }

// HookSpec maps Gemini's native events to loom args. The native names differ
// from Claude's: SessionStart / AfterAgent (turn end) / SessionEnd are the
// transcript/session phases; BeforeTool (all tools) is the yield guard.
func (hookProvider) HookSpec() *harness.HookSpec {
	return &harness.HookSpec{
		ConfigPath: filepath.Join(".gemini", "settings.json"),
		Owner:      hookOwner,
		Events: []harness.HookEntry{
			{NativeEvent: "SessionStart", Arg: argSessionStart},
			{NativeEvent: "AfterAgent", Arg: argStop},
			{NativeEvent: "SessionEnd", Arg: argSessionEnd},
		},
		Yield: &harness.HookEntry{NativeEvent: "BeforeTool", Arg: argYieldGuard},
	}
}

// geminiHookPayload is the subset of Gemini's hook stdin shared across events
// (the base schema documents session_id + transcript_path on every hook).
type geminiHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// ParseHookPayload turns a fired Gemini hook into canonical events: read the
// handed-over JSONL transcript on the turn/session-end phases; emit a session
// marker on SessionStart; yield-guard is handled by HandleHookEvent.
func (hookProvider) ParseHookPayload(ctx harness.HookContext, event string, stdin []byte) ([]transcript.ParsedEvent, error) {
	var p geminiHookPayload
	if err := json.Unmarshal(stdin, &p); err != nil {
		return nil, fmt.Errorf("gemini hook %q: parse payload: %w", event, err)
	}
	if p.SessionID == "" {
		return nil, fmt.Errorf("gemini hook %q: empty session_id", event)
	}
	switch event {
	case argStop, argSessionEnd:
		return readTranscript(ctx, p)
	case argSessionStart:
		return []transcript.ParsedEvent{sessionMarker(p.SessionID)}, nil
	case argYieldGuard:
		return nil, nil
	default:
		return nil, fmt.Errorf("gemini hook: unknown event %q", event)
	}
}

func readTranscript(ctx harness.HookContext, p geminiHookPayload) ([]transcript.ParsedEvent, error) {
	if err := validateTranscriptPath(ctx, p.TranscriptPath); err != nil {
		return nil, err
	}
	events, err := tgemini.ParseFile(p.TranscriptPath)
	if err != nil {
		return nil, fmt.Errorf("gemini hook: parse transcript: %w", err)
	}
	out := make([]transcript.ParsedEvent, len(events))
	for i, e := range events {
		out[i] = transcript.ParsedEvent{HarnessSessionID: p.SessionID, Event: e}
	}
	return out, nil
}

func sessionMarker(sessionID string) transcript.ParsedEvent {
	return transcript.ParsedEvent{
		HarnessSessionID: sessionID,
		Event: transcript.Event{
			Role:     transcript.RoleSystem,
			Type:     transcript.EventSessionMeta,
			Source:   transcript.SourceFile,
			NativeID: "file:session:" + sessionID,
		},
	}
}

// validateTranscriptPath ensures the handed-over path is under Gemini's config
// root (~/.gemini or ConfigDir), defending against a garbled/hostile payload.
// Gemini names transcripts by a project slug (not <session>.jsonl), so only the
// root + a .jsonl extension are checked.
func validateTranscriptPath(ctx harness.HookContext, tpath string) error {
	if tpath == "" {
		return fmt.Errorf("gemini hook: empty transcript_path")
	}
	root := ctx.ConfigDir
	if root == "" {
		root = filepath.Join(ctx.Home, ".gemini")
	}
	clean := filepath.Clean(tpath)
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return fmt.Errorf("gemini hook: transcript_path %q not under gemini root %q", clean, root)
	}
	if filepath.Ext(clean) != ".jsonl" {
		return fmt.Errorf("gemini hook: transcript_path %q is not a .jsonl transcript", clean)
	}
	return nil
}

// EnsureConfig installs loom's hooks into <worktree>/.gemini/settings.json via
// the shared Claude/Gemini settings.json merge.
func (h hookProvider) EnsureConfig(worktreePath string, loomArgv []string) error {
	spec := h.HookSpec()
	settingsPath := filepath.Join(worktreePath, spec.ConfigPath)
	return harness.EnsureSettingsJSONHooks(settingsPath, spec, loomArgv, "gemini")
}

func init() {
	harness.Register("gemini", Profile{})
}
