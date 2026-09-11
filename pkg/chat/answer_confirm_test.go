package chat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// What these lock, in one sentence each:
//
//	(a) navigation and the confirm key are TWO writes, and the confirm one is
//	    written only after the marker has actually landed on the target row;
//	(b) a marker that never moves produces NO confirm key at all — pressing it
//	    would select whatever row the marker is on, and on a trust dialog that
//	    row is "No, exit";
//	(c) a dialog that stays up is re-answered a bounded number of times with
//	    keys RECOMPUTED from the live screen, then fails with the typed error
//	    instead of stalling to the caller's run deadline;
//	(d) a dialog that clears on the first answer is answered exactly once.
//
// (c) is the fleet-wide hang this change exists for: before it, one ineffective
// answer was permanent, because the adapter re-emits InputRequested only when
// the request ID changes and the ID does not depend on where the highlight is.

// trustFrameMarkerOnExit / trustFrameMarkerOnTrust are the claude-code 2.1.261
// folder-trust dialog with the ❯ on each of its two rows.
const (
	trustFrameMarkerOnExit = "Quick safety check: Is this a project you created or one you trust?\r\n" +
		"\r\n" +
		"❯ No, exit\r\n" +
		"  Yes, I trust this folder\r\n" +
		"\r\n" +
		"Enter to confirm · Esc to cancel\r\n"

	trustFrameMarkerOnTrust = "Quick safety check: Is this a project you created or one you trust?\r\n" +
		"\r\n" +
		"  No, exit\r\n" +
		"❯ Yes, I trust this folder\r\n" +
		"\r\n" +
		"Enter to confirm · Esc to cancel\r\n"

	// composerFrame carries no dialog anchor at all — the dialog is gone.
	composerFrame = "❯ Try \"how do I...\"\r\n"

	trustLabel = "Yes, I trust this folder"
)

// answerFake drives a Conversation's screen from its writes: onWrite sees each
// keystroke burst and repaints however the scenario says claude would.
type answerFake struct {
	t   *testing.T
	c   *Conversation
	scr *screen.Screen

	mu     sync.Mutex
	writes [][]byte
	stamps []time.Time

	onWrite func(f *answerFake, p []byte)
}

func newAnswerFake(t *testing.T, budget time.Duration, first string, onWrite func(*answerFake, []byte)) *answerFake {
	t.Helper()
	f := &answerFake{t: t, scr: screen.New(120, 40), onWrite: onWrite}
	f.c = &Conversation{
		opts: Options{
			Harness:               chatClaudeCode,
			permModeRenderTimeout: budget,
		},
		screen:       f.scr,
		eventCh:      make(chan ConversationEvent, 8),
		closed:       make(chan struct{}),
		inputStateCh: make(chan struct{}, 1),
		queue:        newControlQueue(),
	}
	f.c.writeStdin = f.write
	f.paint(first)
	return f
}

// paint replaces the whole screen with frame.
func (f *answerFake) paint(frame string) {
	if _, err := f.scr.Write([]byte("\x1b[2J\x1b[H" + frame)); err != nil {
		f.t.Fatalf("paint: %v", err)
	}
}

func (f *answerFake) write(p []byte) (int, error) {
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), p...))
	f.stamps = append(f.stamps, time.Now())
	f.mu.Unlock()
	if f.onWrite != nil {
		f.onWrite(f, p)
	}
	return len(p), nil
}

func (f *answerFake) written() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.writes))
	copy(out, f.writes)
	return out
}

func (f *answerFake) countOf(want []byte) int {
	n := 0
	for _, w := range f.written() {
		if bytes.Equal(w, want) {
			n++
		}
	}
	return n
}

func (f *answerFake) writtenStrings() []string {
	var out []string
	for _, w := range f.written() {
		out = append(out, string(w))
	}
	return out
}

// trustRequestUnnumbered is the request DetectInput builds from the 2.1.261
// dialog: no digits to press, so the proceed option is Down + Enter.
func trustRequestUnnumbered(t *testing.T) (*turns.InputRequest, *turns.InputOption) {
	t.Helper()
	req, ok := claudecode.DetectInput(trustFrameMarkerOnExit)
	if !ok {
		t.Fatal("fixture no longer detects as a trust dialog")
	}
	opt := findOption(req, trustLabel)
	if opt == nil {
		t.Fatalf("no %q option in %+v", trustLabel, req.Options)
	}
	return req, opt
}

var (
	navDown = []byte("\x1b[B")
	confirm = []byte("\r")
)

