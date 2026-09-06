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
	"github.com/olesho/harness-wrapper/pkg/wrapper"
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

	// Ambient-IS_SANDBOX hygiene, required for the two negative rows to mean
	// anything: cleanedEnv() starts from os.Environ() and strips only Claude
	// Code's nesting markers, so the shim's ${IS_SANDBOX-unset} otherwise
	// reflects the TEST HOST. In a guest that already exports IS_SANDBOX=1 —
	// exactly the deployment this feature targets — the bypass-alone row would
	// fail spuriously and the sandbox-defaults row would pass for the WRONG
	// reason (hasEnvKey suppresses the injection, yet IS_SANDBOX=1 still shows
	// up in the record). Unset, don't t.Setenv("IS_SANDBOX", ""): that DEFINES
	// the key, which hasEnvKey treats as present.
	if prev, ok := os.LookupEnv("IS_SANDBOX"); ok {
		if err := os.Unsetenv("IS_SANDBOX"); err != nil {
			t.Fatalf("unset IS_SANDBOX: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Setenv("IS_SANDBOX", prev); err != nil {
				t.Errorf("restore IS_SANDBOX: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name string
		args []string
		// The three expectations are independent on purpose: a regression that
		// swaps one permission spelling for the other must not be able to pass.
		wantSkipFlag   bool // --dangerously-skip-permissions in argv
		wantRungFlag   bool // --permission-mode bypassPermissions in argv
		wantSandboxEnv bool // IS_SANDBOX=1 in the spawned env
	}{
		{"with sandbox-defaults", []string{"--sandbox-defaults", "claude", "--"}, true, false, true},
		{"without sandbox-defaults", []string{"claude", "--"}, false, false, false},
		// The headline invariant: a RUNG NEVER IMPLIES THE ENV HALF. Observed on
		// a really spawned process, not inferred from a parse result.
		{"bypass alone adds no sandbox env", []string{"--permission-mode", "bypass", "claude", "--"}, false, true, false},
		// Compose: env half from --sandbox-defaults, arg half from pkg/wrapper,
		// and exactly one permission directive between them.
		{"compose: env half plus the rung", []string{"--sandbox-defaults", "--permission-mode", "bypass", "claude", "--"}, false, true, true},
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
			if got := strings.Contains(recorded, "--dangerously-skip-permissions"); got != tc.wantSkipFlag {
				t.Errorf("argv has --dangerously-skip-permissions = %v, want %v; record:\n%s",
					got, tc.wantSkipFlag, recorded)
			}
			if got := strings.Contains(recorded, "--permission-mode bypassPermissions"); got != tc.wantRungFlag {
				t.Errorf("argv has --permission-mode bypassPermissions = %v, want %v; record:\n%s",
					got, tc.wantRungFlag, recorded)
			}
			// Whole recorded LINES, not bare substrings: the record is exactly
			// one "ARGS: …\n" line followed by one "IS_SANDBOX=…\n" line, and a
			// bare Contains("IS_SANDBOX=1") would also match an ambient
			// IS_SANDBOX=10.
			wantLine, otherLine := "\nIS_SANDBOX=1\n", "\nIS_SANDBOX=unset\n"
			if !tc.wantSandboxEnv {
				wantLine, otherLine = otherLine, wantLine
			}
			if !strings.Contains(recorded, wantLine) || strings.Contains(recorded, otherLine) {
				t.Errorf("env: want %q in the record and not %q; record:\n%s",
					strings.TrimSpace(wantLine), strings.TrimSpace(otherLine), recorded)
			}
			// Presence booleans alone do not establish "exactly one permission
			// directive" — count it.
			if tc.wantRungFlag {
				if n := strings.Count(recorded, "--permission-mode "); n != 1 {
					t.Errorf("argv has %d occurrences of --permission-mode, want exactly 1; record:\n%s",
						n, recorded)
				}
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

// TestStructuredRun_UsagePopulatedBestEffort drives a full structured-run turn
// with a REAL claude-layout JSONL staged at the session's on-disk slot — the same
// staging TestStructuredTranscript_ClaudeFidelity uses, but the transcript now
// carries message.usage lines. It asserts the emitted StructuredTurnResult
// carries `usage` (summed + deduped by message.id) alongside the reply, proving
// the best-effort ReadUsage hook fires. It also proves the ABSENT-usage twin: a
// transcript with no usage lines leaves `usage` omitted, without changing status
// or exit code.
func TestStructuredRun_UsagePopulatedBestEffort(t *testing.T) {
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}

	for _, tc := range []struct {
		name      string
		jsonl     string
		wantUsage *transcript.Usage
	}{
		{
			name: "with usage",
			// Two distinct API calls, each repeated across content-block lines that
			// share one message.id — dedup-by-id + SUM must apply.
			jsonl: `{"type":"assistant","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}
{"type":"assistant","uuid":"a2","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"t1"}],"usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":5,"cache_creation_input_tokens":3}}}
{"type":"assistant","uuid":"a3","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"y"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":0,"cache_creation_input_tokens":7}}}
`,
			wantUsage: &transcript.Usage{
				InputTokens:              110,
				OutputTokens:             22,
				CacheReadInputTokens:     5,
				CacheCreationInputTokens: 10,
			},
		},
		{
			name: "without usage",
			jsonl: `{"type":"assistant","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"x"}]}}
`,
			wantUsage: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const prompt = "ship the turn API"
			// The fakeharness reports this session id via its scripted resume hint
			// (which extraction matches with a UUID-shaped regex), so it must be a
			// UUID; stage the matching claude-layout JSONL under HOME at
			// <session>.jsonl so the in-guest ReadUsage locates it.
			const sessionID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			home := t.TempDir()
			t.Setenv("HOME", home)
			wd := t.TempDir()

			projDir := filepath.Join(home, ".claude", "projects", claudecode.EncodedCWD(wd))
			if err := os.MkdirAll(projDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(tc.jsonl), 0o644); err != nil {
				t.Fatal(err)
			}

			script := fakeharness.New("claude-code").
				Session(sessionID).
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
			t.Setenv("LOOM_WORKTREE_PATH", wd)

			out, code := captureStructuredRun(t, prompt, []string{"claude", "--"})
			if code != turnproto.ExitOK {
				t.Fatalf("exit code = %d, want %d; stdout:\n%s", code, turnproto.ExitOK, out)
			}
			res, ok := turnproto.ParseLastJSONLine([]byte(out))
			if !ok {
				t.Fatalf("no JSON result line in stdout:\n%s", out)
			}
			if res.Status != turnproto.StatusCompleted {
				t.Errorf("status = %q, want %q", res.Status, turnproto.StatusCompleted)
			}
			// A completed turn must carry a non-empty reply; the exact text is
			// incidental here (reply extraction may read it back from the staged
			// transcript) — this test's subject is the usage field.
			if res.Reply == "" {
				t.Errorf("reply is empty, want the completed turn's reply preserved")
			}

			// The `usage` key must be present iff usage was expected — assert on the
			// raw JSON so omitempty behavior (absent key, not a zero-value object) is
			// verified directly.
			hasUsageKey := strings.Contains(out, `"usage"`)
			if (tc.wantUsage != nil) != hasUsageKey {
				t.Errorf("usage key present = %v, want %v; stdout:\n%s", hasUsageKey, tc.wantUsage != nil, out)
			}
			if tc.wantUsage == nil {
				if res.Usage != nil {
					t.Errorf("usage = %+v, want nil (absent-usage transcript)", *res.Usage)
				}
			} else {
				if res.Usage == nil {
					t.Fatalf("usage is nil, want %+v", *tc.wantUsage)
				}
				if *res.Usage != *tc.wantUsage {
					t.Errorf("usage = %+v, want %+v", *res.Usage, *tc.wantUsage)
				}
			}
		})
	}
}

// TestStructuredRun_UsageAbsentOnUnreadableTranscript proves the best-effort
// contract's failure path end-to-end: when NO transcript is staged (the
// fakeharness writes none), the ReadUsage locate fails, `usage` is omitted, and
// neither the status nor the exit code changes — the reply survives intact.
func TestStructuredRun_UsageAbsentOnUnreadableTranscript(t *testing.T) {
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
	t.Chdir(t.TempDir())

	out, code := captureStructuredRun(t, prompt, []string{"claude", "--"})

	res, ok := turnproto.ParseLastJSONLine([]byte(out))
	if !ok {
		t.Fatalf("no JSON result line in stdout:\n%s", out)
	}
	if res.Status != turnproto.StatusCompleted {
		t.Errorf("status = %q, want %q (usage failure must not change status)", res.Status, turnproto.StatusCompleted)
	}
	if code != turnproto.ExitOK {
		t.Errorf("exit code = %d, want %d (usage failure must not change exit code)", code, turnproto.ExitOK)
	}
	if !strings.Contains(res.Reply, "assistant reply: "+prompt) {
		t.Errorf("reply = %q, want the reply preserved", res.Reply)
	}
	if res.Usage != nil {
		t.Errorf("usage = %+v, want nil (unreadable transcript)", *res.Usage)
	}
	if strings.Contains(out, `"usage"`) {
		t.Errorf("usage key present, want omitted; stdout:\n%s", out)
	}
}

// TestStructuredRun_InvalidPermissionModeIsStartupError freezes the
// structured-run surface for a bad --permission-mode: a normal
// turnproto.StructuredTurnResult line with status "startup_error", the
// wrapper.ErrInvalidConfig text in reason, and exit turnproto.ExitError.
//
// emitStartupError is deliberately NOT the mechanism here, and the payload is
// asserted rather than just the exit code because of it. The error propagates
// wrapper.Start -> chat.Open -> harness.RunTurn -> oneshot.RunOneShotDetailed,
// where Classify's default: arm sees res.Session.ID == "" and returns
// (StatusStartupError, err.Error()) with a NIL error — so structured_run.go's
// `if oerr != nil` guard never fires and the result travels the ordinary
// emitStructured path. Mode-VALUE validation itself lives in
// wrapper.validateConfig; the CLI never duplicates it.
//
// HARNESS_BINARY_CLAUDE points at an unexecutable placeholder on purpose:
// validation must reject the config BEFORE anything is spawned, so this test
// would fail loudly if that ordering ever inverted.
func TestStructuredRun_InvalidPermissionModeIsStartupError(t *testing.T) {
	placeholder := filepath.Join(t.TempDir(), "never-executed-claude")
	if err := os.WriteFile(placeholder, []byte("#!/bin/sh\nexit 99\n"), 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}
	t.Setenv("HARNESS_BINARY_CLAUDE", placeholder)
	t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "30s")
	t.Chdir(t.TempDir())

	out, code := captureStructuredRun(t, "a prompt", []string{"--permission-mode", "not-a-rung", "claude", "--"})

	res, ok := turnproto.ParseLastJSONLine([]byte(out))
	if !ok {
		t.Fatalf("no JSON result line in stdout:\n%s", out)
	}
	if res.Status != turnproto.StatusStartupError {
		t.Errorf("status = %q, want %q; stdout:\n%s", res.Status, turnproto.StatusStartupError, out)
	}
	if !strings.Contains(res.Reason, wrapper.ErrInvalidConfig.Error()) {
		t.Errorf("reason = %q, want it to contain %q", res.Reason, wrapper.ErrInvalidConfig.Error())
	}
	if !strings.Contains(res.Reason, "not-a-rung") {
		t.Errorf("reason = %q, want it to name the rejected mode", res.Reason)
	}
	if code != turnproto.ExitError {
		t.Errorf("exit code = %d, want %d", code, turnproto.ExitError)
	}
	if res.Reply != "" {
		t.Errorf("reply = %q, want empty for a startup error", res.Reply)
	}
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
