package chat

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
)

// Interrupt (ADR-007), over the real fake harness. The fake paints the frames
// claude 2.1.280 was recorded painting: the prompt's echo while it works, the
// interrupt marker below a stopped reply, the prompt back in the composer after
// a cancel.

const (
	busyFooter = "  ⏵⏵ esc to interrupt"
	spinner    = "✶ Cerebrating… (3s · ↓ 1.2k tokens)"
)

// interruptDirs opens in a private working dir and config root, where the fake
// writes the transcript chat reads.
func interruptDirs(t *testing.T) func(*Options) {
	return func(o *Options) {
		o.WorkingDir = t.TempDir()
		o.Env = append(o.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(t.TempDir(), "claude"))
	}
}

// interruptScript starts at an idle composer box and holds after steps.
func interruptScript(steps func(*fakeharness.Builder) *fakeharness.Builder) fakeharness.Script {
	return steps(fakeharness.New("claude-code").ComposerBox().Idle()).StayAliveUntilStopped().Build()
}

func interruptWithin(conv *Conversation, d time.Duration) (InterruptResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return conv.Interrupt(ctx)
}

// neverShows fails when the screen shows s within d.
func neverShows(t *testing.T, conv *Conversation, s string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(conv.screen.Snapshot().Text, s) {
			t.Fatalf("the screen showed %q:\n%s", s, conv.screen.Snapshot().Text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestInterrupt_StopsTheTurnWithItsPartialReply(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").AwaitInterrupt().Stopped(30, "half a reply")
	}), interruptDirs(t))
	sendOneTurn(t, conv, "tell me a story")
	awaitScreen(t, conv, "esc to interrupt")

	if res, err := interruptWithin(conv, 5*time.Second); err != nil || res != InterruptStopped {
		t.Fatalf("Interrupt = %q, %v; want stopped", res, err)
	}
	turn := waitForTerminalTurn(t, conv, 5*time.Second)
	if turn.State != TurnStateInterrupted || turn.Text != "half a reply" {
		t.Fatalf("turn = %+v, want interrupted with the partial reply", turn)
	}
	if turn.Reason != "claude-code: interrupted: the harness stopped the turn" || turn.Code != "" {
		t.Fatalf("reason = %q, code = %q", turn.Reason, turn.Code)
	}
}

// The transcript's record of the partial reply wins over the screen's, once the
// harness has recorded the interrupt.
func TestInterrupt_PartialReplyFromTheTranscript(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().TranscriptUser(0).EchoWorking(20, "Writing").AwaitInterrupt().
			TranscriptReply(0, "the recorded half").TranscriptInterrupted(0, false).
			Stopped(30, "the painted half")
	}), interruptDirs(t))
	sendOneTurn(t, conv, "tell me a story")
	awaitScreen(t, conv, "esc to interrupt")

	if res, err := interruptWithin(conv, 5*time.Second); err != nil || res != InterruptStopped {
		t.Fatalf("Interrupt = %q, %v; want stopped", res, err)
	}
	if turn := waitForTerminalTurn(t, conv, 5*time.Second); turn.Text != "the recorded half" {
		t.Fatalf("turn = %+v, want the transcript's partial reply", turn)
	}
}

// A turn stopped in a tool call has no reply text: the tool call is not one,
// on the screen or in the transcript.
func TestInterrupt_MidToolHasNoText(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transcript bool
	}{{"screen", false}, {"transcript", true}} {
		t.Run(tc.name, func(t *testing.T) {
			conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
				b = b.AwaitSubmit()
				if tc.transcript {
					b = b.TranscriptUser(0)
				}
				b = b.EchoWorking(20, "Running").AwaitInterrupt()
				if tc.transcript {
					b = b.TranscriptInterrupted(0, true)
				}
				return b.Stopped(30, "Bash(sleep 20)")
			}), interruptDirs(t))
			sendOneTurn(t, conv, "run a command")
			awaitScreen(t, conv, "esc to interrupt")
			if res, err := interruptWithin(conv, 5*time.Second); err != nil || res != InterruptStopped {
				t.Fatalf("Interrupt = %q, %v; want stopped", res, err)
			}
			if turn := waitForTerminalTurn(t, conv, 5*time.Second); turn.State != TurnStateInterrupted || turn.Text != "" {
				t.Fatalf("turn = %+v, want interrupted with no text", turn)
			}
		})
	}
}

