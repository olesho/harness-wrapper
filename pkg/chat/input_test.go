package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
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

// multiSelectRequest is a synthetic clarifying-question prompt: MultiSelect
// with TOGGLE-ONLY option Keys (no submit baked in). No adapter produces this
// yet, so it is constructed directly to exercise the answer plumbing.
func multiSelectRequest() *turns.InputRequest {
	return &turns.InputRequest{
		ID:          "q-1",
		Kind:        "question",
		Prompt:      "Which do you want?",
		Header:      "Choices",
		MultiSelect: true,
		Options: []turns.InputOption{
			{ID: "1", Alias: "a", Label: "Alpha", Keys: []byte("a")},
			{ID: "2", Alias: "b", Label: "Beta", Keys: []byte("b")},
			{ID: "3", Alias: "c", Label: "Gamma", Keys: []byte("c")},
		},
	}
}

// newMultiSelectConv wires a real screen so submitKeyForHarness (which reads
// c.screen.Snapshot().Text) does not deref a nil screen.
func newMultiSelectConv(rec *keyRecorder) *Conversation {
	c := newTestConv(Options{Harness: "claude-code"}, rec)
	c.screen = screen.New(80, 24)
	return c
}

// claude-code's submit key (see submitKeyForHarness).
const ccSubmit = "\x1b[13u"

func answerHeld(t *testing.T, c *Conversation, req *turns.InputRequest, ans InputAnswer) error {
	t.Helper()
	release, err := c.queue.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	c.handleInputRequested(req)
	return c.Answer(context.Background(), req.ID, ans)
}

// Distinct OptionIDs toggle each option in order, then submit exactly once.
func TestMultiSelect_DistinctTogglesThenSubmit(t *testing.T) {
	rec := &keyRecorder{}
	c := newMultiSelectConv(rec)
	if err := answerHeld(t, c, multiSelectRequest(), InputAnswer{OptionIDs: []string{"1", "2"}}); err != nil {
		t.Fatalf("Answer = %v, want nil", err)
	}
	if got, want := string(rec.data), "ab"+ccSubmit; got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

// A repeated id and an id+matching-label both resolve to the same option and
// must collapse to a single toggle (double-toggle would net-deselect).
func TestMultiSelect_DedupByResolvedOption(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"repeated-id", []string{"1", "1"}},
		{"id-and-label", []string{"1", "Alpha"}},
		{"id-and-alias", []string{"1", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &keyRecorder{}
			c := newMultiSelectConv(rec)
			if err := answerHeld(t, c, multiSelectRequest(), InputAnswer{OptionIDs: tc.ids}); err != nil {
				t.Fatalf("Answer = %v, want nil", err)
			}
			if got, want := string(rec.data), "a"+ccSubmit; got != want {
				t.Errorf("wrote %q, want %q", got, want)
			}
		})
	}
}

// Every error channel must write NOTHING.
func TestMultiSelect_ErrorsWriteNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *turns.InputRequest
		ans  InputAnswer
		want error
	}{
		// Empty OptionIDs on a MultiSelect request falls through to the
		// single-select path — findOption(req, "") → nil → ErrUnknownOption,
		// the decided "empty multi-select answer unsupported" contract.
		{"empty-on-multiselect", multiSelectRequest(), InputAnswer{OptionIDs: []string{}}, ErrUnknownOption},
		{"single-select", trustRequest(), InputAnswer{OptionIDs: []string{"proceed"}}, ErrNotMultiSelect},
		{"conflicting", multiSelectRequest(), InputAnswer{OptionID: "1", OptionIDs: []string{"2"}}, ErrConflictingAnswer},
		{"unknown-id", multiSelectRequest(), InputAnswer{OptionIDs: []string{"1", "nope"}}, ErrUnknownOption},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &keyRecorder{}
			c := newMultiSelectConv(rec)
			err := answerHeld(t, c, tc.req, tc.ans)
			if err != tc.want {
				t.Fatalf("Answer = %v, want %v", err, tc.want)
			}
			if len(rec.data) != 0 {
				t.Errorf("wrote %q, want nothing on error", rec.data)
			}
		})
	}
}

