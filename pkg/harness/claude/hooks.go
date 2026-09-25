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
	argSessionStart  = "session-start"
	argUserPrompt    = "user-prompt-submit"
	argStop          = "stop"
	argSessionEnd    = "session-end"
	argSubagentStart = harness.HookArgSubagentStart
	argSubagentStop  = harness.HookArgSubagentStop
	argPostTask      = "post-task"
	argYieldGuard    = "yield-guard"
)

// argPreTask is the Task-matched PreToolUse entry an earlier harness-wrapper
// installed; it fired before the subagent existed and spooled nothing, and
// SubagentStart replaced it (ADR-011). A config written then still runs it
// until its next ensure, so it stays accepted, and still spools nothing.
const argPreTask = "pre-task"

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
			{NativeEvent: "SubagentStart", Arg: argSubagentStart},
			{NativeEvent: "SubagentStop", Arg: argSubagentStop},
			// The subagent's transcript again once its tool call returned —
			// the records SubagentStop read, which consumers dedup by event
			// id — kept for the consumers that read subagents through it
			// (ADR-011). For a background subagent it fires at the launch.
			// claude renamed the tool Agent and still matches the old name,
			// Task.
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
//   - subagent-start: the subagent's start marker; subagent-stop: the
//     subagent's transcript, from the path claude hands over, then its stop
//     marker — see readSubagentHook.
//   - post-task: the subagent's transcript, read once its tool call returned
//     (empty for a subagent launched in the background) — see
//     readSubagentTranscript.
//   - pre-task (retired, ADR-011) and yield-guard: nothing (yield is a control
//     hook HandleHookEvent answers itself).
//   - pre-tool-use / post-tool-use / post-tool-use-failure (ToolHookEntries,
//     installed only by a consumer that asks for them): one event per tool
//     call — see readToolHook.
func (hookProvider) ParseHookPayload(ctx harness.HookContext, event string, stdin []byte) ([]transcript.ParsedEvent, error) {
	var p claudeHookPayload
	if err := json.Unmarshal(stdin, &p); err != nil {
		return nil, fmt.Errorf("claude hook %q: parse payload: %w", event, err)
	}
	if p.SessionID == "" {
		return nil, fmt.Errorf("claude hook %q: empty session_id", event)
	}
	// claude builds transcript_path from its config root VERBATIM, so under a
	// relative CLAUDE_CONFIG_DIR the path it hands over is relative to its own
	// cwd — the run's working dir. Anchor it there before validating or reading
	// it, rather than letting this subprocess's cwd decide.
	p.TranscriptPath = transcript.ResolveHarnessPath(p.TranscriptPath, ctx.Cwd)
	switch event {
	case argStop, argSessionEnd:
		return readParentTranscript(ctx, p)
	case argSessionStart, argUserPrompt:
		return []transcript.ParsedEvent{sessionMarker(p.SessionID)}, nil
	case argSubagentStart, argSubagentStop:
		return readSubagentHook(ctx, event, p, stdin)
	case argPostTask:
		return readSubagentTranscript(ctx, p, stdin)
	case argPreToolUse, argPostToolUse, argPostToolUseFailure:
		return readToolHook(event, p, stdin)
	case argPreTask, argYieldGuard:
		// pre-task fired before the subagent existed (no transcript yet);
		// yield-guard is a control hook handled by HandleHookEvent, not a
		// transcript-bearing event.
		return nil, nil
	default:
		return nil, fmt.Errorf("claude hook: unknown event %q", event)
	}
}

// readSubagentTranscript handles PostToolUse[Task]: when the Task spawned a
// subagent (tool_response.agentId present), Claude has written its transcript
// under a per-session sidecar dir named after the session file — see
// subagentTranscriptPaths for the layouts tried. It reads that file and tags each
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
	var data []byte
	var found bool
	for _, subPath := range subagentTranscriptPaths(p.TranscriptPath, agentID) {
		b, err := os.ReadFile(subPath) //nolint:gosec // derived under the validated parent transcript's session dir
		if err != nil {
			if os.IsNotExist(err) {
				continue // try the next layout
			}
			return nil, fmt.Errorf("claude hook post-task: read subagent %s: %w", subPath, err)
		}
		data, found = b, true
		break
	}
	if !found {
		return nil, nil // subagent transcript not present yet — best-effort
	}
	out, err := subagentEvents(data, agentID, p.SessionID)
	if err != nil {
		return nil, fmt.Errorf("claude hook post-task: %w", err)
	}
	return out, nil
}

// subagentEvents parses a subagent's transcript and tags each event with the
// subagent's own session under its parent's, so the Runs tab nests it.
func subagentEvents(data []byte, agentID, parentSessionID string) ([]transcript.ParsedEvent, error) {
	events, err := claudecode.Events(data)
	if err != nil {
		return nil, fmt.Errorf("parse subagent: %w", err)
	}
	out := make([]transcript.ParsedEvent, len(events))
	for i, e := range events {
		out[i] = transcript.ParsedEvent{
			HarnessSessionID: agentID,         // the subagent's own native session
			ParentSessionID:  parentSessionID, // nested under the parent
			Event:            e,
		}
	}
	return out, nil
}

// subagentTranscriptPaths returns the candidate on-disk locations for a
// spawned subagent transcript, most-current first. Claude 2.1.261 writes it
// under a per-session sidecar dir named after the session file:
//
//	<projects>/<encoded-cwd>/<session-uuid>/subagents/agent-<id>.jsonl
//
// (measured 131/131 at that depth, 0 at the legacy sibling depth). The second
// candidate is that legacy sibling-of-the-file location, kept so an older
// Claude still parses.
//
// Derived from the handed-over parent transcript path, never reconstructed
// from the session id — TrimSuffix keeps working if the basename convention
// changes, and it cannot escape the already-validated transcript root.
func subagentTranscriptPaths(parentTranscript, agentID string) []string {
	name := "agent-" + agentID + ".jsonl"
	return []string{
		filepath.Join(strings.TrimSuffix(parentTranscript, ".jsonl"), "subagents", name),
		filepath.Join(filepath.Dir(parentTranscript), "subagents", name),
	}
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
	root := transcriptRoot(ctx)
	clean := filepath.Clean(tpath)
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return fmt.Errorf("claude hook: transcript_path %q not under transcript root %q", clean, root)
	}
	if base := strings.TrimSuffix(filepath.Base(clean), ".jsonl"); base != sessionID {
		return fmt.Errorf("claude hook: transcript_path basename %q != session id %q", base, sessionID)
	}
	return nil
}
