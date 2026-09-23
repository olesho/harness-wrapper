package chat_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// TestSessionAssignedLive is the live half of session-id assignment: a real
// claude that Open starts fresh writes its transcript under the id chat
// assigned, and /quit → Reopen resumes that same file. Only the real binary
// can show it — claude, not the chat layer, decides where the file goes.
//
// Env-gated like the other live tests; it spends two one-line turns:
//
//	HW_LIVE_SESSION=1 go test ./pkg/chat -run SessionAssignedLive -v
//
// It runs in this package's directory with the operator's own claude config
// (CLAUDE_CONFIG_DIR, else ~/.claude), which is where the transcript lands.
func TestSessionAssignedLive(t *testing.T) {
	if os.Getenv("HW_LIVE_SESSION") != "1" {
		t.Skip("live session-id assignment: set HW_LIVE_SESSION=1 (needs the real claude binary; spends two short turns)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	store := memstore.New()
	policy := &chat.InputPolicy{ByKind: map[string]chat.Disposition{
		"trust_prompt": {Kind: chat.DispositionAnswer, OptionID: "proceed"},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	conv, err := chat.Open(ctx, chat.Options{
		Harness: "claude-code", BinaryPath: bin, WorkingDir: wd,
		Env: harnessenv.Cleaned(), Store: store, InputPolicy: policy,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	trackClose(t, conv)
	id := storedHarnessID(t, store, conv)
	if !uuidRE.MatchString(id) {
		t.Fatalf("stored harness session id = %q, want an assigned UUID before the first turn", id)
	}

	if turn := sendAndAwaitWithin(t, conv, "Reply with exactly: HW_SESSION_ASSIGNED_ONE", 2*time.Minute); turn.State != chat.TurnStateComplete {
		t.Fatalf("first turn = %+v, want complete", turn)
	}
	path := liveTranscriptPath(t, wd, id)
	awaitTranscriptHolding(t, path, "HW_SESSION_ASSIGNED_ONE")

	if err := conv.Quit(ctx); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	if _, err := conv.Wrapper().Wait(); err != nil {
		t.Fatalf("Wait after /quit: %v", err)
	}
	_ = conv.Close(ctx)

	resumed, err := chat.Reopen(ctx, chat.ReopenOptions{
		SessionID: conv.SessionID(), BinaryPath: bin,
		Env: harnessenv.Cleaned(), Store: store, InputPolicy: policy,
	})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	trackClose(t, resumed)
	if turn := sendAndAwaitWithin(t, resumed, "Reply with exactly: HW_SESSION_ASSIGNED_TWO", 2*time.Minute); turn.State != chat.TurnStateComplete {
		t.Fatalf("resumed turn = %+v, want complete", turn)
	}
	awaitTranscriptHolding(t, path, "HW_SESSION_ASSIGNED_TWO")

	hist, src, err := resumed.HistoryWithSource(ctx)
	if err != nil || src != chat.HistorySourceTranscript {
		t.Fatalf("HistoryWithSource = (%d turns, %q, %v), want the transcript", len(hist), src, err)
	}
}

// liveTranscriptPath is where claude files session id's transcript for a launch
// in wd with the operator's config.
func liveTranscriptPath(t *testing.T, wd, id string) string {
	t.Helper()
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		root = filepath.Join(home, ".claude")
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
	return filepath.Join(root, "projects", transcriptcc.EncodedCWD(wd), id+".jsonl")
}

// awaitTranscriptHolding waits for the transcript at path to contain needle;
// claude flushes a turn's entries shortly after painting it.
func awaitTranscriptHolding(t *testing.T, path, needle string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("transcript %s never held %q (read error: %v)", path, needle, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
