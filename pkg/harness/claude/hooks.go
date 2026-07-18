// Claude Code hook-driven acquisition: the HookProvider capability. Claude's
// hook payload HANDS OVER the transcript_path + session_id (no path
// reconstruction — it sidesteps the cwd-encoding bug entirely), and the
// on-disk file is higher fidelity than the stream-json stdout (full tool I/O +
// subagent trees). The native event → loom-arg mapping and the stdin payload
// shapes are ported from tysonthomas9's proven hook implementation.
package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// hookProvider implements harness.HookProvider for Claude Code.
type hookProvider struct{}

// Claude hook subcommand args — the `loom hooks claude <arg>` token templated
// into the config and dispatched on in the fired subprocess. One per native
// Claude hook event loom manages (+ the yield guard).
const (
	argSessionStart = "session-start"
	argUserPrompt   = "user-prompt-submit"
	argStop         = "stop"
	argSessionEnd   = "session-end"
	argPreTask      = "pre-task"
	argPostTask     = "post-task"
	argYieldGuard   = "yield-guard"
)

// hookOwner marks loom-managed entries in settings.json for idempotent
// identify/upgrade/remove (review #5).
const hookOwner = "loom"

// HookSpec returns the static spec the orchestrator ensures in the per-worktree
// .claude/settings.json. ConfigPath is worktree-relative; the orchestrator
// resolves it against the run's working dir.
func (hookProvider) HookSpec() *harness.HookSpec {
	return &harness.HookSpec{
		ConfigPath: filepath.Join(".claude", "settings.json"),
		Owner:      hookOwner,
		Events: []harness.HookEntry{
			{NativeEvent: "SessionStart", Arg: argSessionStart},
			{NativeEvent: "UserPromptSubmit", Arg: argUserPrompt},
			{NativeEvent: "Stop", Arg: argStop},
			{NativeEvent: "SessionEnd", Arg: argSessionEnd},
			{NativeEvent: "PreToolUse", Matcher: "Task", Arg: argPreTask},
			{NativeEvent: "PostToolUse", Matcher: "Task", Arg: argPostTask},
		},
		Yield: &harness.HookEntry{NativeEvent: "PreToolUse", Arg: argYieldGuard},
	}
}

// claudeHookPayload is the subset of Claude's hook stdin shared across events:
// session_id + the handed-over transcript_path are present on every hook.
type claudeHookPayload struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
}

// ParseHookPayload turns one fired Claude hook into canonical events:
//   - stop / session-end (the file-bearing phases): READ the handed-over
//     transcript file (validated) → the full parent conversation. Re-reading
//     the whole file on each Stop is intentional; the consumer dedups by
//     Event.ID(), so replays collapse.
//   - session-start / user-prompt-submit: emit a session marker carrying the
//     session id — the early "hooks live" signal + P4 lock persistence — without
//     reading the (possibly incomplete) file.
//   - pre-task / post-task / yield-guard: no parent transcript here (subagent
//     nesting + yield are handled by later steps), so return nil.
func (hookProvider) ParseHookPayload(ctx harness.HookContext, event string, stdin []byte) ([]transcript.ParsedEvent, error) {
	var p claudeHookPayload
	if err := json.Unmarshal(stdin, &p); err != nil {
		return nil, fmt.Errorf("claude hook %q: parse payload: %w", event, err)
	}
	if p.SessionID == "" {
		return nil, fmt.Errorf("claude hook %q: empty session_id", event)
	}
	switch event {
	case argStop, argSessionEnd:
		return readParentTranscript(ctx, p)
	case argSessionStart, argUserPrompt:
		return []transcript.ParsedEvent{sessionMarker(p.SessionID)}, nil
	case argPostTask:
		return readSubagentTranscript(ctx, p, stdin)
	case argPreTask, argYieldGuard:
		// PreToolUse[Task] fires BEFORE the subagent runs (no transcript yet);
		// yield-guard is a control hook handled by HandleHookEvent, not a
		// transcript-bearing event.
		return nil, nil
	default:
		return nil, fmt.Errorf("claude hook: unknown event %q", event)
	}
}

