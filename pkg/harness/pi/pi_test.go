package pi

import (
	"bufio"
	"os"
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// TestParseStreamLine_RealCorpus replays a real `pi -p --mode json` capture
// (recorded live from pi 0.76.0, committed under test/corpus/pi/) through the
// StreamParser and asserts the canonical events come out — a regression guard
// grounded in actual pi output, not just hand-written line shapes.
func TestParseStreamLine_RealCorpus(t *testing.T) {
	const path = "../../../test/corpus/pi/headless-json-toolcall.jsonl"
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("corpus not present: %v", err)
	}
	defer func() { _ = f.Close() }()

	p := streamParser{}
	byType := map[string]int{}
	var sawReadToolUse, sawLinkedResult, sawUserPrompt bool
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		for _, pe := range p.ParseStreamLine(sc.Text()) {
			e := pe.Event
			byType[e.Type]++
			if e.Type == transcript.EventToolUse && e.ToolName == "read" {
				sawReadToolUse = true
			}
			if e.Type == transcript.EventToolResult && e.ToolUseID != "" {
				sawLinkedResult = true
			}
			if e.Type == transcript.EventText && e.Role == transcript.RoleUser {
				sawUserPrompt = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	t.Logf("events by type: %v", byType)
	if !sawUserPrompt {
		t.Error("expected a user text event (the prompt)")
	}
	if !sawReadToolUse {
		t.Error("expected a tool_use event for the read tool")
	}
	if !sawLinkedResult {
		t.Error("expected a tool_result event carrying its toolCallId linkage")
	}
	if byType[transcript.EventText] < 2 {
		t.Errorf("expected ≥2 text events (user prompt + assistant reply), got %d", byType[transcript.EventText])
	}
}

func TestExtractSessionID(t *testing.T) {
	ext := sessionIDExtractor{}
	cases := []struct {
		name   string
		line   string
		want   string
		wantOK bool
	}{
		{
			name:   "session header carries id",
			line:   `{"type":"session","version":3,"id":"019e2824-db19-72b2-bd4a-d5a5d80f74f0","timestamp":"2026-06-24T00:00:00Z","cwd":"/x"}`,
			want:   "019e2824-db19-72b2-bd4a-d5a5d80f74f0",
			wantOK: true,
		},
		{
			name:   "agent_start line is not the header",
			line:   `{"type":"agent_start"}`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "turn_end line is not the header",
			line:   `{"type":"turn_end","message":{"role":"assistant"},"toolResults":[]}`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "session header with empty id",
			line:   `{"type":"session","version":3,"id":""}`,
			want:   "",
			wantOK: false,
		},
		{
			name:   "non-JSON / ANSI-polluted line is skipped",
			line:   "\x1b[2m> some TUI noise\x1b[0m",
			want:   "",
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   "",
			want:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ext.ExtractSessionID(tc.line)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("ExtractSessionID(%q) = (%q,%v), want (%q,%v)", tc.line, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestResumeArgs(t *testing.T) {
	r := resumer{}
	got := r.ResumeArgs("abc-123")
	want := []string{"--session", "abc-123"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResumeArgs = %v, want %v", got, want)
	}
	if got := r.ResumeArgs(""); got != nil {
		t.Fatalf("ResumeArgs(\"\") = %v, want nil (cold start)", got)
	}
}

func TestResolvePopulatesCapabilities(t *testing.T) {
	rp := Profile{}.Resolve(harness.ResolveContext{})
	if rp.SessionID == nil {
		t.Fatal("Resolve: SessionID capability must be non-nil for pi")
	}
	if rp.Resume == nil {
		t.Fatal("Resolve: Resume capability must be non-nil for pi")
	}
	if rp.Stream == nil {
		t.Fatal("Resolve: Stream capability must be non-nil for pi")
	}
	// Hooks is deliberately not implemented (see package doc); lock that in so a
	// future addition is an intentional change, not an accident.
	if rp.Hooks != nil {
		t.Error("Resolve: Hooks must be nil (pi has no documented shell-hook contract)")
	}
	if id, ok := rp.SessionID.ExtractSessionID(`{"type":"session","id":"s"}`); !ok || id != "s" {
		t.Fatalf("resolved SessionID.ExtractSessionID = (%q,%v)", id, ok)
	}
}

// TestParseStreamLine locks in the message_end-only parsing rule and the block
// mapping, using line shapes captured live from pi 0.76.0.
func TestParseStreamLine(t *testing.T) {
	p := streamParser{}

	t.Run("non-message_end lines are skipped", func(t *testing.T) {
		for _, line := range []string{
			`{"type":"session","version":3,"id":"019efabc-d24b-78b7-ad3b-bb56ff658f11"}`,
			`{"type":"agent_start"}`,
			`{"type":"turn_start"}`,
			`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
			`{"type":"message_update","message":{"role":"assistant","content":[]},"assistantMessageEvent":{"type":"text_delta","delta":"hi"}}`,
			// turn_end carries a copy of the final message_end — skipping it is the de-dup rule.
			`{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"toolResults":[]}`,
			`{"type":"tool_execution_end","toolCallId":"d06ea0da5","toolName":"read","isError":false,"result":{}}`,
			`{"type":"agent_end","messages":[]}`,
			"\x1b[2m> ANSI noise\x1b[0m",
			"",
		} {
			if got := p.ParseStreamLine(line); got != nil {
				t.Errorf("ParseStreamLine(%q) = %+v, want nil", line, got)
			}
		}
	})

	t.Run("user message_end → one user text event", func(t *testing.T) {
		evs := p.ParseStreamLine(`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"Read the file go.mod"}]}}`)
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
		}
		e := evs[0].Event
		if e.Role != transcript.RoleUser || e.Type != transcript.EventText || e.Text != "Read the file go.mod" {
			t.Fatalf("unexpected event: %+v", e)
		}
		if e.Source != transcript.SourceLive {
			t.Errorf("Source = %q, want %q", e.Source, transcript.SourceLive)
		}
	})

	t.Run("assistant thinking+toolCall → one tool_use event (thinking dropped)", func(t *testing.T) {
		line := `{"type":"message_end","message":{"role":"assistant","content":[` +
			`{"type":"thinking","thinking":"reason","thinkingSignature":"reasoning"},` +
			`{"type":"toolCall","id":"d06ea0da5","name":"read","arguments":{"path":"go.mod"}}]}}`
		evs := p.ParseStreamLine(line)
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
		}
		e := evs[0].Event
		if e.Type != transcript.EventToolUse || e.ToolName != "read" || e.ToolUseID != "d06ea0da5" {
			t.Fatalf("unexpected tool_use event: %+v", e)
		}
		if string(e.ToolInput) != `{"path":"go.mod"}` {
			t.Errorf("ToolInput = %s, want {\"path\":\"go.mod\"}", e.ToolInput)
		}
		if e.ID() != "tool-use:d06ea0da5" {
			t.Errorf("ID() = %q, want tool-use:d06ea0da5", e.ID())
		}
	})

	t.Run("assistant text → one assistant text event", func(t *testing.T) {
		evs := p.ParseStreamLine(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hello there friend"}]}}`)
		if len(evs) != 1 || evs[0].Event.Role != transcript.RoleAssistant || evs[0].Event.Text != "hello there friend" {
			t.Fatalf("unexpected: %+v", evs)
		}
	})

	t.Run("toolResult message → one tool_result event linked by toolCallId", func(t *testing.T) {
		line := `{"type":"message_end","message":{"role":"toolResult","toolCallId":"d06ea0da5","toolName":"read","content":[{"type":"text","text":"module github.com/olesho/harness-wrapper\n"}]}}`
		evs := p.ParseStreamLine(line)
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1: %+v", len(evs), evs)
		}
		e := evs[0].Event
		if e.Role != transcript.RoleTool || e.Type != transcript.EventToolResult {
			t.Fatalf("unexpected role/type: %+v", e)
		}
		if e.ToolUseID != "d06ea0da5" || e.ToolName != "read" {
			t.Errorf("linkage = (%q,%q), want (d06ea0da5,read)", e.ToolUseID, e.ToolName)
		}
		if e.Output != "module github.com/olesho/harness-wrapper\n" {
			t.Errorf("Output = %q", e.Output)
		}
		// tool_use and tool_result for the same call must not collapse to one id.
		if e.ID() != "tool-result:d06ea0da5" {
			t.Errorf("ID() = %q, want tool-result:d06ea0da5", e.ID())
		}
	})
}

func TestRegisteredViaInit(t *testing.T) {
	p, ok := harness.For("pi")
	if !ok {
		t.Fatal("harness.For(\"pi\") not registered (init did not run?)")
	}
	if p.Name() != "pi" {
		t.Fatalf("profile name = %q, want pi", p.Name())
	}
}
