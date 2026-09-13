//go:build screenbench

package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedDialog is a minimal fake for answer_dialog's failure arms: it paints
// frame at start and hands every write to onWrite, which may repaint.
type scriptedDialog struct {
	d       *scriptDriver
	mu      sync.Mutex
	writes  [][]byte
	onWrite func(p []byte) (repaint string)
}

func (s *scriptedDialog) WriteStdin(p []byte) (int, error) {
	s.mu.Lock()
	s.writes = append(s.writes, append([]byte(nil), p...))
	s.mu.Unlock()
	if s.onWrite != nil {
		if frame := s.onWrite(p); frame != "" {
			_, _ = s.d.Write([]byte("\x1b[2J\x1b[H" + frame))
		}
	}
	return len(p), nil
}

func (s *scriptedDialog) sent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.writes...)
}

func trustFrame(markerOn string) string {
	var b strings.Builder
	b.WriteString(trustAnchor + "\r\n\r\n")
	for _, o := range []string{trustNo, trustYes} {
		if o == markerOn {
			b.WriteString("❯ " + o + "\r\n")
		} else {
			b.WriteString("  " + o + "\r\n")
		}
	}
	b.WriteString("\r\nEnter to confirm · Esc to cancel\r\n")
	return b.String()
}

func newDialogDriver(t *testing.T, first string, onWrite func([]byte) string) (*scriptDriver, *scriptedDialog) {
	t.Helper()
	d := newScriptDriver(nil, 200*time.Millisecond, 0)
	f := &scriptedDialog{d: d, onWrite: onWrite}
	d.stdin = f
	_, _ = d.Write([]byte("\x1b[2J\x1b[H" + first))
	return d, f
}

func TestAnswerDialog_NoSuchOptionIsAnError(t *testing.T) {
	d, f := newDialogDriver(t, trustFrame(trustNo), nil)
	err := d.answerDialog(context.Background(), "Yes, proceed", time.Second)
	if err == nil || !strings.Contains(err.Error(), "offers no option") {
		t.Fatalf("err = %v, want a no-such-option error", err)
	}
	if w := f.sent(); len(w) != 0 {
		t.Errorf("wrote %q to a dialog it could not answer", w)
	}
}

// The highlight never reaches the target: navigation is written, the confirm
// key never is. Enter here would select "No, exit".
func TestAnswerDialog_NeverConfirmsOnTheWrongRow(t *testing.T) {
	d, f := newDialogDriver(t, trustFrame(trustNo), func([]byte) string { return "" }) // arrows ignored
	err := d.answerDialog(context.Background(), trustYes, time.Second)
	if err == nil || !strings.Contains(err.Error(), "never reached") {
		t.Fatalf("err = %v, want the highlight-never-landed error", err)
	}
	for _, w := range f.sent() {
		if bytes.Contains(w, []byte("\r")) {
			t.Fatalf("wrote a confirm key %q with the highlight on %q", w, trustNo)
		}
	}
}

func TestAnswerDialog_DialogThatWillNotClearIsAnError(t *testing.T) {
	d, _ := newDialogDriver(t, trustFrame(trustNo), func(p []byte) string {
		if bytes.Equal(p, []byte("\x1b[B")) {
			return trustFrame(trustYes)
		}
		return trustFrame(trustYes) // Enter is swallowed: the dialog stays
	})
	err := d.answerDialog(context.Background(), trustYes, time.Second)
	if err == nil || !strings.Contains(err.Error(), "still up") {
		t.Fatalf("err = %v, want the still-up error", err)
	}
}

func TestAnswerDialog_AnswersInTwoWritesAndReturnsOnceCleared(t *testing.T) {
	d, f := newDialogDriver(t, trustFrame(trustNo), func(p []byte) string {
		if bytes.Equal(p, []byte("\x1b[B")) {
			return trustFrame(trustYes)
		}
		return "╭────╮\r\n│ ❯  │\r\n╰────╯\r\n"
	})
	if err := d.answerDialog(context.Background(), trustYes, time.Second); err != nil {
		t.Fatalf("answerDialog: %v", err)
	}
	got := f.sent()
	if len(got) != 2 || string(got[0]) != "\x1b[B" || string(got[1]) != "\r" {
		t.Fatalf("writes = %q, want navigation then a separate confirm", got)
	}
}

func TestAnswerDialog_NumberedMenuIsOneWrite(t *testing.T) {
	// The numbered trust dialog claude shipped before 2.1.251.
	numbered := "Do you trust the files in this folder?\r\n\r\n❯ 1. Yes, proceed\r\n  2. No, exit\r\n\r\nEnter to confirm · Esc to exit\r\n"
	d, f := newDialogDriver(t, numbered, func([]byte) string { return "╭────╮\r\n│ ❯  │\r\n╰────╯\r\n" })
	if err := d.answerDialog(context.Background(), "Yes, proceed", time.Second); err != nil {
		t.Fatalf("answerDialog: %v", err)
	}
	if got := f.sent(); len(got) != 1 || string(got[0]) != "1\r" {
		t.Fatalf("writes = %q, want the single digit answer %q", got, "1\r")
	}
}

func TestAnswerDialog_AbsentDialogIsANoOp(t *testing.T) {
	d, f := newDialogDriver(t, "╭────╮\r\n│ ❯  │\r\n╰────╯\r\n", nil)
	start := time.Now()
	if err := d.answerDialog(context.Background(), trustYes, 5*time.Second); err != nil {
		t.Fatalf("answerDialog: %v", err)
	}
	if w := f.sent(); len(w) != 0 {
		t.Errorf("wrote %q with no dialog on screen", w)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v; an absent dialog should end the step once the screen settles", time.Since(start))
	}
}

func TestValidateStep_Within(t *testing.T) {
	if err := validateStep(scriptStep{Sleep: "1s", Within: "2s"}); err == nil {
		t.Error("within accepted on a sleep step")
	}
	if err := validateStep(scriptStep{AnswerDialog: trustYes, Within: "soon"}); err == nil {
		t.Error("an unparseable within was accepted")
	}
	if err := validateStep(scriptStep{AnswerDialog: trustYes, Within: "10s"}); err != nil {
		t.Errorf("valid answer_dialog step rejected: %v", err)
	}
}
