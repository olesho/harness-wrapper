package harness_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
)

// A harness that accepts the prompt and settles back to a ready prompt without
// ever answering. Every signal RunTurn checks says success — the submit landed,
// the turn machinery ran, the prompt settled — and the reply is empty.
//
// This is the shape of the field failure that motivated ErrEmptyTurn: eight
// consecutive paid agent runs reported complete while producing zero assistant
// output, and the only downstream evidence was an unreleased task claim.
func TestRunTurn_EmptyReplyIsAnError(t *testing.T) {
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		SettleIdle(40, ""). // settled + ready, no reply body
		StayAliveUntilStopped().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	if !errors.Is(err, harness.ErrEmptyTurn) {
		t.Fatalf("RunTurn err = %v, want ErrEmptyTurn\nturn text: %q\noutput:\n%s", err, res.Turn.Text, out.String())
	}
	// The turn travels with the error: callers diagnose from it.
	if res.Turn.State != chat.TurnStateComplete {
		t.Fatalf("Turn.State = %q, want the completed turn returned alongside the error", res.Turn.State)
	}
	if res.Session.ID == "" {
		t.Fatal("Session is empty; the result must still carry enough context to diagnose")
	}
}

// The guard must not fire on a real answer, however short — a harness that
// replies with one character has replied.
func TestRunTurn_ShortReplyIsNotEmpty(t *testing.T) {
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "K", "Baked", "1s").
		StayAliveUntilStopped().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