// (a) Two writes, and the confirm key only after the marker has landed. The
// repaint is deliberately LATE, so a passing run proves the wait happened
// rather than that the write order was lucky.
func TestAnswerAndConfirm_ConfirmsNavigationBeforePressingEnter(t *testing.T) {
	var painted time.Time
	f := newAnswerFake(t, 2*time.Second, trustFrameMarkerOnExit, func(f *answerFake, p []byte) {
		switch {
		case bytes.Equal(p, navDown):
			go func() {
				time.Sleep(60 * time.Millisecond)
				painted = time.Now()
				f.paint(trustFrameMarkerOnTrust)
			}()
		case bytes.Equal(p, confirm):
			f.paint(composerFrame)
		}
	})
	req, opt := trustRequestUnnumbered(t)

	if err := f.c.answerAndConfirm(context.Background(), req, opt); err != nil {
		t.Fatalf("answerAndConfirm: %v", err)
	}

	got := f.writtenStrings()
	want := []string{string(navDown), string(confirm)}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("writes = %q, want the navigation and the confirm key as separate writes %q", got, want)
	}
	f.mu.Lock()
	enterAt := f.stamps[1]
	f.mu.Unlock()
	if painted.IsZero() || enterAt.Before(painted) {
		t.Fatalf("Enter written at %v, before the marker landed at %v — it did not wait for the highlight", enterAt, painted)
	}
}

// (b) The marker never moves: no Enter is written at all, and the caller gets
// the typed error. An Enter here would have selected "No, exit" and killed the
// session — the single most expensive thing this code can get wrong.
func TestAnswerAndConfirm_NeverPressesEnterOnTheWrongRow(t *testing.T) {
	f := newAnswerFake(t, 60*time.Millisecond, trustFrameMarkerOnExit, nil)
	req, opt := trustRequestUnnumbered(t)

	err := f.c.answerAndConfirm(context.Background(), req, opt)

	if n := f.countOf(confirm); n != 0 {
		t.Fatalf("wrote the confirm key %d times with the marker still on %q; it must never be pressed on the wrong row", n, "No, exit")
	}
	if n := f.countOf(navDown); n != answerAttempts {
		t.Errorf("wrote navigation %d times, want %d (one per bounded attempt)", n, answerAttempts)
	}
	var ue *InputUnresolvedError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *InputUnresolvedError", err)
	}
	if !errors.Is(err, ErrInputUnresolved) {
		t.Errorf("errors.Is(err, ErrInputUnresolved) = false for %v", err)
	}
	if ue.Attempts != answerAttempts {
		t.Errorf("Attempts = %d, want %d", ue.Attempts, answerAttempts)
	}
	if !strings.Contains(ue.Observed, "No, exit") {
		t.Errorf("Observed screen carries no evidence of the dialog:\n%s", ue.Observed)
	}
	if ue.Request.Kind != "trust_prompt" {
		t.Errorf("Request.Kind = %q, want trust_prompt", ue.Request.Kind)
	}
}

// (c) The measured symptom: the highlight moves, the answer is taken, and then
// the dialog repaints from its DEFAULT state. Bounded re-answers, keys
// recomputed from the live screen each time, then a fast typed failure.
func TestAnswerAndConfirm_RetriesABoundedNumberOfTimesThenFails(t *testing.T) {
	f := newAnswerFake(t, 60*time.Millisecond, trustFrameMarkerOnExit, func(f *answerFake, p []byte) {
		switch {
		case bytes.Equal(p, navDown):
			f.paint(trustFrameMarkerOnTrust)
		case bytes.Equal(p, confirm):
			// Claude repaints the dialog from scratch: the highlight is back on
			// the default and the dialog is still up.
			f.paint(trustFrameMarkerOnExit)
		}
	})
	req, opt := trustRequestUnnumbered(t)

	start := time.Now()
	err := f.c.answerAndConfirm(context.Background(), req, opt)
	elapsed := time.Since(start)

	if n := f.countOf(navDown); n != answerAttempts {
		t.Errorf("navigation written %d times, want exactly %d", n, answerAttempts)
	}
	if n := f.countOf(confirm); n != answerAttempts {
		t.Errorf("confirm key written %d times, want exactly %d", n, answerAttempts)
	}
	if !errors.Is(err, ErrInputUnresolved) {
		t.Fatalf("err = %v, want ErrInputUnresolved", err)
	}
	// Fast-fail is the point: the alternative was a 43-minute run deadline.
	if elapsed > 5*time.Second {
		t.Errorf("took %v to give up; the whole point is to fail in seconds", elapsed)
	}
}

