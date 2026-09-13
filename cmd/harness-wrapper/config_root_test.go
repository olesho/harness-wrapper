package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	cc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
)

// TestStructuredTranscriptUsesConfigRoot: structured-run launches the harness
// with the process environment (minus nesting markers), so a per-agent
// CLAUDE_CONFIG_DIR there is where the transcript is written — and where the
// in-guest read must look, not the default HOME.
func TestStructuredTranscriptUsesConfigRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, wd := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	dir := filepath.Join(root, "projects", cc.EncodedCWD(wd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("{\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"profile reply\"}]}}\n")
	if err := os.WriteFile(filepath.Join(dir, "review-session.jsonl"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := readStructuredTranscript("claude", "review-session", wd)
	if err != nil {
		t.Fatalf("transcript exists in configured profile but structured-run failed: %v", err)
	}
	if len(events) != 1 || events[0].Text != "profile reply" {
		t.Fatalf("wrong transcript: %+v", events)
	}
}

// configRootJSONL is a claude session log with a text reply and per-message
// usage, so one staged file proves both in-guest reads: transcript and usage.
const configRootJSONL = `{"type":"assistant","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"from the agent's own root"}],"usage":{"input_tokens":7,"output_tokens":3,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}
`

// TestStructuredRun_TranscriptAndUsageFromConfigRoot drives a whole
// structured-run turn for a profiled agent: HOME holds nothing, and the only
// transcript is under the agent's own CLAUDE_CONFIG_DIR — absolute, or relative
// to the harness child's working directory (LOOM_WORKTREE_PATH), which differs
// from this process's cwd. Both in-guest reads must find it.
func TestStructuredRun_TranscriptAndUsageFromConfigRoot(t *testing.T) {
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}
	const sessionID = "aaaaaaaa-1111-2222-3333-bbbbbbbbbbbb"

	for _, tc := range []struct {
		name string
		// root returns the CLAUDE_CONFIG_DIR value to launch with and the
		// directory claude would write under for it.
		root func(t *testing.T, wd string) (value, onDisk string)
	}{
		{
			name: "absolute root",
			root: func(t *testing.T, _ string) (string, string) {
				r := filepath.Join(t.TempDir(), "agent-claude")
				return r, r
			},
		},
		{
			name: "relative root",
			root: func(_ *testing.T, wd string) (string, string) {
				return ".agent-claude", filepath.Join(wd, ".agent-claude")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			wd := t.TempDir()
			value, onDisk := tc.root(t, wd)
			projDir := filepath.Join(onDisk, "projects", cc.EncodedCWD(wd))
			if err := os.MkdirAll(projDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projDir, sessionID+".jsonl"), []byte(configRootJSONL), 0o600); err != nil {
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
			t.Setenv("HARNESS_BINARY_CLAUDE", fakeBin)
			t.Setenv(fakeharness.EnvVar, writeFakeScript(t, t.TempDir(), script))
			t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "30s")
			t.Setenv("LOOM_WORKTREE_PATH", wd)
			t.Setenv("CLAUDE_CONFIG_DIR", value)
			t.Chdir(t.TempDir()) // the wrapper's cwd is neither wd nor the root

			out, code := captureStructuredRun(t, "use my own profile", []string{"claude", "--"})
			if code != turnproto.ExitOK {
				t.Fatalf("exit code = %d, want %d; stdout:\n%s", code, turnproto.ExitOK, out)
			}
			res, ok := turnproto.ParseLastJSONLine([]byte(out))
			if !ok {
				t.Fatalf("no JSON result line in stdout:\n%s", out)
			}
			if res.TranscriptError != "" {
				t.Errorf("transcript_error = %q, want the agent-root transcript read", res.TranscriptError)
			}
			found := false
			for _, e := range res.TranscriptEntries {
				if strings.Contains(e.Text, "from the agent's own root") {
					found = true
				}
			}
			if !found {
				t.Errorf("transcript_entries = %+v, want the staged agent-root transcript", res.TranscriptEntries)
			}
			want := transcript.Usage{InputTokens: 7, OutputTokens: 3}
			if res.Usage == nil || *res.Usage != want {
				t.Errorf("usage = %+v, want %+v from the agent-root transcript", res.Usage, want)
			}
		})
	}
}

// codexRootRollout is a codex rollout with an assistant reply and a cumulative
// token_count, so it feeds both the transcript read and the usage read.
func codexRootRollout(sessionID, cwd string) string {
	return `{"timestamp":"2026-05-14T12:00:00Z","type":"session_meta","payload":{"session_id":"` + sessionID + `","cwd":"` + cwd + `","cli_version":"0.154.0"}}
{"timestamp":"2026-05-14T12:00:01Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"codex from the agent's own home"}]}}
{"timestamp":"2026-05-14T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":11,"cached_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0,"total_tokens":16}}}}
`
}

// TestStructuredRun_CodexTranscriptAndUsageFromConfigRoot is the codex twin: a
// profiled codex agent's rollouts live under its own CODEX_HOME, absolute or
// relative to the harness child's working directory.
func TestStructuredRun_CodexTranscriptAndUsageFromConfigRoot(t *testing.T) {
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}
	const sessionID = "0199cccc-dddd-7eee-8fff-000011112222"

	for _, tc := range []struct {
		name string
		root func(t *testing.T, wd string) (value, onDisk string)
	}{
		{
			name: "absolute root",
			root: func(t *testing.T, _ string) (string, string) {
				r := filepath.Join(t.TempDir(), "agent-codex")
				return r, r
			},
		},
		{
			name: "relative root",
			root: func(_ *testing.T, wd string) (string, string) {
				return ".agent-codex", filepath.Join(wd, ".agent-codex")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			wd := t.TempDir()
			value, onDisk := tc.root(t, wd)
			dir := filepath.Join(onDisk, "sessions", "2026", "05", "14")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			rollout := filepath.Join(dir, "rollout-2026-05-14T12-00-00-"+sessionID+".jsonl")
			if err := os.WriteFile(rollout, []byte(codexRootRollout(sessionID, wd)), 0o600); err != nil {
				t.Fatal(err)
			}

			script := fakeharness.New("codex").
				Session(sessionID).
				Idle().
				AwaitSubmit().
				CodexWorking(30, "Thinking").
				CodexReply(40, "codex reply: "+fakeharness.PromptRef()).
				StayAliveUntilStopped().
				Build()
			t.Setenv("HARNESS_BINARY_CODEX", fakeBin)
			t.Setenv(fakeharness.EnvVar, writeFakeScript(t, t.TempDir(), script))
			t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "30s")
			t.Setenv("LOOM_WORKTREE_PATH", wd)
			t.Setenv("CODEX_HOME", value)
			t.Chdir(t.TempDir())

			out, code := captureStructuredRun(t, "use my own codex home", []string{"codex", "--"})
			if code != turnproto.ExitOK {
				t.Fatalf("exit code = %d, want %d; stdout:\n%s", code, turnproto.ExitOK, out)
			}
			res, ok := turnproto.ParseLastJSONLine([]byte(out))
			if !ok {
				t.Fatalf("no JSON result line in stdout:\n%s", out)
			}
			if res.TranscriptError != "" {
				t.Errorf("transcript_error = %q, want the agent-home rollout read", res.TranscriptError)
			}
			found := false
			for _, e := range res.TranscriptEntries {
				if strings.Contains(e.Text, "codex from the agent's own home") {
					found = true
				}
			}
			if !found {
				t.Errorf("transcript_entries = %+v, want the staged agent-home rollout", res.TranscriptEntries)
			}
			if res.Usage == nil || res.Usage.InputTokens != 11 || res.Usage.OutputTokens != 5 {
				t.Errorf("usage = %+v, want input 11 / output 5 from the agent-home rollout", res.Usage)
			}
		})
	}
}