// PendingInput returns nil while a request is set-but-not-surfaced (being
// auto-answered) and the client-facing view once inputSurfaced flips true.
func TestPendingInput_GatesOnSurfaced(t *testing.T) {
	rec := &keyRecorder{}

	// Auto-answered by policy: currentInput is set but inputSurfaced stays
	// false, so PendingInput must report nil (consistent with
	// inputAwaitingClient()).
	cAuto := newTestConv(Options{
		Harness: "claude-code",
		InputPolicy: &InputPolicy{ByKind: map[string]Disposition{
			"trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
		}},
	}, rec)
	cAuto.handleInputRequested(trustRequest())
	if cAuto.currentInput == nil {
		t.Fatal("precondition: currentInput should be set during auto-answer")
	}
	if cAuto.PendingInput() != nil {
		t.Error("PendingInput() != nil while not surfaced")
	}
	if cAuto.inputAwaitingClient() {
		t.Error("inputAwaitingClient() = true while not surfaced (should agree with PendingInput)")
	}

	// Surfaced request: PendingInput returns the client-facing view.
	cSurf := newTestConv(Options{Harness: "claude-code"}, rec)
	cSurf.handleInputRequested(trustRequest())
	pi := cSurf.PendingInput()
	if pi == nil {
		t.Fatal("PendingInput() = nil after surfacing, want the request")
	}
	if pi.ID != "req-1" || len(pi.Options) != 2 {
		t.Errorf("PendingInput() = %+v, want req-1 with 2 options", pi)
	}
	if !cSurf.inputAwaitingClient() {
		t.Error("inputAwaitingClient() = false while surfaced (should agree with PendingInput)")
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

// freeTextRequest is a free-text prompt: a request with NO options, so the
// answer is composer text rather than a menu keystroke.
func freeTextRequest() *turns.InputRequest {
	return &turns.InputRequest{
		ID:     "ft-1",
		Kind:   "question",
		Prompt: "What should I do next?",
	}
}

// echoingRecorder is a composer that DOES NOT echo until a second frame: the
// echo is written to the screen from another goroutine a beat after the text
// write lands, exactly as a PTY would deliver it. Each PTY write is recorded as
// its own frame, together with whether the echo was on screen at the time — so
// a submit key riding in the same write as the text, or written before the
// composer showed the text, is caught.
type echoingRecorder struct {
	mu      sync.Mutex
	writes  []string
	sawEcho []bool

	scr      *screen.Screen
	echoText string
	echoDone sync.Once
}

func (e *echoingRecorder) write(p []byte) (int, error) {
	e.mu.Lock()
	e.writes = append(e.writes, string(p))
	e.sawEcho = append(e.sawEcho, strings.Contains(e.scr.Snapshot().Text, e.echoText))
	e.mu.Unlock()

	// The first write is the text; the composer renders it one frame later.
	e.echoDone.Do(func() {
		go func() {
			time.Sleep(20 * time.Millisecond)
			_, _ = e.scr.Write([]byte(e.echoText))
		}()
	})
	return len(p), nil
}

func (e *echoingRecorder) frames() ([]string, []bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.writes...), append([]bool(nil), e.sawEcho...)
}

// newFreeTextConv wires a real screen (the echo target) and an answer-path
// writer that feeds it.
func newFreeTextConv(rec *echoingRecorder) *Conversation {
	c := newTestConv(Options{Harness: "claude-code"}, &keyRecorder{})
	c.screen = screen.New(80, 24)
	c.writeStdin = rec.write
	rec.scr = c.screen
	return c
}

// A free-text answer goes out as TWO writes — text, then the submit key — with
// the composer echo awaited in between. One combined write is what lets Claude
// Code collapse the arrival into a paste and swallow the submit.
func TestAnswer_FreeTextSplitsTextFromSubmit(t *testing.T) {
	const answer = "Ship the fleet plan review\nsecond line\nthird line"
	rec := &echoingRecorder{echoText: "Ship the fleet plan review"}
	c := newFreeTextConv(rec)

	if err := answerHeld(t, c, freeTextRequest(), InputAnswer{Text: answer}); err != nil {
		t.Fatalf("Answer = %v, want nil", err)
	}

	writes, sawEcho := rec.frames()
	if len(writes) != 2 {
		t.Fatalf("wrote %d frames (%q), want 2 (text, then submit)", len(writes), writes)
	}
	if writes[0] != answer {
		t.Errorf("frame 0 = %q, want the answer text %q", writes[0], answer)
	}
	if writes[1] != ccSubmit {
		t.Errorf("frame 1 = %q, want the submit key %q", writes[1], ccSubmit)
	}
	if sawEcho[0] {
		t.Error("the composer had already echoed before the text was written; the fake is not exercising the wait")
	}
	if !sawEcho[1] {
		t.Error("submit key written before the composer echoed the text")
	}
}

// A cancelled context is the whole run ending: the submit must NOT be written
// into a dead session, and the error propagates rather than degrading.
func TestAnswer_FreeTextCancelledCtxWritesNoSubmit(t *testing.T) {
	rec := &echoingRecorder{echoText: "never echoed"}
	c := newFreeTextConv(rec)

	release, err := c.queue.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	c.handleInputRequested(freeTextRequest())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Answer(ctx, "ft-1", InputAnswer{Text: "some answer"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Answer with cancelled ctx = %v, want context.Canceled", err)
	}
	if writes, _ := rec.frames(); len(writes) != 1 {
		t.Fatalf("wrote %d frames (%q), want 1 (the text only)", len(writes), writes)
	}
}
