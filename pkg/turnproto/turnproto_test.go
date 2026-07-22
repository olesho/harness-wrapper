package turnproto

import (
	"encoding/json"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
)

func TestParseLastJSONLine(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantOK     bool
		wantStatus TurnStatus
		wantReply  string
	}{
		{
			name:       "clean result line",
			in:         `{"status":"completed","reply":"hi","harnessSessionID":"s1","transcript_entries":[],"working_dir":"/w"}`,
			wantOK:     true,
			wantStatus: StatusCompleted,
			wantReply:  "hi",
		},
		{
			name: "result preceded by banner/log noise",
			in: "harness v1.2.3 starting\n" +
				"WARN: something\n" +
				`{"status":"completed","reply":"ok","harnessSessionID":"s2","transcript_entries":[],"working_dir":"/w"}`,
			wantOK:     true,
			wantStatus: StatusCompleted,
			wantReply:  "ok",
		},
		{
			name: "non-JSON tail after result",
			in: `{"status":"errored","reply":"","harnessSessionID":"","transcript_entries":[],"working_dir":"/w","reason":"boom"}` + "\n" +
				"process exited with code 1\n",
			wantOK:     true,
			wantStatus: StatusErrored,
			wantReply:  "",
		},
		{
			name: "truncated final line after result",
			in: `{"status":"completed","reply":"done","harnessSessionID":"s3","transcript_entries":[],"working_dir":"/w"}` + "\n" +
				`{"status":"comp`,
			wantOK:     true,
			wantStatus: StatusCompleted,
			wantReply:  "done",
		},
		{
			name:   "bare scalar rejected",
			in:     "42\n\"hello\"\ntrue",
			wantOK: false,
		},
		{
			name:   "bare array rejected",
			in:     `[{"status":"completed"}]`,
			wantOK: false,
		},
		{
			name: "multiple objects last wins",
			in: `{"status":"errored","reply":"first","harnessSessionID":"a","transcript_entries":[],"working_dir":"/w"}` + "\n" +
				`{"status":"completed","reply":"second","harnessSessionID":"b","transcript_entries":[],"working_dir":"/w"}`,
			wantOK:     true,
			wantStatus: StatusCompleted,
			wantReply:  "second",
		},
		{
			name:   "empty input",
			in:     "",
			wantOK: false,
		},
		{
			name:   "whitespace-only input",
			in:     "\n  \n\t\n",
			wantOK: false,
		},
		{
			name:   "malformed only",
			in:     "not json\n{oops\n{\"status\":",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseLastJSONLine([]byte(tt.in))
			checkParsedResult(t, got, ok, tt.wantOK, tt.wantStatus, tt.wantReply)
		})
	}
}

// checkParsedResult asserts a ParseLastJSONLine result against expectations:
// the ok flag, a nil result when parsing was expected to fail, and the
// recovered status/reply otherwise.
func checkParsedResult(t *testing.T, got *StructuredTurnResult, ok, wantOK bool, wantStatus TurnStatus, wantReply string) {
	t.Helper()
	if ok != wantOK {
		t.Fatalf("ok = %v, want %v", ok, wantOK)
	}
	if !wantOK {
		if got != nil {
			t.Fatalf("got = %+v, want nil", got)
		}
		return
	}
	if got == nil {
		t.Fatal("got nil result with ok=true")
	}
	if got.Status != wantStatus {
		t.Errorf("Status = %q, want %q", got.Status, wantStatus)
	}
	if got.Reply != wantReply {
		t.Errorf("Reply = %q, want %q", got.Reply, wantReply)
	}
}

func TestStructuredTurnResultRoundTrip(t *testing.T) {
	for _, status := range []TurnStatus{StatusCompleted, StatusErrored, StatusDeadline, StatusStartupError} {
		t.Run(string(status), func(t *testing.T) {
			checkRoundTrip(t, status)
		})
	}
}

