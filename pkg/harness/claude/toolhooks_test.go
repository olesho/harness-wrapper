package claude

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// toolPayload renders a per-tool hook's stdin the way claude 2.1.281 sends
// it: the shared fields plus the tool's.
func toolPayload(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	p := map[string]any{
		"session_id": "sess-1", "transcript_path": "/home/u/.claude/projects/p/sess-1.jsonl",
		"cwd": "/wt", "permission_mode": "bypassPermissions",
	}
	for k, v := range fields {
		p[k] = v
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseTool(t *testing.T, event string, stdin []byte) transcript.ParsedEvent {
	t.Helper()
	evs, err := hookProvider{}.ParseHookPayload(harness.HookContext{Home: t.TempDir(), Cwd: "/wt"}, event, stdin)
	if err != nil {
		t.Fatalf("ParseHookPayload(%s): %v", event, err)
	}
	if len(evs) != 1 {
		t.Fatalf("ParseHookPayload(%s) = %d events, want 1: %+v", event, len(evs), evs)
	}
	return evs[0]
}

func TestToolHookEntries(t *testing.T) {
	var hp harness.HookProvider = hookProvider{}
	th, ok := hp.(harness.ToolHookProvider)
	if !ok {
		t.Fatal("claude's hook provider is not a ToolHookProvider")
	}
	want := map[string]string{
		harness.HookArgPreToolUse:         "PreToolUse",
		harness.HookArgPostToolUse:        "PostToolUse",
		harness.HookArgPostToolUseFailure: "PostToolUseFailure",
	}
	got := th.ToolHookEntries()
	if len(got) != len(want) {
		t.Fatalf("ToolHookEntries = %+v, want %d entries", got, len(want))
	}
	for _, e := range got {
		if want[e.Arg] != e.NativeEvent || e.Matcher != "" {
			t.Errorf("entry %+v: want native %q and no matcher (every tool)", e, want[e.Arg])
		}
	}
	// The default spec — what Run and loom install — is unchanged: per-tool
	// hooks are opt-in.
	for _, e := range hp.HookSpec().Events {
		if _, ok := want[e.Arg]; ok {
			t.Errorf("HookSpec carries per-tool entry %+v; it must stay opt-in", e)
		}
	}
}

func TestToolHookPreToolUse(t *testing.T) {
	before := time.Now().Add(-time.Second)
	pe := parseTool(t, harness.HookArgPreToolUse, toolPayload(t, map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "toolu_1",
		"tool_input": map[string]any{"command": "echo hi", "description": "say hi"},
	}))
	e := pe.Event
	if pe.HarnessSessionID != "sess-1" || pe.ParentSessionID != "" {
		t.Errorf("sessions = %q/%q, want sess-1 and no parent", pe.HarnessSessionID, pe.ParentSessionID)
	}
	if e.Type != transcript.EventToolUse || e.Role != transcript.RoleAssistant || e.ToolName != "Bash" || e.ToolUseID != "toolu_1" {
		t.Errorf("event = %+v, want an assistant tool_use for Bash toolu_1", e)
	}
	if e.Source != transcript.SourceHook || e.NativeID != "hook:pre-tool-use:toolu_1" || e.ID() != "hook:pre-tool-use:toolu_1" {
		t.Errorf("source %q, native id %q, ID %q", e.Source, e.NativeID, e.ID())
	}
	if string(e.ToolInput) != `{"command":"echo hi","description":"say hi"}` {
		t.Errorf("ToolInput = %s, want the compact input", e.ToolInput)
	}
	if e.Timestamp.Before(before) || e.Timestamp.After(time.Now().Add(time.Second)) {
		t.Errorf("Timestamp = %v, want when the hook fired", e.Timestamp)
	}
	if e.Text != "" {
		t.Errorf("Text = %q: a tool event must not read as a chat turn", e.Text)
	}
}

func TestToolHookPostToolUse(t *testing.T) {
	cases := []struct {
		name     string
		response any
		want     string
	}{
		{"object", map[string]any{"stdout": "hi", "stderr": "", "interrupted": false}, `{"interrupted":false,"stderr":"","stdout":"hi"}`},
		{"string", "file contents", "file contents"},
		{"absent", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fields := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "tool_use_id": "toolu_2", "tool_input": map[string]any{"file_path": "/x"}, "duration_ms": 12}
			if c.response != nil {
				fields["tool_response"] = c.response
			}
			e := parseTool(t, harness.HookArgPostToolUse, toolPayload(t, fields)).Event
			if e.Type != transcript.EventToolResult || e.Role != transcript.RoleTool || e.ToolName != "Read" || e.ToolUseID != "toolu_2" {
				t.Errorf("event = %+v, want a tool_result for Read toolu_2", e)
			}
			if e.Output != c.want {
				t.Errorf("Output = %q, want %q", e.Output, c.want)
			}
			if e.NativeID != "hook:post-tool-use:toolu_2" || e.Source != transcript.SourceHook {
				t.Errorf("native id %q, source %q", e.NativeID, e.Source)
			}
			if len(e.ToolInput) != 0 {
				t.Errorf("ToolInput = %s: the start event carries the input", e.ToolInput)
			}
		})
	}
}

