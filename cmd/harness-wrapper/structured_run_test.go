package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
)

// TestStructuredRun_GoldenCompleted drives a scripted fake claude turn through
// the new structured-run subcommand — injected via the HARNESS_BINARY_CLAUDE
// override so the run is hermetic — and asserts the single emitted JSON line's
// shape: a COMPLETED turn maps to status "completed" (NOT the raw Turn.State
// "complete"), the reply echoes the prompt, and the exit code is 0.
//
// The fakeharness is a PTY/screen replay: it writes no claude-layout JSONL, so
// the transcript Reader.Read errors on the absent session. That error is the
// EXPECTED, tolerated path here — transcript_entries stays empty and
// transcript_error is recorded rather than failing the run.
func TestStructuredRun_GoldenCompleted(t *testing.T) {
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}

	const prompt = "ship the turn API"
	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
		StayAliveUntilStopped().
		Build()
	scriptData, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(scriptPath, scriptData, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	t.Setenv("HARNESS_BINARY_CLAUDE", fakeBin)
	t.Setenv(fakeharness.EnvVar, scriptPath)
	t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "30s")
	// Run in a temp working dir so the transcript read has no real corpus to find.
	t.Chdir(t.TempDir())

	out, code := captureStructuredRun(t, prompt, []string{"claude", "--"})

	res, ok := turnproto.ParseLastJSONLine([]byte(out))
	if !ok {
		t.Fatalf("no JSON result line in stdout:\n%s", out)
	}

	if res.Status != turnproto.StatusCompleted {
		t.Errorf("status = %q, want %q (a Turn.State passthrough would emit %q)",
			res.Status, turnproto.StatusCompleted, "complete")
	}
	if code != turnproto.ExitOK {
		t.Errorf("exit code = %d, want %d", code, turnproto.ExitOK)
	}
	if !strings.Contains(res.Reply, "assistant reply: "+prompt) {
		t.Errorf("reply = %q, want it to contain %q", res.Reply, "assistant reply: "+prompt)
	}
	if len(res.TranscriptEntries) != 0 {
		t.Errorf("transcript_entries = %v, want empty (fakeharness writes no JSONL)", res.TranscriptEntries)
	}
	if res.TranscriptError == "" {
		t.Errorf("transcript_error is empty; want the tolerated absent-session read error recorded")
	}
	if res.WorkingDir == "" {
		t.Errorf("working_dir is empty")
	}
}

// TestStructuredRun_SandboxDefaultsInjection asserts what the SPAWNED harness
// actually receives: with --sandbox-defaults the claude argv carries
// --dangerously-skip-permissions and the env carries IS_SANDBOX=1; without the
// flag, neither is injected. The fakeharness records neither argv nor env, so
// HARNESS_BINARY_CLAUDE points at a small recording shim that dumps "$@" and
// IS_SANDBOX to a file and then EXECs the fakeharness binary — exec (not fork)
// keeps the harness a direct child of the PTY, preserving wrapper.Run's
// process-tree and graceful-quit expectations. The injected flag traveling
// through "$@" into the fakeharness is harmless: cmd/fakeharness reads only
// $FAKEHARNESS_SCRIPT (inherited by the shim) and never touches os.Args.
func TestStructuredRun_SandboxDefaultsInjection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script recording shim is Unix-only")
	}
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}

	for _, tc := range []struct {
		name       string
		args       []string
		wantInject bool
	}{
		{"with sandbox-defaults", []string{"--sandbox-defaults", "claude", "--"}, true},
		{"without sandbox-defaults", []string{"claude", "--"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const prompt = "ship the turn API"
			script := fakeharness.New("claude-code").
				Idle().
				AwaitSubmit().
				Working(30, "Working").
				Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
				StayAliveUntilStopped().
				Build()
			scriptData, err := json.Marshal(script)
			if err != nil {
				t.Fatalf("marshal script: %v", err)
			}
			dir := t.TempDir()
			scriptPath := filepath.Join(dir, "script.json")
			if err := os.WriteFile(scriptPath, scriptData, 0o600); err != nil {
				t.Fatalf("write script: %v", err)
			}

			recordPath := filepath.Join(dir, "record.txt")
			shimPath := filepath.Join(dir, "claude-shim")
			shim := fmt.Sprintf(`#!/bin/sh
{
  printf 'ARGS:'
  for a in "$@"; do printf ' %%s' "$a"; done
  printf '\n'
  printf 'IS_SANDBOX=%%s\n' "${IS_SANDBOX-unset}"
} > %q
exec %q "$@"
`, recordPath, fakeBin)
			if err := os.WriteFile(shimPath, []byte(shim), 0o700); err != nil {
				t.Fatalf("write shim: %v", err)
			}

			t.Setenv("HARNESS_BINARY_CLAUDE", shimPath)
			t.Setenv(fakeharness.EnvVar, scriptPath)
			t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "30s")
			t.Chdir(t.TempDir())

			out, code := captureStructuredRun(t, prompt, tc.args)
			if code != turnproto.ExitOK {
				t.Fatalf("exit code = %d, want %d; stdout:\n%s", code, turnproto.ExitOK, out)
			}

			rec, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatalf("read shim record: %v", err)
			}
			recorded := string(rec)
			if got := strings.Contains(recorded, "--dangerously-skip-permissions"); got != tc.wantInject {
				t.Errorf("argv has --dangerously-skip-permissions = %v, want %v; record:\n%s",
					got, tc.wantInject, recorded)
			}
			if got := strings.Contains(recorded, "IS_SANDBOX=1"); got != tc.wantInject {
				t.Errorf("env has IS_SANDBOX=1 = %v, want %v; record:\n%s",
					got, tc.wantInject, recorded)
			}
		})
	}
}

