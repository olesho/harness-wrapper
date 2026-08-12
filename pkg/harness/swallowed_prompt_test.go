package harness_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
)

// A harness that accepts nothing and merely repaints its ready screen: settled,
// prompt-ready, no "⏺" bullet and no "✻ … for Ns" thinking marker. That is
// indistinguishable from a finished turn to everything except the adapter's
// swallow verdict — and completing it "successfully" is what let eight
// consecutive paid agent runs report success while producing nothing.
func TestRunTurn_SwallowedPromptIsNotASuccess(t *testing.T) {
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		SettleIdle(40, ""). // settled + ready, no reply, no thinking marker
		StayAliveUntilStopped().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    bin,
		Env:           env,
		Prompt:        "answer me",
		ExitAfterTurn: true,
		Output:        &out,
	})

	if !errors.Is(err, harness.ErrTurnErrored) {
		t.Fatalf("RunTurn err = %v, want ErrTurnErrored for a swallowed prompt\nturn: %+v", err, res.Turn)
	}
	if res.Turn.State != chat.TurnStateErrored {
		t.Fatalf("Turn.State = %q, want errored", res.Turn.State)
	}
	// The reason has to name the failure, or an operator cannot tell it from a
	// model that simply said nothing. Matches meta-harness's wording.
	if !strings.Contains(res.Turn.Reason, "prompt not accepted / no assistant output") {
		t.Fatalf("Turn.Reason = %q, want the prompt-not-accepted wording", res.Turn.Reason)
	}
}

// The verdict must never fire on a turn that answered, however briefly.
func TestRunTurn_ShortReplyIsNotSwallowed(t *testing.T) {
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "K", "Baked", "1s").
		StayAliveUntilStopped().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    bin,
		Env:           env,
		Prompt:        "say K",
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v\noutput:\n%s", err, out.String())
	}
	if res.Turn.State != chat.TurnStateComplete {
		t.Fatalf("Turn.State = %q, want complete", res.Turn.State)
	}
}
