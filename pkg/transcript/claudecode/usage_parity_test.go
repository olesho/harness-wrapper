package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestUsageParity_MetaHarnessGolden is the byte-for-byte parity gate the
// HARNESS-WRAPPER-47 goldens depend on: it feeds a REAL captured Claude JSONL
// fixture (a multi-line tool loop that REPEATS message.id across content-block
// lines, with TWO distinct API calls so both dedup-by-message.id and the SUM
// rule are exercised) to UsageFromJSONL, marshals the resulting *transcript.Usage,
// and asserts the bytes equal the checked-in golden.
//
// The golden is the source of truth for CI. It was produced by meta-harness's
//
//	JSON.stringify(usageToPublicJSON(usageFromClaudeJSONL(<fixture bytes>)))
//
// from testdata/usage_toolloop.jsonl (verified against the dist build of
// meta-harness at check-in time). Go's json.Marshal of transcript.Usage emits
// the same five snake_case keys in the same order with no trailing newline, so
// the two byte streams are identical:
//
//	{"input_tokens":3300,"output_tokens":215,"cache_read_input_tokens":17500,
//	 "cache_creation_input_tokens":2000,"reasoning_output_tokens":0}
//
// All five wire keys are ALWAYS present including the zero reasoning_output_tokens
// (Claude never reports reasoning tokens).
func TestUsageParity_MetaHarnessGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "usage_toolloop.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "usage_toolloop.golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}

	u, err := UsageFromJSONL(data)
	if err != nil {
		t.Fatalf("UsageFromJSONL: %v", err)
	}
	if u == nil {
		t.Fatal("UsageFromJSONL returned nil; fixture must carry usage")
	}

	got, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal usage: %v", err)
	}
	if string(got) != string(golden) {
		t.Errorf("usage JSON is not byte-identical with meta-harness golden:\n got: %s\nwant: %s", got, golden)
	}
}
