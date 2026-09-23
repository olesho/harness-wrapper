package harness_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
)

// An interrupted turn ends the run: RunTurn returns ErrTurnInterrupted, which
// callers that handle ErrTurnErrored read as before, with the turn in state
// interrupted and its partial reply. Here someone pressed Esc at the terminal.
func TestRunTurn_InterruptedTurnEndsTheRun(t *testing.T) {
	bin := fakeBin(t)
	env := append(scriptEnv(t, fakeharness.New("claude-code").ComposerBox().Idle().
		AwaitSubmit().
		EchoWorking(30, "Writing").
		Stopped(60, "half a reply").
		StayAliveUntilStopped().
		Build()), "CLAUDE_CONFIG_DIR="+filepath.Join(t.TempDir(), "claude"))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    bin,
		WorkingDir:    t.TempDir(),
		Env:           env,
		Prompt:        "tell me a story",
		ExitAfterTurn: true,
		Output:        &out,
	})
	if !errors.Is(err, harness.ErrTurnInterrupted) || !errors.Is(err, harness.ErrTurnErrored) {
		t.Fatalf("RunTurn err = %v, want ErrTurnInterrupted, which is an ErrTurnErrored\nturn: %+v", err, res.Turn)
	}
	if res.Turn.State != chat.TurnStateInterrupted || res.Turn.Text != "half a reply" {
		t.Fatalf("turn = %+v, want interrupted with the partial reply", res.Turn)
	}
}