// Every re-answer must be computed from the screen as it looks NOW. Here the
// first answer leaves the highlight ON the target, so replaying the original
// Down would walk PAST it — the correct second answer is a bare Enter.
func TestAnswerAndConfirm_RecomputesKeysFromTheLiveScreen(t *testing.T) {
	answered := 0
	f := newAnswerFake(t, 60*time.Millisecond, trustFrameMarkerOnExit, func(f *answerFake, p []byte) {
		if bytes.Equal(p, confirm) {
			answered++
			if answered == 1 {
				// Dialog survives, but the highlight stays where it was put.
				f.paint(trustFrameMarkerOnTrust)
				return
			}
			f.paint(composerFrame)
			return
		}
		if bytes.Equal(p, navDown) {
			f.paint(trustFrameMarkerOnTrust)
		}
	})
	req, opt := trustRequestUnnumbered(t)

	if err := f.c.answerAndConfirm(context.Background(), req, opt); err != nil {
		t.Fatalf("answerAndConfirm: %v", err)
	}
	if n := f.countOf(navDown); n != 1 {
		t.Errorf("navigation written %d times, want 1 — the retry must not replay arrows that would walk past the target", n)
	}
	if n := f.countOf(confirm); n != 2 {
		t.Errorf("confirm key written %d times, want 2 (the retry is a bare Enter on the already-highlighted row)", n)
	}
}

// (d) The happy path answers ONCE. A second answer into a menu that already
// took the first is the double-confirm submit.go warns about.
func TestAnswerAndConfirm_AnswersExactlyOnceWhenTheDialogClears(t *testing.T) {
	f := newAnswerFake(t, 2*time.Second, trustFrameMarkerOnExit, func(f *answerFake, p []byte) {
		switch {
		case bytes.Equal(p, navDown):
			f.paint(trustFrameMarkerOnTrust)
		case bytes.Equal(p, confirm):
			f.paint(composerFrame)
		}
	})
	req, opt := trustRequestUnnumbered(t)

	if err := f.c.answerAndConfirm(context.Background(), req, opt); err != nil {
		t.Fatalf("answerAndConfirm: %v", err)
	}
	if got := len(f.written()); got != 2 {
		t.Fatalf("wrote %d bursts (%q), want exactly 2 — one navigation, one confirm", got, f.writtenStrings())
	}
}

// A numbered menu selects by digit, so there is no highlight to watch and no
// wrong row to land on: it must stay ONE write, exactly as before.
func TestSplitNavKeys_LeavesADigitAnswerAlone(t *testing.T) {
	if _, _, ok := splitNavKeys([]byte("2\r")); ok {
		t.Error("split a numbered answer; a digit selects its row regardless of the highlight")
	}
	if _, _, ok := splitNavKeys([]byte("\r")); ok {
		t.Error("split a bare Enter, which has no navigation at all")
	}
	nav, conf, ok := splitNavKeys([]byte("\x1b[B\x1b[B\r"))
	if !ok {
		t.Fatal("did not split an arrow answer")
	}
	if string(nav) != "\x1b[B\x1b[B" || string(conf) != "\r" {
		t.Errorf("split as (%q, %q), want (%q, %q)", nav, conf, "\x1b[B\x1b[B", "\r")
	}
}

// A stall must fail a Send in seconds rather than blocking to the caller's run
// deadline, and it must do so carrying the evidence.
func TestWaitReadyForSend_FailsFastOnAnUnresolvedPrompt(t *testing.T) {
	f := newAnswerFake(t, 60*time.Millisecond, trustFrameMarkerOnExit, nil)
	req, _ := trustRequestUnnumbered(t)
	f.c.opts.InputPolicy = &InputPolicy{ByKind: map[string]Disposition{
		"trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
	}}

	f.c.handleInputRequested(req)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := f.c.waitReadyForSend(ctx)

	var ue *InputUnresolvedError
	if !errors.As(err, &ue) {
		t.Fatalf("waitReadyForSend = %v, want *InputUnresolvedError (ErrInputPending alone loses the evidence)", err)
	}
	if ue.Request.ID != req.ID {
		t.Errorf("error names request %q, want %q", ue.Request.ID, req.ID)
	}
	// The prompt is also surfaced, so a live client can still answer it by hand.
	if !f.c.inputAwaitingClient() {
		t.Error("an unresolvable prompt was not surfaced to the client")
	}
}

// The latch must not outlive the prompt: once the dialog genuinely clears, the
// conversation is sendable again.
func TestUnresolvedInputLatch_ClearsWhenThePromptResolves(t *testing.T) {
	f := newAnswerFake(t, 60*time.Millisecond, trustFrameMarkerOnExit, nil)
	req, _ := trustRequestUnnumbered(t)
	f.c.opts.InputPolicy = &InputPolicy{Default: DispositionAnswer, ByKind: map[string]Disposition{
		"trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
	}}

	f.c.handleInputRequested(req)
	if f.c.inputBlocked() == nil {
		t.Fatal("inputBlocked() = nil after a failed auto-answer")
	}

	f.c.handleInputResolved(req)
	if err := f.c.inputBlocked(); err != nil {
		t.Fatalf("inputBlocked() = %v after the prompt resolved, want nil", err)
	}
}
