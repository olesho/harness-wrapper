package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// Per-tool hooks (harness.ToolHookProvider). Claude fires PreToolUse before
// every tool, PostToolUse after one succeeds and PostToolUseFailure after one
// fails, each with the tool's name, its tool_use id and its input; the
// outcome hooks add the tool's response or its error. Measured on claude
// 2.1.281, whose hook payloads also carry agent_id when the tool runs inside a
// subagent.
const (
	argPreToolUse         = harness.HookArgPreToolUse
	argPostToolUse        = harness.HookArgPostToolUse
	argPostToolUseFailure = harness.HookArgPostToolUseFailure
)

// ToolHookEntries returns claude's per-tool hooks, for every tool (no
// matcher). They are not in HookSpec: each runs a hook subprocess on every
// tool call, and Run's own acquisition never needs them.
func (hookProvider) ToolHookEntries() []harness.HookEntry {
	return []harness.HookEntry{
		{NativeEvent: "PreToolUse", Arg: argPreToolUse},
		{NativeEvent: "PostToolUse", Arg: argPostToolUse},
		{NativeEvent: "PostToolUseFailure", Arg: argPostToolUseFailure},
	}
}

// toolHookPayload is what the per-tool hooks add to the shared payload.
type toolHookPayload struct {
	ToolName     string          `json:"tool_name"`
	ToolUseID    string          `json:"tool_use_id"`
	ToolInput    json.RawMessage `json:"tool_input"`
	ToolResponse json.RawMessage `json:"tool_response"` // PostToolUse
	Error        string          `json:"error"`         // PostToolUseFailure
	IsInterrupt  bool            `json:"is_interrupt"`  // PostToolUseFailure
	AgentID      string          `json:"agent_id"`      // set inside a subagent
}

// readToolHook turns one per-tool hook into its single event, stamped when
// the hook fired:
//   - pre-tool-use → a tool_use carrying the tool's input;
//   - post-tool-use → a tool_result carrying the tool's response as text;
//   - post-tool-use-failure → a tool_result carrying the failure, prefixed
//     "interrupted: " when claude reports the tool was interrupted.
//
// Input and output are bounded by harness.MaxToolHookBytes. The event's
// Source is transcript.SourceHook and its NativeID "hook:<arg>:<tool_use_id>",
// so a finish and a failure never collapse into each other or into the
// stream's or the file's copy of the call. A tool run inside a subagent is
// tagged with the subagent's session under the parent's, as the subagent's
// transcript is.
func readToolHook(event string, p claudeHookPayload, stdin []byte) ([]transcript.ParsedEvent, error) {
	var t toolHookPayload
	if err := json.Unmarshal(stdin, &t); err != nil {
		return nil, fmt.Errorf("claude hook %q: parse tool payload: %w", event, err)
	}
	if t.ToolName == "" {
		return nil, fmt.Errorf("claude hook %q: empty tool_name", event)
	}
	e := transcript.Event{
		Timestamp: time.Now().UTC(),
		ToolName:  t.ToolName,
		ToolUseID: t.ToolUseID,
		Source:    transcript.SourceHook,
	}
	if t.ToolUseID != "" {
		e.NativeID = "hook:" + event + ":" + t.ToolUseID
	}
	switch event {
	case argPreToolUse:
		e.Role, e.Type = transcript.RoleAssistant, transcript.EventToolUse
		e.ToolInput = boundToolInput(t.ToolInput)
	case argPostToolUse:
		e.Role, e.Type = transcript.RoleTool, transcript.EventToolResult
		e.Output = boundToolText(toolResponseText(t.ToolResponse))
	default: // argPostToolUseFailure
		e.Role, e.Type = transcript.RoleTool, transcript.EventToolResult
		msg := t.Error
		if t.IsInterrupt {
			msg = "interrupted: " + msg
		}
		e.Output = boundToolText(msg)
	}
	pe := transcript.ParsedEvent{HarnessSessionID: p.SessionID, Event: e}
	if t.AgentID != "" {
		if !validSubagentID(t.AgentID) {
			return nil, fmt.Errorf("claude hook %q: invalid subagent id %q", event, t.AgentID)
		}
		pe.HarnessSessionID, pe.ParentSessionID = t.AgentID, p.SessionID
	}
	return []transcript.ParsedEvent{pe}, nil
}

// toolResponseText renders a tool's response as text: a JSON string as its
// value, anything else as its compact JSON.
func toolResponseText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

// boundToolText cuts s to harness.MaxToolHookBytes at a rune boundary and
// marks the cut with harness.ToolHookTruncated.
func boundToolText(s string) string {
	if len(s) <= harness.MaxToolHookBytes {
		return s
	}
	cut := harness.MaxToolHookBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + harness.ToolHookTruncated
}

// boundToolInput returns the tool's input as compact JSON, or — over
// harness.MaxToolHookBytes — a JSON string holding the cut JSON text, so the
// truncation is visible in the value's type as well as its end.
func boundToolInput(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var buf bytes.Buffer
	text := raw
	if err := json.Compact(&buf, raw); err == nil {
		text = buf.Bytes()
	}
	if len(text) <= harness.MaxToolHookBytes && json.Valid(text) {
		return append(json.RawMessage(nil), text...)
	}
	cut, err := json.Marshal(boundToolText(string(text)))
	if err != nil {
		return nil
	}
	return cut
}

var _ harness.ToolHookProvider = hookProvider{}