// An interrupt before the first token cancels the turn and puts the prompt
// back in the composer; chat clears it, and the next prompt arrives alone.
func TestInterrupt_CancelClearsTheComposerForTheNextSend(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Thinking").AwaitInterrupt().Cancelled(30).
			AwaitComposerClear(1).EmptyComposer(20).
			AwaitSubmit().Working(20, "Working").Reply(30, "got: "+fakeharness.PromptRef(), "Baked", "1s")
	}), interruptDirs(t))
	sendOneTurn(t, conv, "answer slowly")
	awaitScreen(t, conv, "esc to interrupt")

	if res, err := interruptWithin(conv, 5*time.Second); err != nil || res != InterruptCancelled {
		t.Fatalf("Interrupt = %q, %v; want cancelled", res, err)
	}
	turn := waitForTerminalTurn(t, conv, 5*time.Second)
	if turn.State != TurnStateInterrupted || turn.Text != "" ||
		turn.Reason != "claude-code: interrupted: the harness cancelled the turn before it replied" {
		t.Fatalf("turn = %+v, want interrupted, cancelled, no text", turn)
	}
	sendOneTurn(t, conv, "the next prompt")
	if next := waitForTerminalTurn(t, conv, 5*time.Second); next.State != TurnStateComplete || next.Text != "got: the next prompt" {
		t.Fatalf("next turn = %+v, want the next prompt answered alone", next)
	}
}

func TestInterrupt_NoTurnWritesNothing(t *testing.T) {
	var wrote [][]byte
	c := &Conversation{
		opts: Options{Harness: chatClaudeCode}, adapter: claudecode.New(),
		screen: screen.New(120, 40), closed: make(chan struct{}),
		writeStdin: func(p []byte) (int, error) { wrote = append(wrote, p); return len(p), nil },
	}
	if res, err := interruptWithin(c, time.Second); err != nil || res != InterruptNoTurn {
		t.Fatalf("Interrupt = %q, %v; want no_turn", res, err)
	}
	if len(wrote) != 0 {
		t.Fatalf("wrote %q with no turn in flight", wrote)
	}
}

func TestInterrupt_UnsupportedHarness(t *testing.T) {
	c := &Conversation{opts: Options{Harness: "codex"}, adapter: codex.New(), closed: make(chan struct{})}
	if _, err := interruptWithin(c, time.Second); !errors.Is(err, ErrInterruptUnsupported) {
		t.Fatalf("Interrupt err = %v, want ErrInterruptUnsupported", err)
	}
}

// A turn that finished before the interrupt landed keeps its outcome, and no
// key is written into the idle composer.
func TestInterrupt_TooLateWritesNoKey(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Working").Reply(30, "all done", "Baked", "1s").
			AwaitInterrupt().Paint(0, "INTERRUPT_KEY_ARRIVED", "", "❯ ")
	}), interruptDirs(t), func(o *Options) { o.markerGap = time.Second })
	sendOneTurn(t, conv, "quick one")
	awaitScreen(t, conv, "✻ Baked for 1s")

	if res, err := interruptWithin(conv, 5*time.Second); err != nil || res != InterruptTooLate {
		t.Fatalf("Interrupt = %q, %v; want too_late", res, err)
	}
	if turn := waitForTerminalTurn(t, conv, 5*time.Second); turn.State != TurnStateComplete || turn.Text != "all done" {
		t.Fatalf("turn = %+v, want its own completion", turn)
	}
	neverShows(t, conv, "INTERRUPT_KEY_ARRIVED", 300*time.Millisecond)
}

