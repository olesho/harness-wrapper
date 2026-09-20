package claudecode

import (
	"encoding/json"
	"os"
	"testing"
)

// The event stream is a serialized contract: loom's Runs tab, the hook
// payloads and chat.History all read it, and transcript.SchemaVersion promises
// additions are additive. Carrying the harness's API-error tag through the
// parser is the first field added since that promise, so it is pinned here on
// a committed fixture rather than argued.
//
// The fixture holds no tagged line, which is the point: an ordinary transcript
// must serialize to exactly what it serialized to before, byte for byte. The
// tagged case is covered in pkg/chat's corpus tests and in
// TestParserCompatibility_TagAppearsOnlyWhereTheLineCarriedIt.
//
// Regenerate after an INTENTIONAL change with:
// UPDATE_GOLDEN=1 go test ./pkg/transcript/claudecode/
func TestEventsGolden(t *testing.T) {
	in, err := os.ReadFile("testdata/usage_toolloop.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	evs, err := Events(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("fixture produced no events")
	}
	for _, e := range evs {
		if e.APIError != "" {
			t.Fatalf("fixture carries a tag on %q — pick an untagged fixture for this test", e.NativeID)
		}
	}
	got, err := json.MarshalIndent(evs, "", " ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	const golden = "testdata/events_toolloop.golden.json"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("event stream changed shape; if intentional, regenerate with UPDATE_GOLDEN=1\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
