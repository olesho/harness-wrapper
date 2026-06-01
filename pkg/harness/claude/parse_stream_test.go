package claude

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

func TestParseStreamLineAssistantTextAndToolUse(t *testing.T) {
	line := `{"type":"assistant","session_id":"sess-1","message":{"id":"msg_a","role":"assistant","content":[{"type":"text","text":"let me run that"},{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"ls -la"}}]}}`
	evs := streamParser{}.ParseStreamLine(line)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (text + tool_use): %+v", len(evs), evs)
	}

	text := evs[0]
	if text.HarnessSessionID != "sess-1" {
		t.Errorf("text HarnessSessionID = %q, want sess-1", text.HarnessSessionID)
	}
	if text.Event.Role != transcript.RoleAssistant || text.Event.Type != transcript.EventText || text.Event.Text != "let me run that" {
		t.Errorf("text event wrong: %+v", text.Event)
	}
	if text.Event.Source != transcript.SourceLive {
		t.Errorf("text Source = %q, want live", text.Event.Source)
	}
	if text.Event.NativeID != "live:text:msg_a:0" {
		t.Errorf("text NativeID = %q, want live:text:msg_a:0", text.Event.NativeID)
	}

	tool := evs[1]
	if tool.Event.Type != transcript.EventToolUse || tool.Event.ToolName != "Bash" || tool.Event.ToolUseID != "toolu_01" {
		t.Errorf("tool_use event wrong: %+v", tool.Event)
	}
	if string(tool.Event.ToolInput) != `{"command":"ls -la"}` {
		t.Errorf("tool_use ToolInput = %s, want {\"command\":\"ls -la\"}", tool.Event.ToolInput)
	}
	// tool_use carries a SHARED native id (file == live), so NOT source-prefixed.
	if tool.Event.NativeID != "tool-use:toolu_01" || tool.Event.ID() != "tool-use:toolu_01" {
		t.Errorf("tool_use NativeID/ID = %q/%q, want tool-use:toolu_01", tool.Event.NativeID, tool.Event.ID())
	}
}

func TestParseStreamLineUserToolResult(t *testing.T) {
	line := `{"type":"user","session_id":"sess-1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"total 0\nfile.go"}]}]}}`
	evs := streamParser{}.ParseStreamLine(line)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1 (tool_result): %+v", len(evs), evs)
	}
	r := evs[0]
	if r.Event.Role != transcript.RoleTool || r.Event.Type != transcript.EventToolResult || r.Event.ToolUseID != "toolu_01" {
		t.Errorf("tool_result event wrong: %+v", r.Event)
	}
	if !strings.Contains(r.Event.Output, "file.go") {
		t.Errorf("tool_result Output = %q, want it to contain file.go", r.Event.Output)
	}
	// tool_result shares the tool_use id → cross-source-stable id, no prefix.
	if r.Event.NativeID != "tool-result:toolu_01" {
		t.Errorf("tool_result NativeID = %q, want tool-result:toolu_01", r.Event.NativeID)
	}
}

func TestParseStreamLineUserStringTextStripsIDETags(t *testing.T) {
	// A user text block (rare in the live stream) has IDE/system context tags
	// stripped, matching the file parser's behavior for user content.
	line := `{"type":"user","session_id":"s","message":{"role":"user","content":[{"type":"text","text":"hi <ide_selection>noise</ide_selection> there"}]}}`
	evs := streamParser{}.ParseStreamLine(line)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
	}
	if evs[0].Event.Role != transcript.RoleUser || !strings.Contains(evs[0].Event.Text, "there") {
		t.Errorf("user text event wrong: %+v", evs[0].Event)
	}
	if strings.Contains(evs[0].Event.Text, "noise") {
		t.Errorf("IDE context tag not stripped: %q", evs[0].Event.Text)
	}
}

func TestParseStreamLineSkipsNonConversation(t *testing.T) {
	cases := []struct {
		name, line string
	}{
		{"system init (session id captured separately)", `{"type":"system","subtype":"init","session_id":"s"}`},
		{"result line", `{"type":"result","subtype":"success","session_id":"s"}`},
		{"non-JSON / ANSI noise", "\x1b[2m> thinking…\x1b[0m"},
		{"empty line", ""},
		{"assistant with no message", `{"type":"assistant","session_id":"s"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if evs := (streamParser{}).ParseStreamLine(tc.line); evs != nil {
				t.Fatalf("ParseStreamLine(%q) = %+v, want nil", tc.line, evs)
			}
		})
	}
}

func TestParseStreamLineSkipsEmptyAssistantText(t *testing.T) {
	// An empty text block (e.g. a streamed-but-empty delta) yields no event, but
	// a sibling tool_use is still emitted.
	line := `{"type":"assistant","session_id":"s","message":{"id":"m","content":[{"type":"text","text":""},{"type":"tool_use","id":"t1","name":"Read","input":{}}]}}`
	evs := streamParser{}.ParseStreamLine(line)
	if len(evs) != 1 || evs[0].Event.Type != transcript.EventToolUse {
		t.Fatalf("got %+v, want a single tool_use event", evs)
	}
}

// TestResolvePopulatesStream locks the P3a contract: claude's ResolvedProfile
// now carries a working StreamParser capability.
func TestResolvePopulatesStream(t *testing.T) {
	rp := Profile{}.Resolve(harness.ResolveContext{})
	if rp.Stream == nil {
		t.Fatal("Resolve: Stream capability must be non-nil for claude")
	}
	evs := rp.Stream.ParseStreamLine(`{"type":"assistant","session_id":"s","message":{"id":"m","content":[{"type":"text","text":"hi"}]}}`)
	if len(evs) != 1 || evs[0].Event.Text != "hi" {
		t.Fatalf("resolved Stream.ParseStreamLine = %+v, want one text event 'hi'", evs)
	}
}
