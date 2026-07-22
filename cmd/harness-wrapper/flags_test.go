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

	tests = append(
		tests,
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
			assertParsedArgs(t, tc.wantErrSubs, tc.wantName, tc.wantHArgs, parsed.HarnessName, parsed.HarnessArgs, err)
		})
	}
}

// assertParsedArgs checks a parseHarnessWrapperArgs outcome: when wantErrSubs is
// set it asserts the error contains it, otherwise it asserts a nil error and the
// expected harness name/args.
func assertParsedArgs(t *testing.T, wantErrSubs, wantName string, wantHArgs []string, gotName string, gotHArgs []string, err error) {
	t.Helper()
	if wantErrSubs != "" {
		if err == nil {
			t.Fatalf("expected error containing %q, got nil", wantErrSubs)
		}
		if !strings.Contains(err.Error(), wantErrSubs) {
			t.Fatalf("error %q does not contain %q", err.Error(), wantErrSubs)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotName != wantName {
		t.Errorf("HarnessName = %q, want %q", gotName, wantName)
	}
	if !reflect.DeepEqual(gotHArgs, wantHArgs) {
		t.Errorf("HarnessArgs = %v, want %v", gotHArgs, wantHArgs)
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

func TestParseHarnessWrapperArgs_EffortFlagPropagated(t *testing.T) {
	parsed, err := parseHarnessWrapperArgs([]string{"--effort", "high", "claude", "--", "-p", "prompt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.Effort != "high" {
		t.Errorf("Effort = %q, want high", parsed.Effort)
	}
	if parsed.HarnessName != "claude" {
		t.Errorf("HarnessName = %q, want claude", parsed.HarnessName)
	}
	if !reflect.DeepEqual(parsed.HarnessArgs, []string{"-p", "prompt"}) {
		t.Errorf("HarnessArgs = %v, want [-p prompt]", parsed.HarnessArgs)
	}
}

func TestParseHarnessWrapperArgs_PermissionModeFlagPropagated(t *testing.T) {
	parsed, err := parseHarnessWrapperArgs([]string{"--permission-mode", "ask", "claude", "--", "-p", "prompt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.PermissionMode != "ask" {
		t.Errorf("PermissionMode = %q, want ask", parsed.PermissionMode)
	}
	if parsed.HarnessName != "claude" {
		t.Errorf("HarnessName = %q, want claude", parsed.HarnessName)
	}
	if !reflect.DeepEqual(parsed.HarnessArgs, []string{"-p", "prompt"}) {
		t.Errorf("HarnessArgs = %v, want [-p prompt]", parsed.HarnessArgs)
	}
}

// TestParseHarnessWrapperArgs_SandboxDefaultsPermissionModeComposition freezes
// the composition rule. Only a bypass-class rung composes with
// --sandbox-defaults; every other value is contradictory (it asks for a
// restriction while --sandbox-defaults asks for none) and is rejected at parse
// time with exit 2.
//
// The check is asserted HERE, at parseHarnessWrapperArgs, precisely because it
// touches neither PATH nor a harness binary: like the --trace-file and
// --tmux-session exclusions it is deterministic on any machine, and it fires
// before resolveHarness and before any tmux session could be spawned. No test
// here sets HARNESS_BINARY_* or installs a fake harness.
func TestParseHarnessWrapperArgs_SandboxDefaultsPermissionModeComposition(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		wantErr bool
	}{
		{
			// A restrictive rung contradicts --sandbox-defaults outright.
			name: "manual is rejected", mode: "manual", wantErr: true,
		},
		{
			// The hole the NARROWED predicate closes: danger-full-access is
			// codex's native bypass spelling, and wrapper.IsBypassPermissionMode
			// deliberately returns false for it. A looser "does this look like
			// bypass?" test would have accepted it, pairing --sandbox-defaults
			// with a mode that can never reach the claude-only compose path.
			name: "danger-full-access is rejected", mode: "danger-full-access", wantErr: true,
		},
		{"plan is rejected", "plan", true},
		{"auto is rejected", "auto", true},
		{
			// PARSE-LEVEL ONLY. A clean parse here says the flag pair is
			// coherent; it does NOT say this invocation runs. Passthrough
			// still rejects --sandbox-defaults later at main.go's
			// SandboxDefaults check — see main_test.go. Under run /
			// structured-run the same parse proceeds to the compose path.
			name: "bypass parses cleanly", mode: "bypass", wantErr: false,
		},
		{
			// Same, for claude's native spelling of the bypass rung.
			// Parse-level only, as above.
			name: "bypassPermissions parses cleanly", mode: "bypassPermissions", wantErr: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseHarnessWrapperArgs([]string{"--sandbox-defaults", "--permission-mode", tc.mode, "claude", "--"})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if parsed.PermissionMode != tc.mode || !parsed.SandboxDefaults {
					t.Fatalf("parsed = {mode:%q sandbox:%v}, want {%q true}", parsed.PermissionMode, parsed.SandboxDefaults, tc.mode)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected rejection of --sandbox-defaults with --permission-mode %s", tc.mode)
			}
			want := "harness-wrapper: --sandbox-defaults is incompatible with --permission-mode " + tc.mode +
				" (only --permission-mode bypass composes with it)"
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}
		})
	}
}

// TestParseHarnessWrapperArgs_SandboxDefaultsAloneStillParses guards the
// composition check against over-reach: --sandbox-defaults with no
// --permission-mode at all is the pre-existing, untouched invocation.
func TestParseHarnessWrapperArgs_SandboxDefaultsAloneStillParses(t *testing.T) {
	parsed, err := parseHarnessWrapperArgs([]string{"--sandbox-defaults", "claude", "--"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parsed.SandboxDefaults || parsed.PermissionMode != "" {
		t.Errorf("parsed = {sandbox:%v mode:%q}, want {true \"\"}", parsed.SandboxDefaults, parsed.PermissionMode)
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
