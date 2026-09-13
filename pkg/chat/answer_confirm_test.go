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
//	    row is "No, exit" — and no second arrow either;
//	(c) a dialog that visibly RESETS after an answer is re-answered a bounded
//	    number of times with keys RECOMPUTED from the live screen, then fails
//	    with the typed error instead of stalling to the caller's run deadline;
//	(d) a dialog that clears on the first answer is answered exactly once, and
//	    one that merely stays as it was gets no second Enter.
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
	// Exactly one arrow: a frame that did not move cannot say whether the
	// first arrow was lost or is still in flight, and a second one could walk
	// the real highlight past the target.
	if n := f.countOf(navDown); n != 1 {
		t.Errorf("wrote navigation %d times, want 1 — no blind re-send", n)
	}
	var ue *InputUnresolvedError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *InputUnresolvedError", err)
	}
	if !errors.Is(err, ErrInputUnresolved) {
		t.Errorf("errors.Is(err, ErrInputUnresolved) = false for %v", err)
	}
	if ue.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0: no answer was written", ue.Attempts)
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

// The unsafe case the previous contract REQUIRED: the first answer leaves the
// highlight on the target and the dialog up, unchanged. That frame cannot tell
// a rejected Enter from an accepted one whose next screen has not painted, and
// in the second case a bare Enter lands on the next dialog. So there is no
// second Enter: the answer ends with the typed error, one answer written.
func TestAnswerAndConfirm_UnchangedFrameAfterEnterGetsNoSecondEnter(t *testing.T) {
	f := newAnswerFake(t, 60*time.Millisecond, trustFrameMarkerOnExit, func(f *answerFake, p []byte) {
		if bytes.Equal(p, navDown) || bytes.Equal(p, confirm) {
			f.paint(trustFrameMarkerOnTrust) // the dialog survives, highlight where it was put
		}
	})
	req, opt := trustRequestUnnumbered(t)

	err := f.c.answerAndConfirm(context.Background(), req, opt)
	var ue *InputUnresolvedError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *InputUnresolvedError", err)
	}
	if n := f.countOf(confirm); n != 1 {
		t.Errorf("confirm key written %d times, want 1 — an unchanged frame is no evidence the Enter was rejected", n)
	}
	if ue.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", ue.Attempts)
	}
}

// threeRowTrust is an unnumbered trust-shaped dialog with a third row, so the
// row a reset lands on can differ from where navigation started.
func threeRowTrust(marker int) string {
	rows := []string{"No, exit", "Yes, I trust this folder", "Yes, and remember"}
	out := "Quick safety check: Is this a project you created or one you trust?\r\n\r\n"
	for i, r := range rows {
		if i == marker {
			out += "❯ " + r + "\r\n"
		} else {
			out += "  " + r + "\r\n"
		}
	}
	return out + "\r\nEnter to confirm · Esc to cancel\r\n"
}

// Every re-answer is computed from the screen as it looks NOW. The dialog
// resets with its highlight BELOW the target, so the correct re-answer is Up,
// and replaying the original Down would walk away from the target.
func TestAnswerAndConfirm_RecomputesKeysFromTheLiveScreen(t *testing.T) {
	navUp := []byte("\x1b[A")
	marker, answered := 0, 0
	f := newAnswerFake(t, 2*time.Second, threeRowTrust(0), func(f *answerFake, p []byte) {
		switch {
		case bytes.Equal(p, navDown):
			marker++
			f.paint(threeRowTrust(marker))
		case bytes.Equal(p, navUp):
			marker--
			f.paint(threeRowTrust(marker))
		case bytes.Equal(p, confirm):
			answered++
			if answered == 1 {
				marker = 2 // reset, landing below the target
				f.paint(threeRowTrust(marker))
				return
			}
			f.paint(composerFrame)
		}
	})
	req, ok := claudecode.DetectInput(threeRowTrust(0))
	if !ok {
		t.Fatal("three-row fixture does not detect")
	}
	opt := findOption(req, trustLabel)
	if opt == nil {
		t.Fatalf("no %q option in %+v", trustLabel, req.Options)
	}

	if err := f.c.answerAndConfirm(context.Background(), req, opt); err != nil {
		t.Fatalf("answerAndConfirm: %v", err)
	}
	if got := f.writtenStrings(); strings.Join(got, "|") != strings.Join([]string{string(navDown), string(confirm), string(navUp), string(confirm)}, "|") {
		t.Errorf("writes = %q, want Down, Enter, then the recomputed Up, Enter", got)
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
