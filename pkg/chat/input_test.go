package chat

import (
	"context"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

type keyRecorder struct{ data []byte }

func (k *keyRecorder) write(p []byte) (int, error) {
	k.data = append(k.data, p...)
	return len(p), nil
}

func trustRequest() *turns.InputRequest {
	return &turns.InputRequest{
		ID:     "req-1",
		Kind:   "trust_prompt",
		Prompt: "Do you trust the files in this folder?",
		Options: []turns.InputOption{
			{ID: "1", Alias: "proceed", Label: "Yes, proceed", Keys: []byte("1\r")},
			{ID: "2", Alias: "deny", Label: "No, exit", Keys: []byte("2\r")},
		},
	}
}

func newTestConv(opts Options, rec *keyRecorder) *Conversation {
	return &Conversation{
		opts:         opts,
		eventCh:      make(chan ConversationEvent, 8),
		closed:       make(chan struct{}),
		inputStateCh: make(chan struct{}, 1),
		queue:        newControlQueue(),
		writeStdin:   rec.write,
	}
}

// Policy "answer" auto-resolves server-side: keys are written, nothing is
// surfaced to the client, and the prompt stays pending until it clears.
func TestHandleInputRequested_PolicyAutoAnswer(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{
		Harness: "claude-code",
		InputPolicy: &InputPolicy{ByKind: map[string]Disposition{
			"trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
		}},
	}, rec)

	c.handleInputRequested(trustRequest())

	if got := string(rec.data); got != "1\r" {
		t.Errorf("wrote %q, want %q", got, "1\r")
	}
	if c.inputSurfaced {
		t.Error("inputSurfaced = true, want false for an auto-answered prompt")
	}
	if c.currentInput == nil {
		t.Error("currentInput cleared too early; should persist until InputResolved")
	}
	select {
	case ev := <-c.eventCh:
		t.Errorf("unexpected surfaced event for auto-answered prompt: %+v", ev)
	default:
	}
}

// Policy "deny" picks the option aliased "deny".
func TestHandleInputRequested_PolicyDeny(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{
		Harness:     "claude-code",
		InputPolicy: &InputPolicy{Default: DispositionDeny},
	}, rec)

	c.handleInputRequested(trustRequest())
	if got := string(rec.data); got != "2\r" {
		t.Errorf("wrote %q, want %q (the deny option)", got, "2\r")
	}
}

// In-process handler resolves when the policy says ask / is absent.
func TestHandleInputRequested_HandlerResolves(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{
		Harness: "claude-code",
		OnInputRequest: func(r InputRequest) (InputAnswer, bool) {
			if r.Kind != "trust_prompt" {
				return InputAnswer{}, false
			}
			return InputAnswer{OptionID: "deny"}, true
		},
	}, rec)

	c.handleInputRequested(trustRequest())
	if got := string(rec.data); got != "2\r" {
		t.Errorf("wrote %q, want %q", got, "2\r")
	}
	if c.inputSurfaced {
		t.Error("inputSurfaced = true, want false (handler resolved it)")
	}
}

// No policy / handler → the request is surfaced on Events() and marked
// awaiting-client so Send fails fast.
func TestHandleInputRequested_SurfacesToClient(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{Harness: "claude-code"}, rec)

	c.handleInputRequested(trustRequest())

	if len(rec.data) != 0 {
		t.Errorf("wrote %q, want nothing when surfacing", rec.data)
	}
	if !c.inputAwaitingClient() {
		t.Error("inputAwaitingClient() = false, want true")
	}
	select {
	case ev := <-c.eventCh:
		if ev.Type != EventInputRequest {
			t.Errorf("event type = %q, want %q", ev.Type, EventInputRequest)
		}
		if ev.Input == nil || ev.Input.ID != "req-1" {
			t.Errorf("event Input = %+v, want request req-1", ev.Input)
		}
		if len(ev.Input.Options) != 2 || ev.Input.Options[0].Label != "Yes, proceed" {
			t.Errorf("surfaced options not propagated: %+v", ev.Input)
		}
	case <-time.After(time.Second):
		t.Fatal("no EventInputRequest surfaced")
	}
}

func TestAnswer(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{Harness: "claude-code"}, rec)

	// No control held yet.
	if err := c.Answer(context.Background(), "req-1", InputAnswer{OptionID: "proceed"}); err != ErrNoControl {
		t.Fatalf("Answer without control = %v, want ErrNoControl", err)
	}

	release, err := c.queue.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	// Control held but nothing pending.
	if err := c.Answer(context.Background(), "", InputAnswer{OptionID: "proceed"}); err != ErrNoInputPending {
		t.Fatalf("Answer with no pending = %v, want ErrNoInputPending", err)
	}

	// Surface a request, then answer it.
	c.handleInputRequested(trustRequest())

	if err := c.Answer(context.Background(), "wrong-id", InputAnswer{OptionID: "proceed"}); err != ErrStaleInputRequest {
		t.Fatalf("Answer with stale id = %v, want ErrStaleInputRequest", err)
	}
	if err := c.Answer(context.Background(), "req-1", InputAnswer{OptionID: "nope"}); err != ErrUnknownOption {
		t.Fatalf("Answer with bad option = %v, want ErrUnknownOption", err)
	}
	if err := c.Answer(context.Background(), "req-1", InputAnswer{OptionID: "proceed"}); err != nil {
		t.Fatalf("Answer = %v, want nil", err)
	}
	if got := string(rec.data); got != "1\r" {
		t.Errorf("wrote %q, want %q", got, "1\r")
	}
}

func TestHandleInputResolved_ClearsAndNotifies(t *testing.T) {
	rec := &keyRecorder{}
	c := newTestConv(Options{Harness: "claude-code"}, rec)
	c.handleInputRequested(trustRequest()) // surfaces; drains one event below
	<-c.eventCh

	c.handleInputResolved(&turns.InputRequest{ID: "req-1"})

	if c.currentInput != nil {
		t.Error("currentInput not cleared after InputResolved")
	}
	if c.inputAwaitingClient() {
		t.Error("inputAwaitingClient() = true after resolve, want false")
	}
	select {
	case ev := <-c.eventCh:
		if ev.Type != EventInputResolved {
			t.Errorf("event type = %q, want %q", ev.Type, EventInputResolved)
		}
	case <-time.After(time.Second):
		t.Fatal("no EventInputResolved emitted")
	}
}
