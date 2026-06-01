package codex

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// TestEvents exercises the byte parser directly (the API loom delegates to):
// response_item lines become role-tagged text events; non-response_item and
// empty-content lines are skipped; unknown roles map to system.
func TestEvents(t *testing.T) {
	body := `{"type":"response_item","payload":{"role":"user","content":[{"type":"text","text":"hello"}]}}
{"type":"response_item","payload":{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"text","text":"there"}]}}
{"type":"response_item","payload":{"role":"tool","content":[{"type":"text","text":"tool out"}]}}
{"type":"session_meta","payload":{"id":"x"}}
{"type":"response_item","payload":{"role":"assistant","content":[]}}
`
	evs, err := Events([]byte(body))
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("got %d events, want 3 (user, assistant, tool→system): %+v", len(evs), evs)
	}
	if evs[0].Role != "user" || evs[0].Text != "hello" {
		t.Errorf("event 0 = %+v", evs[0])
	}
	if evs[1].Role != transcript.RoleAssistant || evs[1].Text != "hi\n\nthere" {
		t.Errorf("event 1 = %+v (want joined text)", evs[1])
	}
	if evs[2].Role != transcript.RoleSystem { // tool → system
		t.Errorf("event 2 role = %q, want system", evs[2].Role)
	}
	for i, e := range evs {
		if e.Type != transcript.EventText || e.Source != transcript.SourceFile {
			t.Errorf("event %d type/source = %q/%q", i, e.Type, e.Source)
		}
	}
}

func TestEventsRejectsMalformedLine(t *testing.T) {
	if _, err := Events([]byte("not json\n")); err == nil {
		t.Error("expected a parse error on a malformed line")
	}
}
