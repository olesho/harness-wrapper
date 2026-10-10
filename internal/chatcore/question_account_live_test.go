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

	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	"github.com/olesho/harness-wrapper/pkg/harnessname"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// TestQuestionAccountLive has the real model ask a clarifying question with
// AskUserQuestion, through the screen driver, and answers it through Answer:
// the reply must name the answer. It spends one short turn on a small model:
//
//	HW_LIVE_ACCOUNT=1 HW_LIVE_TOKEN_FILE=<file holding a `claude setup-token` token> \
//	  go test ./internal/chatcore -run QuestionAccountLive -v
//
// The token is read from the file and passed only as CLAUDE_CODE_OAUTH_TOKEN
// in claude's environment; it is never logged. HW_LIVE_MODEL overrides the
// model (default haiku).
func TestQuestionAccountLive(t *testing.T) {
	if os.Getenv("HW_LIVE_ACCOUNT") != "1" {
		t.Skip("uses a real account: set HW_LIVE_ACCOUNT=1 and HW_LIVE_TOKEN_FILE (spends one short turn)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	tok, err := os.ReadFile(os.Getenv("HW_LIVE_TOKEN_FILE"))
	if err != nil || len(strings.TrimSpace(string(tok))) == 0 {
		t.Fatalf("HW_LIVE_TOKEN_FILE must name a file holding the token: %v", err)
	}
	model := os.Getenv("HW_LIVE_MODEL")
	if model == "" {
		model = "haiku"
	}

	root := t.TempDir()
	wd, cfg := filepath.Join(root, "work"), filepath.Join(root, "cfg")
	for _, d := range []string{wd, cfg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realWD, _ := filepath.EvalSymlinks(wd)
	writeJSON := func(path string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(filepath.Join(cfg, ".claude.json"), map[string]any{
		"hasCompletedOnboarding": true, "bypassPermissionsModeAccepted": true, "hasSeenAutoDefaultNudge": true,
		"projects": map[string]any{realWD: map[string]any{"hasTrustDialogAccepted": true}},
	})
	writeJSON(filepath.Join(cfg, "settings.json"), map[string]any{
		"permissions":                       map[string]any{"defaultMode": "bypassPermissions"},
		"skipDangerousModePermissionPrompt": true,
	})

	conv, err := Open(context.Background(), Options{
		Harness: harnessname.ClaudeCode, BinaryPath: bin, WorkingDir: wd, Model: model,
		Env: append(harnessenv.Cleaned(),
			"CLAUDE_CONFIG_DIR="+cfg,
			"CLAUDE_CODE_OAUTH_TOKEN="+strings.TrimSpace(string(tok)),
			"DISABLE_AUTOUPDATER=1"),
		Store:                     newFakeStore(),
		KeepAliveOnClassification: true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = conv.Close(context.Background()) }()

	terminal := make(chan Turn, 1)
	go func() {
		for ev := range conv.Events() {
			if ev.Type == EventTurn && ev.Turn.Role == RoleAssistant &&
				(ev.Turn.State == TurnStateComplete || ev.Turn.State == TurnStateErrored || ev.Turn.State == TurnStateInterrupted) {
				select {
				case terminal <- ev.Turn:
				default:
				}
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	defer release()
	if _, err := conv.Send(ctx, "Use your AskUserQuestion tool now to ask me one question: which colour do I prefer, with the options Red and Blue. "+
		"Once I answer, reply with one short sentence that names the colour I chose, and do nothing else."); err != nil {
		t.Fatalf("Send: %v", err)
	}
	req := awaitNewPendingInput(t, conv, "", 2*time.Minute)
	if req.Kind != claudecode.KindQuestion {
		t.Fatalf("request is a %q (%q), want a question", req.Kind, req.Prompt)
	}
	var labels []string
	for _, o := range req.Options {
		labels = append(labels, o.Label)
	}
	t.Logf("asked %q, options %q", req.Prompt, labels)
	if err := conv.Answer(ctx, req.ID, InputAnswer{OptionID: "Blue"}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	select {
	case turn := <-terminal:
		if turn.State != TurnStateComplete || !strings.Contains(strings.ToLower(turn.Text), "blue") {
			t.Fatalf("turn ended %s (%q): %q, want completed naming blue", turn.State, turn.Reason, turn.Text)
		}
		t.Logf("reply: %q", turn.Text)
	case <-time.After(2 * time.Minute):
		t.Fatal("the turn did not end within 2m of the answer")
	}
}