// captureStructuredRun feeds prompt on a piped stdin, captures stdout, and runs
// runStructuredRun(args), returning the captured stdout and the exit code.
func captureStructuredRun(t *testing.T, prompt string, args []string) (string, int) {
	t.Helper()

	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptPath, []byte(prompt), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	stdinFile, err := os.Open(promptPath)
	if err != nil {
		t.Fatalf("open prompt: %v", err)
	}
	defer func() { _ = stdinFile.Close() }()

	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	origStdin, origStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinFile, wp
	defer func() { os.Stdin, os.Stdout = origStdin, origStdout }()

	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := rp.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		done <- sb.String()
	}()

	code := runStructuredRun(args)

	_ = wp.Close()
	os.Stdout = origStdout
	out := <-done
	_ = rp.Close()
	return out, code
}

// TestStructuredTranscript_ClaudeFidelity covers the fidelity gap the
// fakeharness golden cannot: with a REAL claude-layout JSONL on disk and a valid
// session id + working dir, the harness-name-selected Reader.Read surfaces
// tool-call events (tool_use / tool_result carrying tool_name / tool_input /
// output), not just role+text turns.
func TestStructuredTranscript_ClaudeFidelity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	const sessionID = "sess-fidelity"
	wd := "/some/work/dir"
	projDir := filepath.Join(home, ".claude", "projects", claudecode.EncodedCWD(wd))
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"running"},{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"ls"}}]},"timestamp":"2026-05-14T12:00:00Z"}
{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"file.go"}]}]},"timestamp":"2026-05-14T12:00:01Z"}
`
	if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := readStructuredTranscript("claude", sessionID, wd)
	if err != nil {
		t.Fatalf("readStructuredTranscript: %v", err)
	}
	assertToolCallFidelity(t, entries)
}

// TestStructuredTranscript_CodexFidelity is the codex twin: the short name
// "codex" must select codex.New(), and its function_call / function_call_output
// entries must map to tool_use / tool_result.
func TestStructuredTranscript_CodexFidelity(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	const sessionID = "abc-def-ghi"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "05", "14")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"2026-05-14T12:00:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}
{"timestamp":"2026-05-14T12:00:02Z","type":"response_item","payload":{"type":"function_call","name":"shell","call_id":"call_1","arguments":"{\"cmd\":\"ls\"}"}}
{"timestamp":"2026-05-14T12:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"file.go"}}
`
	path := filepath.Join(dir, "rollout-2026-05-14T12-00-00-"+sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := readStructuredTranscript("codex", sessionID, "/unused")
	if err != nil {
		t.Fatalf("readStructuredTranscript: %v", err)
	}
	assertToolCallFidelity(t, entries)
}

// assertToolCallFidelity asserts the event stream carries a populated tool_use
// and a populated tool_result — the tool-call detail res.History would fold away.
func assertToolCallFidelity(t *testing.T, entries []transcript.Event) {
	t.Helper()
	var toolUse, toolResult *transcript.Event
	for i := range entries {
		switch entries[i].Type {
		case transcript.EventToolUse:
			toolUse = &entries[i]
		case transcript.EventToolResult:
			toolResult = &entries[i]
		}
	}
	if toolUse == nil {
		t.Fatalf("no tool_use event in transcript entries: %+v", entries)
	}
	if toolUse.ToolName == "" || len(toolUse.ToolInput) == 0 {
		t.Errorf("tool_use missing tool_name/tool_input: %+v", *toolUse)
	}
	if toolResult == nil {
		t.Fatalf("no tool_result event in transcript entries: %+v", entries)
	}
	if toolResult.Output == "" {
		t.Errorf("tool_result missing output: %+v", *toolResult)
	}
}
