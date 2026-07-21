package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// TestUsageFromJSONL_DedupByMessageID exercises the core parity contract: two
// API calls, each split across MULTIPLE content-block lines that REPEAT the same
// message.usage. The result must SUM the two calls with EACH COUNTED ONCE.
func TestUsageFromJSONL_DedupByMessageID(t *testing.T) {
	body := `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"tool_use","id":"t1"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}
{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":7}}}
{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"tool_use","id":"t2"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":7}}}
`
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	want := transcript.Usage{
		InputTokens:              110, // 10 + 100
		OutputTokens:             22,  // 2 + 20
		CacheReadInputTokens:     5,   // 5 + 0
		CacheCreationInputTokens: 10,  // 3 + 7
		ReasoningOutputTokens:    0,   // Claude never emits this
	}
	if *u != want {
		t.Errorf("usage = %+v, want %+v", *u, want)
	}
}

// TestUsageFromJSONL_FallbackToUUID checks the dedup fallback: when a line has an
// empty/absent message.id, the dedup key becomes "line:"+uuid. Two lines with no
// id but DIFFERENT uuids are distinct API calls and both contribute.
func TestUsageFromJSONL_FallbackToUUID(t *testing.T) {
	body := `{"type":"assistant","uuid":"u1","message":{"role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"output_tokens":1}}}
{"type":"assistant","uuid":"u2","message":{"role":"assistant","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":20,"output_tokens":2}}}
`
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.InputTokens != 30 || u.OutputTokens != 3 {
		t.Errorf("fallback dedup: got input=%d output=%d, want 30/3", u.InputTokens, u.OutputTokens)
	}
}

// TestUsageFromJSONL_FallbackDedupsSameUUID confirms two id-less lines sharing
// one uuid collapse to a single API call (the repeated-per-block case without an
// id present).
func TestUsageFromJSONL_FallbackDedupsSameUUID(t *testing.T) {
	body := `{"type":"assistant","uuid":"u1","message":{"role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"output_tokens":1}}}
{"type":"assistant","uuid":"u1","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1"}],"usage":{"input_tokens":10,"output_tokens":1}}}
`
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.InputTokens != 10 || u.OutputTokens != 1 {
		t.Errorf("same-uuid dedup: got input=%d output=%d, want 10/1", u.InputTokens, u.OutputTokens)
	}
}

// TestUsageFromJSONL_ClampsNegative proves every field is clamped via
// transcript.ToCount: a negative input_tokens becomes 0, not a subtraction.
func TestUsageFromJSONL_ClampsNegative(t *testing.T) {
	body := `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":-50,"output_tokens":5}}}
`
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	if u.InputTokens != 0 {
		t.Errorf("negative input_tokens should clamp to 0, got %d", u.InputTokens)
	}
	if u.OutputTokens != 5 {
		t.Errorf("output_tokens = %d, want 5", u.OutputTokens)
	}
}

// TestUsageFromJSONL_NoUsage asserts (nil, nil) when no assistant line carried a
// message.usage object.
func TestUsageFromJSONL_NoUsage(t *testing.T) {
	body := `{"type":"user","message":{"role":"user","content":"hi"}}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"a"}]}}
`
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != nil {
		t.Errorf("expected nil usage, got %+v", *u)
	}
}

// TestReadUsageAgainstFixture is the thin file-facing smoke test: it stages a
// transcript on disk (the EncodedCWD/ProjectsRoot layout) and asserts the
// locate→readFile→aggregator flow yields the SAME Usage the inline byte path
// produces.
func TestReadUsageAgainstFixture(t *testing.T) {
	dir := t.TempDir()
	cwd := "/some/work/dir"
	projDir := filepath.Join(dir, "projects", EncodedCWD(cwd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"a"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"tool_use","id":"t1"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}
{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"b"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":7}}}
`
	if err := os.WriteFile(filepath.Join(projDir, "sess.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &Reader{ProjectsRoot: filepath.Join(dir, "projects")}
	got, err := r.ReadUsage("sess", cwd)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected usage, got nil")
	}
	want := transcript.Usage{
		InputTokens:              110,
		OutputTokens:             22,
		CacheReadInputTokens:     5,
		CacheCreationInputTokens: 10,
	}
	if *got != want {
		t.Errorf("ReadUsage = %+v, want %+v", *got, want)
	}

	// Compile-time proof that *Reader satisfies transcript.UsageReader.
	var _ transcript.UsageReader = (*Reader)(nil)
}
