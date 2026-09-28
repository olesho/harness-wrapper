package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// entriesFixture is a claude 2.1.283 print-mode session against a mock
// Messages API — a reply, interrupts mid-reply, mid-tool and before the first
// token, an exhausted 529, and a 429 retried until the mock went away — with
// its attachment entries left out.
const entriesFixture = "testdata/entries-2.1.283.jsonl"

func followFixture(t *testing.T, decode transcript.Decoder) []transcript.FollowedEvent {
	t.Helper()
	f, err := transcript.NewFollower(entriesFixture, "a1f82cd3-6936-4ffd-8998-70b64b65dec9", transcript.Checkpoint{}, decode)
	if err != nil {
		t.Fatal(err)
	}
	f.MaxBatchBytes = 1 << 30
	b, err := f.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Errors) > 0 {
		t.Fatalf("source errors: %v", b.Errors)
	}
	return b.Events
}

// DecodeEntry gives Follow's events, each with its entry, and the entry facts
// a turn's input and end are read from.
func TestDecodeEntry(t *testing.T) {
	plain := followFixture(t, decodeRecord)
	withEntries := followFixture(t, DecodeEntry)

	var same []transcript.FollowedEvent
	byUUID := map[string]*Entry{}
	for _, fe := range withEntries {
		e, ok := fe.Meta.(*Entry)
		if !ok {
			t.Fatalf("event %s has no entry", fe.Event.NativeID)
		}
		if e.UUID != fe.Event.UUID {
			t.Errorf("event %s: entry %s", fe.Event.NativeID, e.UUID)
		}
		byUUID[e.UUID[:8]] = e
		if fe.Event.Type != EventEntry {
			same = append(same, fe)
		}
	}
	if len(same) != len(plain) {
		t.Fatalf("%d events besides entry events, Follow gives %d", len(same), len(plain))
	}
	for i := range plain {
		if same[i].Event.NativeID != plain[i].Event.NativeID || same[i].Offset != plain[i].Offset {
			t.Errorf("event %d: %s at %d, Follow gives %s at %d", i, same[i].Event.NativeID, same[i].Offset, plain[i].Event.NativeID, plain[i].Offset)
		}
	}

	for _, c := range []struct {
		uuid string
		want Entry
	}{
		{"836c3ebb", Entry{Type: "user"}},                              // a prompt, by the message's uuid
		{"624ab4b2", Entry{Type: "assistant", StopReason: "end_turn"}}, // the reply that ended its turn
		{"7f5987c5", Entry{Type: "user", Interrupt: true}},             // interrupted mid-reply
		{"95a5c07c", Entry{Type: "user", Interrupt: true}},             // interrupted mid-tool
		{"c3bbd9af", Entry{Type: "user", Interrupt: true}},             // interrupted before the first token
		{"543c389a", Entry{Type: "assistant", StopReason: "tool_use"}},
		{"a1e0efbc", Entry{Type: "assistant", StopReason: "stop_sequence", APIError: "server_error", APIErrorStatus: 529}},
		// The mock went away mid-retries: "API Error: Connection refused".
		{"13d4464c", Entry{Type: "assistant", StopReason: "stop_sequence", APIError: "server_error"}},
	} {
		got := byUUID[c.uuid]
		if got == nil {
			t.Errorf("no entry %s", c.uuid)
			continue
		}
		g := *got
		g.UUID, g.MessageID = "", ""
		if g != c.want {
			t.Errorf("entry %s = %+v, want %+v", c.uuid, g, c.want)
		}
	}
	if e := byUUID["624ab4b2"]; e != nil && !strings.HasPrefix(e.MessageID, "msg_") {
		t.Errorf("the reply's message id is %q", e.MessageID)
	}
}
