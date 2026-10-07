package chatcore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/harnessname"
)

// notReadyScreen paints a frame with NO composer prompt at all: the harness is
// mid-reply and readyForInput is false for it under every version of the
// claude-code branch, before or after the banner requirement was dropped.
const notReadyScreen = "⏺ still drafting the answer\n"

// TestSend_TurnInFlightRejectedBeforeReadiness pins the ordering that makes it
// safe for readyForInput's claude-code branch to be a bare composer-prompt
// check: Send rejects a concurrent send on currentTurn with ErrTurnInFlight
// BEFORE waitReadyForSend is ever consulted.
//
// waitReadyForSend applies no busy gate of its own, so if that order were ever
// reversed, loosening the readiness predicate could let Send type into a
// composer mid-turn. The two cases below are the discriminator: on a not-ready
// screen a send with no turn in flight blocks to the context deadline, while a
// send WITH a turn in flight returns ErrTurnInFlight immediately.
func TestSend_TurnInFlightRejectedBeforeReadiness(t *testing.T) {
	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Build()
	// Never paints a prompt again, and never paints an end-of-turn marker: the
	// first turn stays in flight on a permanently not-ready screen.
	script.Steps = append(script.Steps, fakeharness.Step{
		Frame: &fakeharness.Frame{DelayMs: 20, Screen: notReadyScreen},
	})

	conv := openFake(t, script)
	sendOneTurn(t, conv, "first turn")

	// Let the not-ready frame land and the turn settle into flight.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conv.mu.Lock()
		inFlight := conv.currentTurn != nil
		conv.mu.Unlock()
		if inFlight && !readyForInput(harnessname.ClaudeCode, conv.screen.Snapshot().Text) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	conv.mu.Lock()
	inFlight := conv.currentTurn != nil
	conv.mu.Unlock()
	if !inFlight {
		t.Fatal("precondition: a turn must still be in flight")
	}
	if readyForInput(harnessname.ClaudeCode, conv.screen.Snapshot().Text) {
		t.Fatal("precondition: the screen must be not-ready, or the ordering is untested")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	defer release()

	// With a turn in flight: ErrTurnInFlight, and fast — the readiness wait is
	// never entered, so a context far shorter than the readiness wait suffices.
	short, cancelShort := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelShort()
	start := time.Now()
	if _, err := conv.Send(short, "second turn"); !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("Send during an in-flight turn = %v, want ErrTurnInFlight "+
			"(a DeadlineExceeded here means the readiness wait ran first)", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("Send took %s to reject; the currentTurn check must short-circuit before waiting", elapsed)
	}

	// Control case, proving the screen really is not-ready: clear the in-flight
	// turn and the SAME send now blocks in waitReadyForSend to the deadline.
	conv.mu.Lock()
	conv.currentTurn = nil
	conv.mu.Unlock()
	short2, cancelShort2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancelShort2()
	if _, err := conv.Send(short2, "third turn"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send on a not-ready screen with no turn in flight = %v, want DeadlineExceeded "+
			"(the readiness wait must be what blocks)", err)
	}
}

// TestSend_ReservationCoversTheReadinessWait pins the check-then-act fix: Send
// and SetPermissionMode used to check for an in-flight turn, wait seconds for a
// ready prompt, and only then claim the conversation, so two callers sharing a
// control token could both pass the check and interleave keystrokes. The claim
// is now taken with the check, so while one Send waits for readiness every
// other write is refused at once.
func TestSend_ReservationCoversTheReadinessWait(t *testing.T) {
	script := fakeharness.New("claude-code").Build()
	script.Steps = append(script.Steps, fakeharness.Step{
		Frame: &fakeharness.Frame{DelayMs: 20, Screen: notReadyScreen},
	})
	conv := openFake(t, script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	defer release()

	// The first Send parks in waitReadyForSend: the screen never gets ready.
	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelFirst()
	firstDone := make(chan error, 1)
	go func() {
		_, err := conv.Send(firstCtx, "first")
		firstDone <- err
	}()
	// Give it time to pass its checks and enter the readiness wait.
	time.Sleep(300 * time.Millisecond)

	short, cancelShort := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancelShort()
	if _, err := conv.Send(short, "second"); !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("second Send while the first waits = %v, want ErrTurnInFlight "+
			"(a DeadlineExceeded means both passed the check)", err)
	}
	if _, err := conv.SetPermissionMode(short, "plan"); !errors.Is(err, ErrTurnInFlight) {
		t.Fatalf("SetPermissionMode while a Send waits = %v, want ErrTurnInFlight", err)
	}

	cancelFirst()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Send = %v, want context.Canceled", err)
	}
	conv.mu.Lock()
	reserved := conv.writeReserved
	conv.mu.Unlock()
	if reserved {
		t.Fatal("the write slot stayed claimed after the first Send returned")
	}
}
