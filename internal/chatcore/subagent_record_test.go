package chatcore

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// TestRecordSubAgent records a turn that runs a foreground subagent into the
// corpus: the real claude binary against the local Messages API, which answers
// "AGENT TOOL sleep N" with an Agent tool_use whose subagent runs `sleep N` in
// Bash, so the subagent's progress rows stay on screen for N seconds
// (HW_SUBAGENT_SLEEP, default 30: longer than the idle-completion gap, so the
// recording covers a wait the fallback must not cut short). No account, no
// tokens:
//
//	HW_RECORD_SUBAGENT=1 go test ./pkg/chat -run RecordSubAgent -v
//
// It writes test/corpus/claude-code/subagent-tool (bytes.raw, meta.json,
// transcript.jsonl). The recording pins where claude paints a running
// subagent relative to the spinner and the composer box, which Busy reads.
func TestRecordSubAgent(t *testing.T) {
	if os.Getenv("HW_RECORD_SUBAGENT") != "1" {
		t.Skip("records the subagent corpus: set HW_RECORD_SUBAGENT=1 (needs the real claude binary; no tokens)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	version, _ := exec.Command(bin, "--version").Output()

	mock := mockapi.Start()
	defer mock.Close()

	const name = "subagent-tool"
	wd := filepath.Join("/tmp", "hw-rebake", name)
	if err := os.RemoveAll(wd); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	realWD, _ := filepath.EvalSymlinks(wd)
	cfg := t.TempDir()
	seed := func(file string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(cfg, file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seed(".claude.json", map[string]any{
		"hasCompletedOnboarding": true,
		"projects":               map[string]any{realWD: map[string]any{"hasTrustDialogAccepted": true}},
	})
	seed("settings.json", map[string]any{"permissions": map[string]any{"defaultMode": "bypassPermissions"}})

	conv, err := Open(context.Background(), Options{
		Harness:    chatClaudeCode,
		BinaryPath: bin,
		WorkingDir: wd,
		Args:       []string{"--dangerously-skip-permissions"},
		Env: append(harnessenv.Cleaned(),
			"CLAUDE_CONFIG_DIR="+cfg,
			"IS_SANDBOX=1",
			"ANTHROPIC_BASE_URL="+mock.URL(),
			"ANTHROPIC_AUTH_TOKEN=placeholder-for-a-local-api"),
		Store:                     newFakeStore(),
		KeepAliveOnClassification: true,
		InputPolicy: &InputPolicy{ByKind: map[string]Disposition{
			"trust_prompt":      {Kind: DispositionAnswer, OptionID: "proceed"},
			"bypass_acceptance": {Kind: DispositionAnswer, OptionID: "proceed"},
		}},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	secs := "30"
	if s := os.Getenv("HW_SUBAGENT_SLEEP"); s != "" {
		secs = s
	}
	prompt := "AGENT TOOL sleep " + secs
	start := time.Now()
	sendOneTurnWithin(t, conv, prompt, 2*time.Minute)
	turn := waitForTerminalTurn(t, conv, 3*time.Minute)
	t.Logf("turn %s after %s: %q (%s)", turn.State, time.Since(start).Round(time.Second), turn.Text, turn.Reason)
	time.Sleep(2 * time.Second) // let the settled frame paint
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := conv.Quit(ctx); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	_, _ = conv.Wrapper().Wait()
	raw := conv.Wrapper().RecentOutput()
	if len(raw) >= 64*1024 {
		t.Fatalf("the session wrote %d bytes, past the recent-output ring; the recording would be truncated", len(raw))
	}
	conv.mu.Lock()
	id := conv.session.HarnessID()
	conv.mu.Unlock()
	_ = conv.Close(ctx)

	out := filepath.Join("..", "..", "test", "corpus", "claude-code", corpusName(name))
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "bytes.raw"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.MarshalIndent(map[string]any{
		"harness":        "claude-code",
		"binary_version": strings.Fields(string(version))[0],
		"recorded_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"cols":           120,
		"rows":           40,
		"notes": "A turn that runs a subagent: the model calls the Agent tool (run_in_background false) and the subagent runs `sleep " + secs + "` in Bash. claude backgrounds the agent anyway (\"Backgrounded agent\"), answers the tool call, then holds the turn on \"✻ Waiting for 1 background agent to finish\" with no \"esc to interrupt\" in the footer, while the agent's row (\"◯ general-purpose  mock … Ns · ↓ N tokens\") is painted BELOW the composer box; once the agent's <task-notification> arrives it answers again and ends the turn." +
			platformNote() + " Recorded with `HW_RECORD_SUBAGENT=1 go test ./pkg/chat -run RecordSubAgent` " +
			"(internal/chatcore/subagent_record_test.go): the real claude against internal/mockapi, in " + wd +
			" with a fresh CLAUDE_CONFIG_DIR (onboarding done, the directory trusted, bypassPermissions) " +
			"and a placeholder ANTHROPIC_AUTH_TOKEN, so no account or tokens are involved.",
	}, "", " ")
	if err := os.WriteFile(filepath.Join(out, "meta.json"), append(meta, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg, "projects", transcriptcc.EncodedCWD(realWD), id+".jsonl")
	lines, err := reducedTranscript(path)
	if err != nil {
		t.Fatalf("transcript %s: %v", path, err)
	}
	if err := os.WriteFile(filepath.Join(out, "transcript.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