// An Interrupt made while Send is between its prompt and its submit key waits
// for the submit: the key never lands inside the prompt.
func TestInterrupt_WaitsForTheSubmit(t *testing.T) {
	const prompt = "a prompt that must arrive whole"
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").AwaitInterrupt().Stopped(30, "cut")
	}), interruptDirs(t))
	sent := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		release, err := conv.AcquireControl(ctx)
		if err != nil {
			sent <- err
			return
		}
		defer release()
		_, err = conv.Send(ctx, prompt)
		sent <- err
	}()
	for ev := range conv.Events() {
		if ev.Type == EventTurn && ev.Turn.Role == RoleUser {
			break // Send has recorded the turn and is typing it
		}
	}
	if res, err := interruptWithin(conv, 5*time.Second); err != nil || res != InterruptStopped {
		t.Fatalf("Interrupt = %q, %v; want stopped", res, err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !showsLine(conv, "❯ "+prompt) {
		t.Fatalf("the prompt did not arrive whole:\n%s", conv.screen.Snapshot().Text)
	}
}

// showsLine reports whether a screen row reads exactly line, padding aside.
func showsLine(conv *Conversation, line string) bool {
	for _, row := range strings.Split(conv.screen.Snapshot().Text, "\n") {
		if strings.TrimRight(row, " ") == line {
			return true
		}
	}
	return false
}

// Interrupt takes no control token, so it cannot deadlock against a caller
// holding the token for the whole turn, as RunTurn does.
func TestInterrupt_WhileTheTokenIsHeld(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").AwaitInterrupt().Stopped(30, "cut")
	}), interruptDirs(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := conv.Send(ctx, "long task"); err != nil {
		t.Fatal(err)
	}
	awaitScreen(t, conv, "esc to interrupt")
	done := make(chan InterruptResult, 1)
	go func() {
		res, _ := interruptWithin(conv, 5*time.Second)
		done <- res
	}()
	select {
	case res := <-done:
		if res != InterruptStopped {
			t.Fatalf("Interrupt = %q, want stopped", res)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Interrupt blocked behind the held control token")
	}
}

// Esc pressed at the terminal ends the turn the same way, and its reason says
// so.
func TestInterrupt_AtTheTerminal(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").Stopped(80, "cut short")
	}), interruptDirs(t))
	sendOneTurn(t, conv, "tell me a story")
	turn := waitForTerminalTurn(t, conv, 5*time.Second)
	if turn.State != TurnStateInterrupted || turn.Text != "cut short" ||
		turn.Reason != "claude-code: interrupted: the harness stopped the turn (at the terminal)" {
		t.Fatalf("turn = %+v, want interrupted at the terminal", turn)
	}
}

// earlierTurn is an interrupted turn left on screen above the next one.
var earlierTurn = []string{"❯ first", "⏺ first half", fakeharness.InterruptMarkerLine(), ""}

func withEarlier(lines ...string) []string {
	return append(append([]string{"Claude Code", ""}, earlierTurn...), lines...)
}

// The marker an earlier turn left on screen never ends the next turn.
func TestInterrupt_StaleMarkerNeverEndsTheNextTurn(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").Stopped(60, "first half").
			AwaitSubmit().
			Paint(20, withEarlier("❯ "+fakeharness.PromptRef(), spinner, "", "❯ ", busyFooter)...).
			Paint(60, withEarlier("❯ "+fakeharness.PromptRef(), "⏺ second reply", "", "✻ Baked for 1s", "", "❯ ")...)
	}), interruptDirs(t))
	sendOneTurn(t, conv, "first")
	if turn := waitForTerminalTurn(t, conv, 5*time.Second); turn.State != TurnStateInterrupted {
		t.Fatalf("first turn = %+v, want interrupted", turn)
	}
	sendOneTurn(t, conv, "second")
	if turn := waitForTerminalTurn(t, conv, 5*time.Second); turn.State != TurnStateComplete || turn.Text != "second reply" {
		t.Fatalf("second turn = %+v, want it complete with its reply", turn)
	}
}

// A second interrupt, below the first one's marker, is seen: the reading is
// per turn, not by the marker's presence.
func TestInterrupt_SecondInterruptIsSeen(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").Stopped(60, "first half").
			AwaitSubmit().
			Paint(20, withEarlier("❯ "+fakeharness.PromptRef(), spinner, "", "❯ ", busyFooter)...).
			AwaitInterrupt().
			Paint(30, withEarlier("❯ "+fakeharness.PromptRef(), "⏺ second half", fakeharness.InterruptMarkerLine(), "", "❯ ")...)
	}), interruptDirs(t))
	sendOneTurn(t, conv, "first")
	if turn := waitForTerminalTurn(t, conv, 5*time.Second); turn.State != TurnStateInterrupted {
		t.Fatalf("first turn = %+v, want interrupted", turn)
	}
	sendOneTurn(t, conv, "second")
	awaitScreen(t, conv, "esc to interrupt")
	if res, err := interruptWithin(conv, 5*time.Second); err != nil || res != InterruptStopped {
		t.Fatalf("Interrupt = %q, %v; want stopped", res, err)
	}
	if turn := waitForTerminalTurn(t, conv, 5*time.Second); turn.State != TurnStateInterrupted || turn.Text != "second half" {
		t.Fatalf("second turn = %+v, want interrupted with its own partial reply", turn)
	}
}