// checkRoundTrip marshals a fully-populated StructuredTurnResult and asserts that
// ParseLastJSONLine recovers every field for the given status.
func checkRoundTrip(t *testing.T, status TurnStatus) {
	t.Helper()
	in := StructuredTurnResult{
		Status:            status,
		Reply:             "r",
		HarnessSessionID:  "sess",
		TranscriptEntries: []transcript.Event{{Seq: 1, Role: transcript.RoleUser, Type: transcript.EventText, Text: "hi"}},
		WorkingDir:        "/work",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, ok := ParseLastJSONLine(data)
	if !ok || got == nil {
		t.Fatalf("ParseLastJSONLine failed to round-trip %q", status)
	}
	if got.Status != status {
		t.Errorf("Status = %q, want %q", got.Status, status)
	}
	if got.WorkingDir != "/work" || got.HarnessSessionID != "sess" || got.Reply != "r" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if len(got.TranscriptEntries) != 1 || got.TranscriptEntries[0].Seq != 1 {
		t.Errorf("transcript entries not preserved: %+v", got.TranscriptEntries)
	}
}

// TestJSONTagSpellings asserts the exact frozen JSON key spellings and that the
// four optional keys are absent when unset while the five required keys are
// always present.
func TestJSONTagSpellings(t *testing.T) {
	data, err := json.Marshal(StructuredTurnResult{Status: StatusCompleted})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	required := []string{"status", "reply", "harnessSessionID", "transcript_entries", "working_dir"}
	for _, k := range required {
		if _, ok := raw[k]; !ok {
			t.Errorf("required key %q missing from %s", k, data)
		}
	}
	for _, k := range []string{"reason", "transcript_error", "usage", "permission_mode"} {
		if _, ok := raw[k]; ok {
			t.Errorf("optional key %q must be absent when unset, got %s", k, data)
		}
	}
	if len(raw) != len(required) {
		t.Errorf("unexpected keys: got %v, want exactly %v", keys(raw), required)
	}
}

// TestOptionalKeysPresentWhenSet confirms the omitempty keys serialize with their
// frozen snake_case spellings when populated.
func TestOptionalKeysPresentWhenSet(t *testing.T) {
	data, err := json.Marshal(StructuredTurnResult{
		Status:          StatusErrored,
		Reason:          "boom",
		TranscriptError: "read failed",
		PermissionMode:  "bypass",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["reason"]; !ok {
		t.Errorf("reason missing when set: %s", data)
	}
	if _, ok := raw["transcript_error"]; !ok {
		t.Errorf("transcript_error missing when set: %s", data)
	}
	if got, ok := raw["permission_mode"]; !ok {
		t.Errorf("permission_mode missing when set: %s", data)
	} else if string(got) != `"bypass"` {
		t.Errorf("permission_mode = %s, want %q", got, "bypass")
	}
}

// A result line from an OLD producer — one that predates permission_mode — must
// still parse, with the field recovering as "" (the "no canonical rung could be
// named" reading, NOT "safe"). The backward-compatible direction is cheap, but
// it is the one a host upgrade depends on.
func TestParseLastJSONLineOmittedPermissionMode(t *testing.T) {
	const line = `{"status":"completed","reply":"hi","harnessSessionID":"s1","transcript_entries":[],"working_dir":"/w"}`
	got, ok := ParseLastJSONLine([]byte(line))
	if !ok || got == nil {
		t.Fatalf("ParseLastJSONLine failed on a pre-permission_mode line")
	}
	if got.PermissionMode != "" {
		t.Errorf("PermissionMode = %q, want \"\" for a line that omits the key", got.PermissionMode)
	}
}

// TestUsagePresentWhenSet confirms that a non-nil Usage emits the frozen `usage`
// key with ALL FIVE nested keys present — including zero-valued ones (the inner
// fields carry no omitempty), which guards byte-parity with MH's
// usageToPublicJSON.
func TestUsagePresentWhenSet(t *testing.T) {
	// A Codex-shaped usage: CacheCreationInputTokens and ReasoningOutputTokens
	// are zero, yet must still serialize.
	data, err := json.Marshal(StructuredTurnResult{
		Status: StatusCompleted,
		Usage: &transcript.Usage{
			InputTokens:              100,
			OutputTokens:             50,
			CacheReadInputTokens:     20,
			CacheCreationInputTokens: 0,
			ReasoningOutputTokens:    0,
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usageRaw, ok := raw["usage"]
	if !ok {
		t.Fatalf("usage missing when set: %s", data)
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(usageRaw, &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	for _, k := range []string{
		"input_tokens",
		"output_tokens",
		"cache_read_input_tokens",
		"cache_creation_input_tokens",
		"reasoning_output_tokens",
	} {
		if _, ok := usage[k]; !ok {
			t.Errorf("nested usage key %q missing (zero-valued keys must still serialize): %s", k, usageRaw)
		}
	}
	if len(usage) != 5 {
		t.Errorf("usage should have exactly 5 keys, got %d: %s", len(usage), usageRaw)
	}
}

func TestConstants(t *testing.T) {
	if ExitOK != 0 || ExitError != 1 || ExitUsage != 2 || ExitDeadline != 124 {
		t.Errorf("exit codes drifted: OK=%d Error=%d Usage=%d Deadline=%d", ExitOK, ExitError, ExitUsage, ExitDeadline)
	}
	if DeadlineLine != "harness-wrapper run: context deadline exceeded" {
		t.Errorf("DeadlineLine drifted: %q", DeadlineLine)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