func TestToolHookPostToolUseFailure(t *testing.T) {
	e := parseTool(t, harness.HookArgPostToolUseFailure, toolPayload(t, map[string]any{
		"hook_event_name": "PostToolUseFailure", "tool_name": "Bash", "tool_use_id": "toolu_3",
		"tool_input": map[string]any{"command": "false"}, "error": "Exit code 1",
	})).Event
	if e.Type != transcript.EventToolResult || e.Output != "Exit code 1" || e.NativeID != "hook:post-tool-use-failure:toolu_3" {
		t.Errorf("event = %+v, want a failure tool_result", e)
	}
	// A finish and a failure of the same call never collapse into one.
	if e.ID() == "hook:post-tool-use:toolu_3" {
		t.Error("a failure shares the success's identity")
	}
	e = parseTool(t, harness.HookArgPostToolUseFailure, toolPayload(t, map[string]any{
		"tool_name": "Bash", "tool_use_id": "toolu_4", "error": "stopped", "is_interrupt": true,
	})).Event
	if e.Output != "interrupted: stopped" {
		t.Errorf("interrupted Output = %q", e.Output)
	}
}

func TestToolHookInsideSubagent(t *testing.T) {
	pe := parseTool(t, harness.HookArgPreToolUse, toolPayload(t, map[string]any{
		"tool_name": "Grep", "tool_use_id": "toolu_5", "tool_input": map[string]any{"pattern": "x"}, "agent_id": "a1b2-c3",
	}))
	if pe.HarnessSessionID != "a1b2-c3" || pe.ParentSessionID != "sess-1" {
		t.Errorf("sessions = %q under %q, want the subagent under sess-1", pe.HarnessSessionID, pe.ParentSessionID)
	}
	_, err := hookProvider{}.ParseHookPayload(harness.HookContext{Cwd: "/wt"}, harness.HookArgPreToolUse, toolPayload(t, map[string]any{
		"tool_name": "Grep", "tool_use_id": "toolu_6", "agent_id": "../escape",
	}))
	if err == nil {
		t.Error("a traversal agent_id was accepted")
	}
}

func TestToolHookBounds(t *testing.T) {
	big := strings.Repeat("é", harness.MaxToolHookBytes) // two bytes a rune: well over the bound
	pe := parseTool(t, harness.HookArgPreToolUse, toolPayload(t, map[string]any{
		"tool_name": "Write", "tool_use_id": "toolu_7", "tool_input": map[string]any{"file_path": "/x", "content": big},
	}))
	var s string
	if err := json.Unmarshal(pe.Event.ToolInput, &s); err != nil {
		t.Fatalf("oversize ToolInput = %.80s…: want a JSON string (%v)", pe.Event.ToolInput, err)
	}
	if !strings.HasSuffix(s, harness.ToolHookTruncated) || len(s) > harness.MaxToolHookBytes+len(harness.ToolHookTruncated) || !utf8.ValidString(s) {
		t.Errorf("oversize input: %d bytes, marked %v, valid UTF-8 %v", len(s), strings.HasSuffix(s, harness.ToolHookTruncated), utf8.ValidString(s))
	}
	if !strings.HasPrefix(s, `{"content":"éé`) {
		t.Errorf("oversize input does not start with the input's JSON text: %.40q", s)
	}

	out := parseTool(t, harness.HookArgPostToolUse, toolPayload(t, map[string]any{
		"tool_name": "Bash", "tool_use_id": "toolu_8", "tool_response": big,
	})).Event.Output
	if !strings.HasSuffix(out, harness.ToolHookTruncated) || len(out) > harness.MaxToolHookBytes+len(harness.ToolHookTruncated) || !utf8.ValidString(out) {
		t.Errorf("oversize output: %d bytes, marked %v, valid UTF-8 %v", len(out), strings.HasSuffix(out, harness.ToolHookTruncated), utf8.ValidString(out))
	}
	// At the bound exactly, nothing is cut.
	exact := strings.Repeat("a", harness.MaxToolHookBytes)
	if got := parseTool(t, harness.HookArgPostToolUse, toolPayload(t, map[string]any{
		"tool_name": "Bash", "tool_use_id": "toolu_9", "tool_response": exact,
	})).Event.Output; got != exact {
		t.Errorf("output at the bound was changed: %d bytes", len(got))
	}
}

func TestToolHookRejectsMalformed(t *testing.T) {
	ctx := harness.HookContext{Cwd: "/wt"}
	for name, stdin := range map[string][]byte{
		"no tool name":   toolPayload(t, map[string]any{"tool_use_id": "toolu_1"}),
		"bad tool input": []byte(`{"session_id":"s","tool_name":7}`),
	} {
		if _, err := (hookProvider{}).ParseHookPayload(ctx, harness.HookArgPreToolUse, stdin); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// No tool_use id: the event keeps its content-hash identity rather than
	// a shared "hook:pre-tool-use:" one.
	e := parseTool(t, harness.HookArgPreToolUse, toolPayload(t, map[string]any{"tool_name": "Bash"})).Event
	if e.NativeID != "" || !strings.HasPrefix(e.ID(), "h:") {
		t.Errorf("id-less call: native id %q, ID %q", e.NativeID, e.ID())
	}
}