// readSubagentTranscript handles PostToolUse[Task]: when the Task spawned a
// subagent (tool_response.agentId present), Claude has written its transcript at
// <parent_dir>/subagents/agent-<agentId>.jsonl. It reads that file and tags each
// event with the subagent's own session id + the ParentSessionID, so the Runs
// tab can nest the subagent under its parent. Subagent capture is best-effort:
// an absent file (timing) yields nil, not an error.
func readSubagentTranscript(ctx harness.HookContext, p claudeHookPayload, stdin []byte) ([]transcript.ParsedEvent, error) {
	var pt struct {
		ToolResponse struct {
			AgentID string `json:"agentId"`
		} `json:"tool_response"`
	}
	if err := json.Unmarshal(stdin, &pt); err != nil {
		return nil, fmt.Errorf("claude hook post-task: parse tool_response: %w", err)
	}
	agentID := pt.ToolResponse.AgentID
	if agentID == "" {
		return nil, nil // a Task result that did not spawn a tracked subagent
	}
	if !validSubagentID(agentID) {
		return nil, fmt.Errorf("claude hook post-task: invalid subagent id %q", agentID)
	}
	// The payload's transcript_path is the PARENT session's file (basename ==
	// parent session id), so the standard validation applies; the subagent path
	// is then derived from that validated directory.
	if err := validateTranscriptPath(ctx, p.SessionID, p.TranscriptPath); err != nil {
		return nil, err
	}
	subPath := filepath.Join(filepath.Dir(p.TranscriptPath), "subagents", "agent-"+agentID+".jsonl")
	data, err := os.ReadFile(subPath) //nolint:gosec // derived under the validated parent transcript dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // subagent transcript not present yet — best-effort
		}
		return nil, fmt.Errorf("claude hook post-task: read subagent %s: %w", subPath, err)
	}
	events, err := claudecode.Events(data)
	if err != nil {
		return nil, fmt.Errorf("claude hook post-task: parse subagent: %w", err)
	}
	out := make([]transcript.ParsedEvent, len(events))
	for i, e := range events {
		out[i] = transcript.ParsedEvent{
			HarnessSessionID: agentID,     // the subagent's own native session
			ParentSessionID:  p.SessionID, // nested under the parent
			Event:            e,
		}
	}
	return out, nil
}

// validSubagentID guards the agentId interpolated into the subagent file path
// against traversal — it must be a simple identifier (no '/', '.', '..').
func validSubagentID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// readParentTranscript validates and reads the handed-over transcript file into
// session-tagged parent events.
func readParentTranscript(ctx harness.HookContext, p claudeHookPayload) ([]transcript.ParsedEvent, error) {
	if err := validateTranscriptPath(ctx, p.SessionID, p.TranscriptPath); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p.TranscriptPath) //nolint:gosec // validated under the harness transcript root
	if err != nil {
		return nil, fmt.Errorf("claude hook: read transcript %s: %w", p.TranscriptPath, err)
	}
	events, err := claudecode.Events(data)
	if err != nil {
		return nil, fmt.Errorf("claude hook: parse transcript: %w", err)
	}
	out := make([]transcript.ParsedEvent, len(events))
	for i, e := range events {
		out[i] = transcript.ParsedEvent{HarnessSessionID: p.SessionID, Event: e}
	}
	return out, nil
}

// sessionMarker is a non-conversation session event carrying the session id. The
// id is source-prefixed (no shared native id across schemas) and the authority
// filter admits session kinds from any source.
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

// validateTranscriptPath ensures the handed-over path is a Claude session
// transcript for THIS session under the harness transcript root, defending
// against a malicious/garbled payload pointing elsewhere (review #1). It checks
// the transcript ROOT (~/.claude/projects or ConfigDir/projects) plus a
// session-id basename match — deliberately NOT the exact encoded-cwd subdir,
// since Claude's cwd encoding can differ from a naive reconstruction and the
// point of hooks is to avoid path reconstruction.
func validateTranscriptPath(ctx harness.HookContext, sessionID, tpath string) error {
	if tpath == "" {
		return fmt.Errorf("claude hook: empty transcript_path")
	}
	configDir := ctx.ConfigDir
	if configDir == "" {
		configDir = filepath.Join(ctx.Home, ".claude")
	}
	root := filepath.Join(configDir, "projects")
	clean := filepath.Clean(tpath)
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return fmt.Errorf("claude hook: transcript_path %q not under transcript root %q", clean, root)
	}
	if base := strings.TrimSuffix(filepath.Base(clean), ".jsonl"); base != sessionID {
		return fmt.Errorf("claude hook: transcript_path basename %q != session id %q", base, sessionID)
	}
	return nil
}
