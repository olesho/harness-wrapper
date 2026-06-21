package harness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// fakeBin builds the scriptable fake harness (cmd/fakeharness) once per process,
// skipping when the Go toolchain is unavailable. scriptEnv marshals a script to
// a temp file and returns the env that points the fake at it — passed to RunTurn
// via TurnConfig.Env so the one-shot driver spawns the fake over a real PTY and
// submits with CSI 13u (no newline coupling).
func fakeBin(t *testing.T) string {
	t.Helper()
	p, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}
	return p
}

func scriptEnv(t *testing.T, s fakeharness.Script) []string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	p := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return append(os.Environ(), fakeharness.EnvVar+"="+p)
}

func TestRunTurn_ClaudeStyleTurnStopsAfterCompletion(t *testing.T) {
	const sessionID = "123e4567-e89b-12d3-a456-426614174000"
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Session(sessionID).
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
		ExitOnQuit().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    bin,
		Env:           env,
		Prompt:        "ship the turn API",
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	if res.Turn.State != chat.TurnStateComplete {
		t.Fatalf("Turn.State = %q, want complete", res.Turn.State)
	}
	if res.Session.HarnessSessionID != sessionID {
		t.Fatalf("HarnessSessionID = %q", res.Session.HarnessSessionID)
	}
	if len(res.History) < 2 {
		t.Fatalf("History length = %d, want at least user + assistant turns", len(res.History))
	}
	if res.Conversation != nil {
		t.Fatal("Conversation should be nil when ExitAfterTurn is true")
	}
	if !res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = false, want true")
	}
	if res.WrapperResult.Status != wrapper.StatusInterrupted {
		t.Fatalf("raw WrapperResult.Status = %q, want interrupted after intentional stop", res.WrapperResult.Status)
	}
	if !strings.Contains(out.String(), "assistant reply: ship the turn API") {
		t.Fatalf("Output missing assistant reply:\n%s", out.String())
	}
}

func TestRunTurn_CanKeepConversationAlive(t *testing.T) {
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply one", "Baked", "1s").
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply two", "Baked", "2s").
		ExitOnQuit().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    bin,
		Env:           env,
		Prompt:        "first turn",
		ExitAfterTurn: false,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.Conversation == nil {
		t.Fatal("Conversation is nil when ExitAfterTurn is false")
	}
	if res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = true, want false for kept conversation")
	}
	defer res.Conversation.Close(context.Background())

	release, err := res.Conversation.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl second turn: %v", err)
	}
	defer release()
	turnID, err := res.Conversation.Send(ctx, "second turn")
	if err != nil {
		t.Fatalf("Send second turn: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case ev := <-res.Conversation.Events():
			if ev.Turn.ID == turnID && ev.Turn.State == chat.TurnStateComplete {
				return
			}
		}
	}
}

func TestRunTurn_ReturnsErrTurnErrored(t *testing.T) {
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Exit(2). // crash mid-turn, like a harness that dies after the prompt
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:    "claude",
		BinaryPath: bin,
		Env:        env,
		Prompt:     "fail this turn",
	})
	if !errors.Is(err, harness.ErrTurnErrored) {
		t.Fatalf("RunTurn err = %v, want ErrTurnErrored", err)
	}
	if res.Turn.State != chat.TurnStateErrored {
		t.Fatalf("Turn.State = %q, want errored", res.Turn.State)
	}
}

func TestRunTurn_RealClaudeDogfood(t *testing.T) {
	if os.Getenv("HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN") != "1" {
		t.Skip("set HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN=1 to run against real Claude Code")
	}
	claudePath := requireRealClaude(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    claudePath,
		Args:          []string{"--dangerously-skip-permissions"},
		Prompt:        "Reply with exactly: HARNESS_WRAPPER_RUNTURN_OK",
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err != nil {
		t.Fatalf("RunTurn real Claude: %v\noutput:\n%s", err, out.String())
	}
	if res.Turn.State != chat.TurnStateComplete {
		t.Fatalf("Turn.State = %q, want complete\nreason: %s\noutput:\n%s", res.Turn.State, res.Turn.Reason, out.String())
	}
	if !strings.Contains(res.Turn.Text, "HARNESS_WRAPPER_RUNTURN_OK") && !strings.Contains(out.String(), "HARNESS_WRAPPER_RUNTURN_OK") {
		t.Fatalf("real Claude output missing sentinel\nturn text:\n%s\noutput:\n%s", res.Turn.Text, out.String())
	}
	if !res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = false, want true")
	}
	if res.WrapperResult.Status != wrapper.StatusInterrupted {
		t.Fatalf("raw WrapperResult.Status = %q, want interrupted after intentional stop", res.WrapperResult.Status)
	}
}

func TestRunTurn_RealClaudeDogfoodKeepAlive(t *testing.T) {
	if os.Getenv("HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN") != "1" {
		t.Skip("set HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN=1 to run against real Claude Code")
	}
	claudePath := requireRealClaude(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    claudePath,
		Args:          []string{"--dangerously-skip-permissions"},
		Prompt:        "Reply with exactly: HARNESS_WRAPPER_RUNTURN_KEEP_1",
		ExitAfterTurn: false,
		Output:        &out,
	})
	if err != nil {
		t.Fatalf("RunTurn real Claude keep-alive first turn: %v\noutput:\n%s", err, out.String())
	}
	if res.Conversation == nil {
		t.Fatal("Conversation is nil when ExitAfterTurn is false")
	}
	defer res.Conversation.Close(context.Background())
	if res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = true, want false")
	}

	release, err := res.Conversation.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl second real Claude turn: %v", err)
	}
	defer release()

	turnID, err := res.Conversation.Send(ctx, "Reply with exactly: HARNESS_WRAPPER_RUNTURN_KEEP_2")
	if err != nil {
		t.Fatalf("Send second real Claude turn: %v\noutput:\n%s", err, out.String())
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("second real Claude turn timed out: %v\noutput:\n%s", ctx.Err(), out.String())
		case ev, ok := <-res.Conversation.Events():
			if !ok {
				t.Fatalf("conversation closed before second real Claude turn completed\noutput:\n%s", out.String())
			}
			if ev.Turn.ID == turnID && ev.Turn.State == chat.TurnStateComplete {
				if !strings.Contains(ev.Turn.Text, "HARNESS_WRAPPER_RUNTURN_KEEP_2") && !strings.Contains(out.String(), "HARNESS_WRAPPER_RUNTURN_KEEP_2") {
					t.Fatalf("second real Claude output missing sentinel\nturn text:\n%s\noutput:\n%s", ev.Turn.Text, out.String())
				}
				return
			}
		}
	}
}

func requireRealClaude(t *testing.T) string {
	t.Helper()
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not found on PATH: %v", err)
	}
	return claudePath
}