// Concurrent Interrupts for one turn write one key: two Escs close together
// open claude's Rewind picker on an idle composer.
func TestInterrupt_ConcurrentCallsJoin(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").AwaitInterrupt().Stopped(300, "cut").
			AwaitInterrupt().Paint(0, "SECOND_INTERRUPT_KEY", "", "❯ ")
	}), interruptDirs(t))
	sendOneTurn(t, conv, "long task")
	awaitScreen(t, conv, "esc to interrupt")

	var wg sync.WaitGroup
	results := make([]InterruptResult, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = interruptWithin(conv, 5*time.Second)
		}()
	}
	wg.Wait()
	for i, res := range results {
		if res != InterruptStopped {
			t.Fatalf("Interrupt #%d = %q, want stopped", i, res)
		}
	}
	neverShows(t, conv, "SECOND_INTERRUPT_KEY", 300*time.Millisecond)
}

// A harness that never acknowledges leaves the turn in flight, and while the
// interrupt waits, the idle fallback does not pass the settled fragment off as
// the reply.
func TestInterrupt_UnconfirmedLeavesTheTurnInFlight(t *testing.T) {
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").AwaitInterrupt().SettleIdle(20, "fragment")
	}), interruptDirs(t))
	sendOneTurn(t, conv, "long task")
	awaitScreen(t, conv, "esc to interrupt")

	// Three idle gaps: without the hold, the fallback completes the turn from
	// the fragment one gap after it settles.
	wait := 3 * testIdleGap
	start := time.Now()
	_, err := interruptWithin(conv, wait)
	if !errors.Is(err, ErrInterruptUnconfirmed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Interrupt err = %v, want ErrInterruptUnconfirmed wrapping the deadline", err)
	}
	// Once the interrupt gives up, the turn ends as the harness left it.
	turn := waitForTerminalTurn(t, conv, 5*time.Second)
	if turn.CompletedAt.Before(start.Add(wait - 50*time.Millisecond)) {
		t.Fatalf("the turn completed %s into a %s interrupt wait: %+v", turn.CompletedAt.Sub(start), wait, turn)
	}
}

// Send types into an empty composer only: a draft left there is cleared first.
func TestSend_ClearsWhatTheComposerHolds(t *testing.T) {
	conv := openFake(t, fakeharness.New("claude-code").ComposerBox().
		ComposerHolding(0, "a draft someone typed").AwaitComposerClear(1).EmptyComposer(10).
		AwaitSubmit().Working(20, "Working").Reply(30, "got: "+fakeharness.PromptRef(), "Baked", "1s").
		StayAliveUntilStopped().Build(), interruptDirs(t))
	sendOneTurn(t, conv, "the real prompt")
	if turn := waitForTerminalTurn(t, conv, 5*time.Second); turn.State != TurnStateComplete || turn.Text != "got: the real prompt" {
		t.Fatalf("turn = %+v, want the prompt answered alone", turn)
	}
}

// A composer that will not clear gets nothing typed into it, and no turn.
func TestSend_RefusesAComposerThatWillNotClear(t *testing.T) {
	conv := openFake(t, fakeharness.New("claude-code").ComposerBox().
		ComposerHolding(0, "stuck draft").StayAliveUntilStopped().Build(), interruptDirs(t))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := conv.Send(ctx, "the real prompt"); !errors.Is(err, ErrComposerNotCleared) {
		t.Fatalf("Send err = %v, want ErrComposerNotCleared", err)
	}
	if turns, _ := conv.store.ListTurns(ctx, conv.SessionID()); len(turns) != 0 {
		t.Fatalf("recorded %d turns for a prompt that was never typed", len(turns))
	}
}
