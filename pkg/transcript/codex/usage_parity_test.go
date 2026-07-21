package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestUsageParity_MetaHarnessGolden is the byte-for-byte parity gate the
// HARNESS-WRAPPER-47 goldens depend on: it feeds a REAL captured Codex rollout
// fixture (SEVERAL token_count events — including an early info:null one — so the
// last-total_token_usage rule is exercised) to UsageFromJSONL, marshals the
// resulting *transcript.Usage, and asserts the bytes equal the checked-in golden.
//
// The golden is the source of truth for CI. It was produced by meta-harness's
//
//	JSON.stringify(usageToPublicJSON(usageFromCodexJSONL(<fixture bytes>)))
//
// from testdata/usage_tokencounts.jsonl (verified against the dist build of
// meta-harness at check-in time). Go's json.Marshal of transcript.Usage emits
// the same five snake_case keys in the same order with no trailing newline, so
// the two byte streams are identical:
//
//	{"input_tokens":5000,"output_tokens":850,"cache_read_input_tokens":3200,
//	 "cache_creation_input_tokens":0,"reasoning_output_tokens":420}
//
// All five wire keys are ALWAYS present including the zero
// cache_creation_input_tokens (Codex has no cache-creation accounting).
func TestUsageParity_MetaHarnessGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "usage_tokencounts.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "usage_tokencounts.golden.json"))
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
