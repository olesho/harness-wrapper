package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseHarnessWrapperArgs(t *testing.T) {
	tests := []struct {
		name        string
		in          []string
		wantName    string
		wantHArgs   []string
		wantErrSubs string
	}{
		{
			name:      "harness with no harness args",
			in:        []string{"claude", "--"},
			wantName:  "claude",
			wantHArgs: []string{},
		},
		{
			name:      "harness with one harness arg",
			in:        []string{"codex", "--", "--version"},
			wantName:  "codex",
			wantHArgs: []string{"--version"},
		},
		{
			name:      "harness with multiple harness args",
			in:        []string{"claude", "--", "--dangerously-skip-permissions", "."},
			wantName:  "claude",
			wantHArgs: []string{"--dangerously-skip-permissions", "."},
		},
		{
			name:        "missing separator",
			in:          []string{"claude", "--version"},
			wantErrSubs: "missing -- separator",
		},
		{
			name:        "empty input",
			in:          nil,
			wantErrSubs: "missing -- separator",
		},
		{
			name:        "no harness name before separator",
			in:          []string{"--", "args"},
			wantErrSubs: "missing harness name",
		},
		{
			name:        "multiple positional args before separator",
			in:          []string{"claude", "extra", "--"},
			wantErrSubs: "expected exactly one harness name",
		},
	}

	tests = append(tests,
		struct {
			name        string
			in          []string
			wantName    string
			wantHArgs   []string
			wantErrSubs string
		}{
			name:      "trace-file flag before harness name",
			in:        []string{"--trace-file", "/tmp/trace.log", "claude", "--", "--version"},
			wantName:  "claude",
			wantHArgs: []string{"--version"},
		},
	)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseHarnessWrapperArgs(tc.in)
			if tc.wantErrSubs != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErrSubs)
				}
				if !strings.Contains(err.Error(), tc.wantErrSubs) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubs)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if parsed.HarnessName != tc.wantName {
				t.Errorf("HarnessName = %q, want %q", parsed.HarnessName, tc.wantName)
			}
			if !reflect.DeepEqual(parsed.HarnessArgs, tc.wantHArgs) {
				t.Errorf("HarnessArgs = %v, want %v", parsed.HarnessArgs, tc.wantHArgs)
			}
		})
	}
}

func TestParseHarnessWrapperArgs_TraceFileFlagPropagated(t *testing.T) {
	parsed, err := parseHarnessWrapperArgs([]string{"--trace-file", "/var/log/trace.ndjson", "codex", "--"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.TraceFile != "/var/log/trace.ndjson" {
		t.Errorf("TraceFile = %q, want %q", parsed.TraceFile, "/var/log/trace.ndjson")
	}
	if parsed.TraceStderr {
		t.Errorf("TraceStderr should be false when only --trace-file is set")
	}
	if parsed.HarnessName != "codex" {
		t.Errorf("HarnessName = %q, want codex", parsed.HarnessName)
	}
}

func TestParseHarnessWrapperArgs_TraceStderrFlagPropagated(t *testing.T) {
	parsed, err := parseHarnessWrapperArgs([]string{"--trace-stderr", "codex", "--"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parsed.TraceStderr {
		t.Errorf("TraceStderr should be true when --trace-stderr is set")
	}
	if parsed.TraceFile != "" {
		t.Errorf("TraceFile should be empty when only --trace-stderr is set, got %q", parsed.TraceFile)
	}
}

func TestParseHarnessWrapperArgs_TraceFileAndStderrMutuallyExclusive(t *testing.T) {
	_, err := parseHarnessWrapperArgs([]string{"--trace-file", "/tmp/x", "--trace-stderr", "codex", "--"})
	if err == nil {
		t.Fatal("expected error when both --trace-file and --trace-stderr are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion, got: %v", err)
	}
}

func TestParseHarnessWrapperArgs_NoTraceFlagsByDefault(t *testing.T) {
	parsed, err := parseHarnessWrapperArgs([]string{"codex", "--"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.TraceFile != "" {
		t.Errorf("TraceFile should default to empty, got %q", parsed.TraceFile)
	}
	if parsed.TraceStderr {
		t.Errorf("TraceStderr should default to false")
	}
}
