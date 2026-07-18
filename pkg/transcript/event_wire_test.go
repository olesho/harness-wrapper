package transcript

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestParsedEventsDurableRoundTrip locks the contract that the DURABLE codec
// preserves the INTERNAL fields (Source/NativeID/SchemaVersion) that Event's
// public JSON omits (json:"-"). The hook spool + event store depend on this:
// the authority filter keys on Source and dedup keys on NativeID.
func TestParsedEventsDurableRoundTrip(t *testing.T) {
	in := []ParsedEvent{
		{
			HarnessSessionID: "sess-1",
			Event: Event{
				Seq: 3, Timestamp: time.Unix(1700000000, 500), Role: RoleAssistant,
				Type: EventToolUse, ToolName: "Bash", ToolUseID: "tu1",
				ToolInput: json.RawMessage(`{"command":"ls"}`),
				Source:    SourceLive, NativeID: "tool-use:tu1", SchemaVersion: SchemaVersion,
			},
		},
		{
			HarnessSessionID: "sub-2", ParentSessionID: "sess-1",
			Event: Event{
				Seq: 0, Role: RoleTool, Type: EventToolResult, Output: "file.go",
				ToolUseID: "tu1", Source: SourceFile, NativeID: "tool-result:tu1",
				SchemaVersion: SchemaVersion,
			},
		},
	}

	data, err := MarshalParsedEvents(in)
	if err != nil {
		t.Fatalf("MarshalParsedEvents: %v", err)
	}
	out, err := UnmarshalParsedEvents(data)
	if err != nil {
		t.Fatalf("UnmarshalParsedEvents: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("round-trip count: got %d, want %d", len(out), len(in))
	}
	for i := range in {
		assertDurableRoundTrip(t, i, in[i], out[i])
	}
}

// assertDurableRoundTrip checks that a single ParsedEvent survived the durable
// round-trip with its session tags and internal Event fields intact.
func assertDurableRoundTrip(t *testing.T, i int, a, b ParsedEvent) {
	t.Helper()
	if a.HarnessSessionID != b.HarnessSessionID || a.ParentSessionID != b.ParentSessionID {
		t.Errorf("event %d session tags differ: %+v vs %+v", i, a, b)
	}
	// The internal fields are the whole point of the durable form.
	if b.Event.Source != a.Event.Source {
		t.Errorf("event %d Source lost: got %q want %q", i, b.Event.Source, a.Event.Source)
	}
	if b.Event.NativeID != a.Event.NativeID {
		t.Errorf("event %d NativeID lost: got %q want %q", i, b.Event.NativeID, a.Event.NativeID)
	}
	if b.Event.SchemaVersion != a.Event.SchemaVersion {
		t.Errorf("event %d SchemaVersion lost: got %d want %d", i, b.Event.SchemaVersion, a.Event.SchemaVersion)
	}
	if b.Event.Type != a.Event.Type || b.Event.ToolUseID != a.Event.ToolUseID || b.Event.Output != a.Event.Output {
		t.Errorf("event %d body differs: %+v vs %+v", i, a.Event, b.Event)
	}
	if string(b.Event.ToolInput) != string(a.Event.ToolInput) {
		t.Errorf("event %d ToolInput differs: %q vs %q", i, a.Event.ToolInput, b.Event.ToolInput)
	}
}

// TestPublicEventJSONStillOmitsInternal guards the OTHER serialization: Event's
// public JSON (the Runs-tab DTO) must still omit the internal fields, so the
// durable codec and the DTO stay distinct.
func TestPublicEventJSONStillOmitsInternal(t *testing.T) {
	data, err := json.Marshal(Event{Type: EventText, Text: "hi", Source: SourceLive, NativeID: "x", SchemaVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{`"source"`, `"native_id"`, `"schema_version"`} {
		if strings.Contains(string(data), banned) {
			t.Errorf("public Event JSON leaked internal field %s: %s", banned, data)
		}
	}
}
