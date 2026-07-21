package codex

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// tokenCountLine builds an event_msg/token_count JSONL line with the given
// total_token_usage fields.
func tokenCountLine(input, cached, output, reasoning string) string {
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{` +
		`"input_tokens":` + input + `,"cached_input_tokens":` + cached +
		`,"output_tokens":` + output + `,"reasoning_output_tokens":` + reasoning + `}}}}`
}

// TestUsageFromJSONL_LastWins asserts the LAST token_count event's cumulative
// total_token_usage is returned, with cached_input_tokens mapped to
// CacheReadInputTokens and CacheCreationInputTokens left at 0.
func TestUsageFromJSONL_LastWins(t *testing.T) {
	body := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}
` + tokenCountLine("100", "10", "20", "5") + `
` + tokenCountLine("300", "40", "60", "15") + `
`
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatalf("UsageFromJSONL: %v", err)
	}
	if u == nil {
		t.Fatal("got nil usage, want the last token_count total")
	}
	want := transcript.Usage{
		InputTokens:              300,
		OutputTokens:             60,
		CacheReadInputTokens:     40,
		CacheCreationInputTokens: 0,
		ReasoningOutputTokens:    15,
	}
	if *u != want {
		t.Fatalf("usage = %+v, want %+v", *u, want)
	}
}

// TestUsageFromJSONL_SkipsNullInfo asserts a token_count with info:null before
// the last real one is skipped and the last real event wins.
func TestUsageFromJSONL_SkipsNullInfo(t *testing.T) {
	body := `{"type":"event_msg","payload":{"type":"token_count","info":null}}
` + tokenCountLine("200", "20", "40", "8") + `
{"type":"event_msg","payload":{"type":"token_count","info":null}}
`
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatalf("UsageFromJSONL: %v", err)
	}
	if u == nil {
		t.Fatal("got nil usage, want the last non-null token_count total")
	}
	want := transcript.Usage{InputTokens: 200, OutputTokens: 40, CacheReadInputTokens: 20, ReasoningOutputTokens: 8}
	if *u != want {
		t.Fatalf("usage = %+v, want %+v (info:null events must be skipped)", *u, want)
	}
}

// TestUsageFromJSONL_NoTokenCount asserts a body with only response_item lines
// yields (nil, nil).
func TestUsageFromJSONL_NoTokenCount(t *testing.T) {
	body := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}
{"type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"c1","arguments":"{}"}}
`
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatalf("UsageFromJSONL: %v", err)
	}
	if u != nil {
		t.Fatalf("usage = %+v, want nil for a body with no token_count", *u)
	}
}

// TestUsageFromJSONL_ClampsNegative asserts a negative field in the winning
// event clamps to 0 (proves ToCount is applied).
func TestUsageFromJSONL_ClampsNegative(t *testing.T) {
	body := tokenCountLine("-5", "10", "20", "3") + "\n"
	u, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatalf("UsageFromJSONL: %v", err)
	}
	if u == nil {
		t.Fatal("got nil usage, want a clamped total")
	}
	if u.InputTokens != 0 {
		t.Fatalf("InputTokens = %d, want 0 (negative must clamp via ToCount)", u.InputTokens)
	}
	if u.CacheReadInputTokens != 10 || u.OutputTokens != 20 || u.ReasoningOutputTokens != 3 {
		t.Fatalf("non-negative fields altered: %+v", *u)
	}
}

// TestReadUsage_OnDisk is the thin file-facing smoke test: stage a rollout on
// disk, then assert locate→readFile→aggregator yields the same Usage the inline
// byte test produces. Also confirms *Reader satisfies transcript.UsageReader.
func TestReadUsage_OnDisk(t *testing.T) {
	root := t.TempDir()
	sessionID := "00000000-0000-0000-0000-0000000000d1"
	dir := filepath.Join(root, "2026", "07", "21")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}
` + tokenCountLine("100", "10", "20", "5") + `
` + tokenCountLine("300", "40", "60", "15") + `
`
	path := filepath.Join(dir, "rollout-2026-07-21T07-25-23-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	var ur transcript.UsageReader = &Reader{SessionsRoot: root}
	got, err := ur.ReadUsage(sessionID, "")
	if err != nil {
		t.Fatalf("ReadUsage: %v", err)
	}
	want, err := UsageFromJSONL([]byte(body))
	if err != nil {
		t.Fatalf("UsageFromJSONL: %v", err)
	}
	if got == nil || want == nil || *got != *want {
		t.Fatalf("ReadUsage = %+v, want %+v (same as inline aggregator)", got, want)
	}
}
