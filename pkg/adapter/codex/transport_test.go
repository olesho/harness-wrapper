package codex

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// stdinLines is codex's stdin, as a test reads it.
type stdinLines struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *stdinLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *stdinLines) Close() error { return nil }

func (l *stdinLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// folded is a transport on a turn of codex's own that an input was folded
// into: the turn started, then turn/start answered with its id, as codex
// answers an input it folds into the turn it is on.
func folded(t *testing.T) (*transport, *inputState, *stdinLines, func() []adapter.Event) {
	t.Helper()
	var mu sync.Mutex
	var evs []adapter.Event
	stdin := &stdinLines{}
	tr := &transport{
		report: func(ev adapter.Event) {
			mu.Lock()
			evs = append(evs, ev)
			mu.Unlock()
		},
		stdin: stdin, thread: "thread-1",
		calls: map[int64]chan rpcResult{}, wake: make(chan struct{}), exited: make(chan struct{}),
	}
	tr.onNotification("turn/started", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"own-1","status":"inProgress"}}`))
	in := &inputState{native: "in-1", text: "PING", call: 7}
	tr.mu.Lock()
	tr.in = in
	tr.mu.Unlock()
	tr.onAnswer(7, &message{Result: json.RawMessage(`{"turn":{"id":"own-1","status":"inProgress"}}`)})
	return tr, in, stdin, func() []adapter.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]adapter.Event(nil), evs...)
	}
}

// due runs what the input's wait arranged, now, as its timer would.
func due(tr *transport, in *inputState) {
	tr.mu.Lock()
	again := in.again
	tr.mu.Unlock()
	if again == nil {
		return
	}
	again.Stop()
	tr.resend(in)
}

func (tr *transport) holds(in *inputState) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.in == in
}

// A turn of codex's own that ends other than interrupted, never taking in
// the input it held, leaves codex maybe holding it still, and codex keeps no
// record of a client id: sent again, it could run twice. It is not sent
// again; codex starting no turn for it, it ends errored.
func TestHeldInputIsNotSentAgain(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			tr, in, stdin, events := folded(t)
			tr.onNotification("turn/completed", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"own-1","status":"`+status+`"}}`))
			due(tr, in)
			if strings.Contains(stdin.String(), "turn/start") {
				t.Errorf("the held input was sent again: %s", stdin.String())
			}
			evs := events()
			if last := evs[len(evs)-1]; last.Kind != adapter.Ended || last.Native != "in-1" || last.Outcome != contract.TurnErrored ||
				last.Error == nil || last.Error.Class != contract.ErrorInternal {
				t.Errorf("the held input's end: %+v, want errored, internal", last)
			}
			if tr.holds(in) {
				t.Error("the transport still holds the input")
			}
		})
	}
}

// A held input that a turn of codex's own takes in before the wait runs out
// is that turn's: it neither ends nor is sent again.
func TestHeldInputTakenInLater(t *testing.T) {
	tr, in, stdin, events := folded(t)
	tr.onNotification("turn/completed", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"own-1","status":"completed"}}`))
	tr.onNotification("turn/started", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"own-2","status":"inProgress"}}`))
	tr.onNotification("item/started", json.RawMessage(`{"threadId":"thread-1","turnId":"own-2","item":{"type":"userMessage","id":"m-1","clientId":"in-1"}}`))
	due(tr, in)
	if strings.Contains(stdin.String(), "turn/start") {
		t.Errorf("the input a turn took in was sent again: %s", stdin.String())
	}
	started := false
	for _, ev := range events() {
		started = started || ev.Kind == adapter.Started && ev.Native == "in-1"
		if ev.Kind == adapter.Ended && ev.Native == "in-1" {
			t.Errorf("the input a turn took in ended: %+v", ev)
		}
	}
	if !started || !tr.holds(in) {
		t.Errorf("the input is not the turn's that took it in (started: %v)", started)
	}
}

// An input a turn of codex's own dropped — it was interrupted first — is sent
// again under its client id: codex never ran it.
func TestDroppedInputIsSentAgain(t *testing.T) {
	tr, in, stdin, events := folded(t)
	tr.onNotification("turn/completed", json.RawMessage(`{"threadId":"thread-1","turn":{"id":"own-1","status":"interrupted"}}`))
	due(tr, in)
	if !strings.Contains(stdin.String(), `"clientUserMessageId":"in-1"`) {
		t.Errorf("the dropped input was not sent again: %q", stdin.String())
	}
	for _, ev := range events() {
		if ev.Kind == adapter.Ended && ev.Native == "in-1" {
			t.Errorf("the dropped input ended: %+v", ev)
		}
	}
}
