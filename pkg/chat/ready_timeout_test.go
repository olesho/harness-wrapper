package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// trustDialogScreen mirrors the folder-trust dialog claudecode's own
// input_test.go pins: a bordered box whose "❯" selector would otherwise make the
// screen look like a ready composer. It is the concrete screen that motivated
// the readiness bound — nothing answers it without an InputPolicy, so the wait
// used to run to the caller's whole-run deadline.
const trustDialogScreen = `╭─────────────────────────────────────────────────╮
│ Do you trust the files in this folder?            │
│                                                   │
│ /Users/oleh/Work/sandbox                          │
│                                                   │
│ ❯ 1. Yes, proceed                                 │
│   2. No, exit                                     │
│                                                   │
│ Enter to confirm · Esc to exit                    │
╰─────────────────────────────────────────────────╯`

// newNotReadyConv builds a Conversation parked on the given screen with a short
// readiness budget, so the bound is exercised in milliseconds.
func newNotReadyConv(t *testing.T, harness, screenText string, budget time.Duration) *Conversation {
	t.Helper()
	scr := screen.New(120, 40)
	if screenText != "" {
		// The emulator needs CRLF: a bare LF moves down without returning to
		// column 0, so each line would start where the previous ended and the
		// box would render as wrapped fragments.
		if _, err := scr.Write([]byte(strings.ReplaceAll(screenText, "\n", "\r\n"))); err != nil {
			t.Fatalf("screen.Write: %v", err)
		}
	}
	return &Conversation{
		opts:         Options{Harness: harness, readyTimeout: budget},
		screen:       scr,
		inputStateCh: make(chan struct{}, 1),
		closed:       make(chan struct{}),
	}
}

// A screen that never becomes ready must fail on the readiness budget with
// ErrNotReady — NOT run to the caller's deadline. The context here is an order
// of magnitude longer than the budget, standing in for the run budget an
// unattended orchestrator passes; if the bound regresses, this test takes the
// long path and the elapsed-time assertion catches it.
func TestWaitReadyForSend_BoundedByOwnBudget(t *testing.T) {
	const budget = 150 * time.Millisecond
	c := newNotReadyConv(t, chatClaudeCode, trustDialogScreen, budget)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	err := c.waitReadyForSend(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("waitReadyForSend = %v, want ErrNotReady", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("failed on the caller's deadline; the wait must bound itself")
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %v with a %v budget — the bound is not being applied", elapsed, budget)
	}
}

// The error has to be readable from one log line: the harness, how long it
// waited, the recognized cause, and enough of the screen to recognize the
// dialog. A bare "timeout" is what this replaces.
func TestNotReadyError_NamesCauseAndScreen(t *testing.T) {
	c := newNotReadyConv(t, chatClaudeCode, trustDialogScreen, 50*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.waitReadyForSend(ctx)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("waitReadyForSend = %v, want ErrNotReady", err)
	}

	msg := err.Error()
	for _, want := range []string{
		chatClaudeCode, // which harness
		"trust_prompt", // the recognized cause
		"InputPolicy",  // the remedy
		"Do you trust", // proof the screen rode along
		"last screen:", // and that it is labelled as such
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got: %s", want, msg)
		}
	}
}

// A blank screen means the harness never painted at all — a different cause, and
// worth naming separately, because it points at a failed launch rather than at a
// dialog to configure.
func TestNotReadyCause_BlankScreen(t *testing.T) {
	if got := notReadyCause(chatClaudeCode, "   \n  \n"); !strings.Contains(got, "failed to start") {
		t.Errorf("blank screen cause = %q, want it to name a failed start", got)
	}
}

// An onboarding wall reached through the budget (rather than the 2s auth dwell)
// still names itself. The dwell normally wins; this pins the classifier so the
// message never degrades to an unexplained timeout if it does not.
func TestNotReadyCause_Onboarding(t *testing.T) {
	if got := notReadyCause(chatClaudeCode, "Select login method\n  1. Subscription"); !strings.Contains(got, "onboarding") {
		t.Errorf("onboarding cause = %q, want it to name onboarding/sign-in", got)
	}
}

// The bound must never fail a conversation that did become ready. A screen that
// turns into a composer just before the budget expires has to send through —
// whichever internal path observes it first.
func TestWaitReadyForSend_ReadyJustBeforeDeadlineStillSends(t *testing.T) {
	c := newNotReadyConv(t, chatClaudeCode, "starting up…", 2*time.Second)

	go func() {
		time.Sleep(200 * time.Millisecond)
		_, _ = c.screen.Write([]byte("\r\nClaude Code\r\n❯ \r\n"))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.waitReadyForSend(ctx); err != nil {
		t.Fatalf("waitReadyForSend on a screen that became ready = %v, want nil", err)
	}
}

// A screen that is already ready must not pay for, or be failed by, the bound —
// including the degenerate case of a budget that has effectively no time in it.
// The fast path returns before the timer is ever armed.
func TestWaitReadyForSend_AlreadyReadyIgnoresBudget(t *testing.T) {
	c := newNotReadyConv(t, chatClaudeCode, "Claude Code\n❯ \n", time.Nanosecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.waitReadyForSend(ctx); err != nil {
		t.Fatalf("waitReadyForSend on a ready composer = %v, want nil", err)
	}
}

// screenTail is what makes the error one log line: last non-blank lines only,
// joined, length-capped, and never cut mid-rune (these screens are box-drawing).
func TestScreenTail(t *testing.T) {
	t.Run("keeps the last non-blank lines in order", func(t *testing.T) {
		got := screenTail("alpha\n\nbeta\n\n\ngamma\n\n", 2)
		if got != "beta | gamma" {
			t.Errorf("screenTail = %q, want %q", got, "beta | gamma")
		}
	})

	t.Run("caps length and stays valid UTF-8", func(t *testing.T) {
		got := screenTail(strings.Repeat("─", 5000), 8)
		if len(got) > 420 {
			t.Errorf("screenTail len = %d, want capped", len(got))
		}
		if !utf8ValidString(got) {
			t.Errorf("screenTail produced invalid UTF-8: %q", got)
		}
	})

	t.Run("empty screen yields empty tail", func(t *testing.T) {
		if got := screenTail("  \n\n ", 8); got != "" {
			t.Errorf("screenTail on blank = %q, want empty", got)
		}
	})
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
