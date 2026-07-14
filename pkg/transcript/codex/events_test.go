package codex

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// TestEventsToolAware exercises the byte parser (the API loom delegates to):
// response_item message → one text event per content block (IDE tags stripped
// for user), function_call → tool_use, function_call_output → tool_result.
func TestEventsToolAware(t *testing.T) {
	body := `{"timestamp":"2026-05-14T12:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do it <ide_selection>noise</ide_selection>"}]}}
{"timestamp":"2026-05-14T12:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"sure"},{"type":"output_text","text":"running"}]}}
{"timestamp":"2026-05-14T12:00:02Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"call_1","arguments":"{\"cmd\":\"ls\"}"}}
{"timestamp":"2026-05-14T12:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"file.go"}}
{"type":"event_msg","payload":{"x":1}}
`
	evs, err := Events([]byte(body))
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// user "do it" (IDE tag stripped) + assistant "sure" + "running" (one per
	// block) + tool_use + tool_result = 5 events. event_msg skipped.
	if len(evs) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(evs), evs)
	}
	if evs[0].Role != transcript.RoleUser || evs[0].Text != "do it" {
		t.Errorf("event 0 (user, IDE-stripped) = %+v", evs[0])
	}
	if evs[1].Text != "sure" || evs[2].Text != "running" {
		t.Errorf("assistant blocks should be SEPARATE events: %q, %q", evs[1].Text, evs[2].Text)
	}
	tu := evs[3]
	if tu.Type != transcript.EventToolUse || tu.ToolName != "shell" || tu.ToolUseID != "call_1" {
		t.Errorf("function_call → tool_use wrong: %+v", tu)
	}
	if string(tu.ToolInput) != `{"cmd":"ls"}` {
		t.Errorf("tool_use ToolInput = %s", tu.ToolInput)
	}
	if tu.ID() != "tool-use:call_1" {
		t.Errorf("tool_use ID() = %q, want tool-use:call_1", tu.ID())
	}
	tr := evs[4]
	if tr.Type != transcript.EventToolResult || tr.ToolUseID != "call_1" || tr.Output != "file.go" {
		t.Errorf("function_call_output → tool_result wrong: %+v", tr)
	}
	if tr.ID() != "tool-result:call_1" {
		t.Errorf("tool_result ID() = %q, want tool-result:call_1", tr.ID())
	}
	for i, e := range evs {
		if e.Source != transcript.SourceFile {
			t.Errorf("event %d Source = %q, want file", i, e.Source)
		}
	}
}

// TestEventsCustomToolCall covers codex-cli >= 0.144's freeform tool schema:
// custom_tool_call → tool_use (input is a JSON value, used verbatim), and
// custom_tool_call_output → tool_result (array of {text} content blocks
// flattened into readable output).
func TestEventsCustomToolCall(t *testing.T) {
	body := `{"timestamp":"2026-07-14T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"call_9","input":"const r = await tools.exec_command({cmd:[\"bash\",\"-lc\",\"ls\"]});"}}
{"timestamp":"2026-07-14T12:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_9","output":[{"type":"input_text","text":"Script completed\n"},{"type":"input_text","text":"file.go\n"}]}}
`
	evs, err := Events([]byte(body))
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(evs), evs)
	}

	tu := evs[0]
	if tu.Type != transcript.EventToolUse || tu.ToolName != "exec" || tu.ToolUseID != "call_9" {
		t.Errorf("custom_tool_call → tool_use wrong: %+v", tu)
	}
	if !json.Valid(tu.ToolInput) {
		t.Errorf("ToolInput is not valid JSON: %s", tu.ToolInput)
	}
	var script string
	if err := json.Unmarshal(tu.ToolInput, &script); err != nil || !strings.Contains(script, "tools.exec_command") {
		t.Errorf("ToolInput should round-trip to the script: %s (%v)", tu.ToolInput, err)
	}
	if tu.ID() != "tool-use:call_9" {
		t.Errorf("tool_use ID() = %q, want tool-use:call_9", tu.ID())
	}

	tr := evs[1]
	if tr.Type != transcript.EventToolResult || tr.ToolUseID != "call_9" {
		t.Errorf("custom_tool_call_output → tool_result wrong: %+v", tr)
	}
	if tr.Output != "Script completed\nfile.go\n" {
		t.Errorf("flattened output = %q, want %q", tr.Output, "Script completed\nfile.go\n")
	}
	if tr.ID() != "tool-result:call_9" {
		t.Errorf("tool_result ID() = %q, want tool-result:call_9", tr.ID())
	}
}

func TestEventsSkipsNoiseAndEmpty(t *testing.T) {
	body := `{"type":"session_meta","payload":{"id":"x"}}
{"type":"response_item","payload":{"type":"reasoning","summary":"thinking"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}
`
	evs, err := Events([]byte(body))
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("got %d events, want 0 (noise/empty skipped): %+v", len(evs), evs)
	}
}
