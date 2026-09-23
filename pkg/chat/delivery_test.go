package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Ordered, bounded event delivery (ADR-008), over the real fake harness.

// recorder is an OnEvent that keeps what it is handed, optionally slowly.
type recorder struct {
	mu    sync.Mutex
	evs   []ConversationEvent
	delay time.Duration
	gate  chan struct{} // when non-nil, each call waits for it to close
}

func (r *recorder) onEvent(ev ConversationEvent) {
	if r.gate != nil {
		<-r.gate
	}
	time.Sleep(r.delay)
	r.mu.Lock()
	r.evs = append(r.evs, ev)
	r.mu.Unlock()
}

func (r *recorder) events() []ConversationEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ConversationEvent(nil), r.evs...)
}

// waitExited waits for EventExited to be delivered.
func (r *recorder) waitExited(t *testing.T) []ConversationEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		evs := r.events()
		if n := len(evs); n > 0 && evs[n-1].Type == EventExited {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("EventExited never delivered; got %s", describe(evs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func describe(evs []ConversationEvent) string {
	var b strings.Builder
	for _, ev := range evs {
		b.WriteString(string(ev.Type))
		if ev.Type == EventTurn {
			b.WriteString(":" + string(ev.Turn.Role) + "/" + string(ev.Turn.State))
		}
		b.WriteString(" ")
	}
	return b.String()
}

func withOnEvent(r *recorder) func(*Options) {
	return func(o *Options) { o.OnEvent = r.onEvent }
}

// A slow consumer gets every event of a turn the harness died in, in order:
// the user's turn, the pending turn, its terminal event, then EventExited —
// and Events() closes after it.
func TestDelivery_ExitMidTurnInOrder(t *testing.T) {
	r := &recorder{delay: 30 * time.Millisecond}
	conv := openFake(t, fakeharness.New("claude-code").Idle().AwaitSubmit().Working(20, "Working").Exit(3).Build(), withOnEvent(r))
	sendOneTurn(t, conv, "go")
	evs := r.waitExited(t)
	got := describe(evs)
	if want := "turn:user/complete turn:assistant/pending turn:assistant/errored exited "; got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if exit := evs[len(evs)-1].Exit; exit == nil || exit.Status != wrapper.StatusFailed || exit.ExitCode != 3 {
		t.Fatalf("exit = %+v, want failed with code 3", exit)
	}
	select {
	case <-conv.Done():
	case <-time.After(time.Second):
		t.Fatal("Done never closed after the harness exited")
	}
	for range conv.Events() {
		// Events() closes after the last event.
	}
	if st := conv.State(); st.Alive || st.Exit == nil || st.Turn != nil {
		t.Fatalf("State = %+v, want exited with no turn", st)
	}
}

// A harness that exits between turns still says so.
func TestDelivery_ExitWithNoTurn(t *testing.T) {
	r := &recorder{}
	conv := openFake(t, fakeharness.New("claude-code").Idle().Raw(200, "bye").Exit(0).Build(), withOnEvent(r))
	evs := r.waitExited(t)
	if got := describe(evs); got != "exited " {
		t.Fatalf("events = %q, want only EventExited", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := conv.Send(ctx, "anyone?"); !errors.Is(err, ErrExited) {
		t.Fatalf("Send after the exit = %v, want ErrExited", err)
	}
}

// Close delivers what is left, EventExited last, and returns nil once it has.
func TestDelivery_CloseDrains(t *testing.T) {
	r := &recorder{delay: 20 * time.Millisecond}
	conv := openFake(t, fakeharness.New("claude-code").Idle().AwaitSubmit().Working(20, "Working").
		Reply(30, "done", "Baked", "1s").StayAliveUntilStopped().Build(), withOnEvent(r))
	sendOneTurn(t, conv, "go")
	waitForTerminalTurn(t, conv, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conv.Close(ctx); err != nil {
		t.Fatalf("Close = %v, want the drain to finish", err)
	}
	evs := r.events()
	if n := len(evs); n == 0 || evs[n-1].Type != EventExited {
		t.Fatalf("events = %s, want EventExited last", describe(evs))
	}
}

// A consumer that stops taking events fills the queue to its bound and holds
// the producers there — memory stays bounded — while Interrupt still answers
// and Close returns, reporting what it could not deliver.
func TestDelivery_StalledConsumer(t *testing.T) {
	r := &recorder{gate: make(chan struct{})}
	defer close(r.gate)
	conv := openFake(t, interruptScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().EchoWorking(20, "Writing").AwaitInterrupt().Stopped(30, "cut")
	}), interruptDirs(t), withOnEvent(r), func(o *Options) { o.EventQueue = wrapper.QueueLimits{Events: 1} })

	// The user's turn goes to the stalled callback, the pending one fills the
	// queue, and the turn still starts.
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
	if res, err := interruptWithin(conv, 5*time.Second); err != nil || res != InterruptStopped {
		t.Fatalf("Interrupt with a stalled consumer = %q, %v; want stopped", res, err)
	}
	// The interrupted turn's event waits for room behind the stalled callback.
	deadline := time.Now().Add(5 * time.Second)
	for conv.State().Delivery.WaitingSince.IsZero() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	st := conv.State().Delivery
	if st.Queued > 1 || st.WaitingSince.IsZero() || st.DeliveringSince.IsZero() {
		t.Fatalf("delivery = %+v, want the queue at its bound, a producer waiting, the callback stuck", st)
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer closeCancel()
	start := time.Now()
	if err := conv.Close(closeCtx); !errors.Is(err, ErrUndelivered) {
		t.Fatalf("Close = %v, want ErrUndelivered", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("Close outlived its context")
	}
	if st := conv.State().Delivery; st.Undelivered == 0 {
		t.Fatalf("delivery = %+v, want the dropped events counted", st)
	}
}

// An event over the byte bound is delivered without its text, carrying the
// resource error; History keeps the text.
func TestDelivery_OversizedEvent(t *testing.T) {
	r := &recorder{}
	long := strings.Repeat("x", 4096)
	conv := openFake(t, fakeharness.New("claude-code").Idle().AwaitSubmit().Working(20, "Working").
		Reply(30, long, "Baked", "1s").StayAliveUntilStopped().Build(),
		withOnEvent(r), func(o *Options) { o.EventQueue = wrapper.QueueLimits{Bytes: 2048} })
	sendOneTurn(t, conv, "go")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var terminal *ConversationEvent
		for _, ev := range r.events() {
			if ev.Type == EventTurn && ev.Turn.Role == RoleAssistant && ev.Turn.State == TurnStateComplete {
				terminal = &ev
			}
		}
		if terminal != nil {
			if terminal.Turn.Text != "" || !errors.Is(terminal.Err, ErrEventTooLarge) {
				t.Fatalf("oversized turn = %+v, err %v; want no text and ErrEventTooLarge", terminal.Turn, terminal.Err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reply never arrived: %s", describe(r.events()))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := conv.State().Delivery; st.Oversized != 1 {
		t.Fatalf("delivery = %+v, want one oversized event", st)
	}
}

// State reads the live conversation in one call.
func TestState(t *testing.T) {
	conv := openFake(t, fakeharness.New("claude-code").ComposerBox().Idle().AwaitSubmit().Working(20, "Working").
		StayAliveUntilStopped().Build())
	sendOneTurn(t, conv, "go")
	awaitScreen(t, conv, "esc to interrupt")
	st := conv.State()
	if !st.Alive || st.PID == 0 || st.Turn == nil || st.Turn.State != TurnStatePending || !st.Busy ||
		st.LastOutputAt.IsZero() || st.HarnessSessionID == "" || st.Exit != nil {
		t.Fatalf("State = %+v, want alive, busy, the turn in flight and the session id", st)
	}
	if st.Delivery.Limits.Events != 1024 || st.Delivery.Limits.Bytes != 16<<20 {
		t.Fatalf("delivery limits = %+v, want the defaults", st.Delivery.Limits)
	}
}

// OnEvent runs outside the conversation's locks: a callback that reads the
// conversation's state does not deadlock it.
func TestDelivery_CallbackRunsOutsideLocks(t *testing.T) {
	var conv *Conversation
	var mu sync.Mutex
	seen := 0
	onEvent := func(ConversationEvent) {
		mu.Lock()
		c := conv
		mu.Unlock()
		if c != nil {
			_ = c.State()
			_ = c.PendingInput()
		}
		mu.Lock()
		seen++
		mu.Unlock()
	}
	conv = openFake(t, fakeharness.New("claude-code").Idle().AwaitSubmit().Working(20, "Working").
		Reply(30, "done", "Baked", "1s").StayAliveUntilStopped().Build(),
		func(o *Options) { o.OnEvent = onEvent })
	mu.Lock()
	conv2 := conv
	mu.Unlock()
	sendOneTurn(t, conv2, "go")
	if turn := waitForTerminalTurn(t, conv2, 5*time.Second); turn.State != TurnStateComplete {
		t.Fatalf("turn = %+v", turn)
	}
}

// Close racing the events a busy harness produces: no panic, no send on a
// closed channel, and Events() ends.
func TestDelivery_CloseWhileEmitting(t *testing.T) {
	b := fakeharness.New("claude-code").Idle().AwaitSubmit()
	for range 50 {
		b = b.Working(5, "Working").Reply(5, "part", "Baked", "1s")
	}
	conv := openFake(t, b.StayAliveUntilStopped().Build())
	sendOneTurn(t, conv, "go")
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = conv.Close(ctx)
	for range conv.Events() {
		// Drains, then closes.
	}
}

// A harness that dies while Send is submitting ends the turn once, after its
// pending event and before EventExited, however the race falls.
func TestDelivery_TurnEndsOnceWhenTheHarnessDiesMidSubmit(t *testing.T) {
	r := &recorder{}
	conv := openFake(t, fakeharness.New("claude-code").Idle().Raw(100, "").Exit(1).Build(), withOnEvent(r))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if release, err := conv.AcquireControl(ctx); err == nil {
		_, _ = conv.Send(ctx, "go")
		release()
	}
	evs := r.waitExited(t)
	got := describe(evs)
	switch got {
	case "exited ", // the exit came first: Send refused
		"turn:user/complete turn:assistant/pending turn:assistant/errored exited ":
	default:
		t.Fatalf("events = %q, want the turn to end once, before the exit", got)
	}
}

// Done says the process ended; Events() closing says delivery finished. With
// the consumer stalled the first comes and the second does not.
func TestDelivery_DoneIsNotDrained(t *testing.T) {
	r := &recorder{gate: make(chan struct{})}
	defer close(r.gate)
	conv := openFake(t, fakeharness.New("claude-code").Idle().AwaitSubmit().Working(20, "Working").Exit(3).Build(),
		withOnEvent(r))
	sendOneTurn(t, conv, "go")
	select {
	case <-conv.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done never closed")
	}
	if st := conv.State(); st.Alive || st.Exit == nil {
		t.Fatalf("State = %+v, want exited", st)
	}
	if st := conv.State().Delivery; st.DeliveringSince.IsZero() || st.Delivered != 0 {
		t.Fatalf("delivery = %+v, want nothing delivered past the stalled callback", st)
	}
}
