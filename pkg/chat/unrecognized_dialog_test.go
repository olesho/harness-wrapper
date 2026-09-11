package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// selectorTrustFrame is claude 2.1.251's UNNUMBERED folder-trust dialog
// (captured live, tmux, 2026-08-29). CRLF because these frames are written
// straight into the emulator, which — like the raw-mode PTY the real thing
// paints through — needs the CR to return each line to column 0. The label
// column is what the selector parser keys on, so a staircased frame would not
// model the real one.
const selectorTrustFrame = "Accessing workspace:\r\n" +
	"/private/tmp/trustrepo\r\n" +
	"Quick safety check: Is this a project you created or one you trust? …\r\n" +
	"Claude Code'll be able to read, edit, and execute files here.\r\n" +
	"Security guide\r\n" +
	" ❯ No, exit\r\n" +
	"   Yes, I trust this folder\r\n" +
	"Enter to confirm · Esc to cancel\r\n"

// selectorTrustFrameMoved is the SAME dialog after a Down: the "❯" has moved to
// "Yes, I trust this folder". A real claude repaints like this the moment it
// processes the arrow, and the wrapper withholds the confirming CR until it
// sees the marker on the target label — pressing Enter on an unconfirmed row is
// how "No, exit" gets selected.
const selectorTrustFrameMoved = "Accessing workspace:\r\n" +
	"/private/tmp/trustrepo\r\n" +
	"Quick safety check: Is this a project you created or one you trust? …\r\n" +
	"Claude Code'll be able to read, edit, and execute files here.\r\n" +
	"Security guide\r\n" +
	"   No, exit\r\n" +
	" ❯ Yes, I trust this folder\r\n" +
	"Enter to confirm · Esc to cancel\r\n"

// unparseableTrustFrame keeps the anchor and a choice-shaped "❯" row but gives
// it no sibling, so no option set can be built: DetectUnparseable, the state
// that used to be reported as "no dialog at all".
const unparseableTrustFrame = "Quick safety check: Is this a project you created or one you trust? …\r\n" +
	"Security guide\r\n" +
	" ❯ No, exit\r\n" +
	"Enter to confirm · Esc to cancel\r\n"

func screenWith(t *testing.T, text string) *screen.Screen {
	t.Helper()
	scr := screen.New(160, 60)
	if _, err := scr.Write([]byte(text)); err != nil {
		t.Fatalf("screen.Write: %v", err)
	}
	return scr
}

func convOnScreen(scr *screen.Screen) *Conversation {
	return &Conversation{
		opts:         Options{Harness: chatClaudeCode},
		screen:       scr,
		inputStateCh: make(chan struct{}, 1),
		closed:       make(chan struct{}),
	}
}

// A dialog whose choices cannot be parsed never clears on its own and can never
// be answered, so waitReadyForSend must fail with a NAMED cause shortly after
// the dwell — not wait out the send deadline, and above all not type the prompt
// into the menu.
func TestWaitReadyForSend_UnrecognizedDialogShortCircuits(t *testing.T) {
	c := convOnScreen(screenWith(t, unparseableTrustFrame))
	if got := claudeDialogState(chatClaudeCode, c.screen.Snapshot().Text); got != claudecode.DetectUnparseable {
		t.Fatalf("precondition: dialog state = %v, want DetectUnparseable — the fixture proves nothing otherwise", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), unrecognizedDialogStabilizeGap+3*time.Second)
	defer cancel()
	start := time.Now()
	err := c.waitReadyForSend(ctx)
	if !errors.Is(err, ErrUnrecognizedDialog) {
		t.Fatalf("waitReadyForSend = %v, want ErrUnrecognizedDialog", err)
	}
	if elapsed := time.Since(start); elapsed < unrecognizedDialogStabilizeGap {
		t.Errorf("fired after %v, before the %v dwell — a half-painted frame would trip it",
			elapsed, unrecognizedDialogStabilizeGap)
	}
	if elapsed := time.Since(start); elapsed > unrecognizedDialogStabilizeGap+2*time.Second {
		t.Errorf("short-circuit took %v, expected ~%v (not the full deadline)", elapsed, unrecognizedDialogStabilizeGap)
	}
}

// A dialog that DOES parse is answerable, so the existing behaviour stands: not
// ready, and we wait for it to be answered rather than failing the send.
func TestWaitReadyForSend_ParsedDialogKeepsWaiting(t *testing.T) {
	c := convOnScreen(screenWith(t, selectorTrustFrame))
	if got := claudeDialogState(chatClaudeCode, c.screen.Snapshot().Text); got != claudecode.DetectOK {
		t.Fatalf("precondition: dialog state = %v, want DetectOK", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), unrecognizedDialogStabilizeGap+time.Second)
	defer cancel()
	err := c.waitReadyForSend(ctx)
	if errors.Is(err, ErrUnrecognizedDialog) {
		t.Fatal("waitReadyForSend failed a parseable dialog as unrecognized; it must wait for the answer")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitReadyForSend = %v, want the context deadline (still waiting)", err)
	}
}

// TestIntegration_SelectorTrustDialogAnsweredWithArrowThenEnter is the load-
// bearing test of the whole change: a REAL fake-harness process paints the
// unnumbered 2.1.251 trust dialog over a REAL PTY, and the only bytes the
// wrapper may send to answer it under `trust_prompt: allow` are Down + CR.
//
// The default highlight is "No, exit". A bare CR — what a "simplification" of
// the positional keys would produce — selects it and quits claude at startup,
// silently. The fake's first wait step matches ONLY "ESC [ B", so a bare CR
// hangs the scenario to the test deadline; and because that step CAPTURES
// everything received before the match, the echoed "[…]" is empty exactly when
// those were the first bytes written, with nothing typed into the dialog
// beforehand.
//
// The arrow and the CR are awaited SEPARATELY, with a repaint between them, and
// that is the contract this test pins rather than an implementation detail. The
// wrapper writes the navigation, then confirms on screen that the marker landed
// on the target LABEL, and only then writes the CR (see answerAndConfirm in
// input.go). A fake that painted the dialog once and never repainted would
// never show the marker move, so the CR would correctly never be sent — which
// is the point: an Enter pressed on an unconfirmed row selects "No, exit".
// What must NOT be split is "ESC [ B" itself: a lone Esc cancels the dialog, so
// the regex below matches the whole sequence and the wrapper writes it as one.
func TestIntegration_SelectorTrustDialogAnsweredWithArrowThenEnter(t *testing.T) {
	script := fakeharness.New("claude-code").Idle().Build()
	script.Steps = append(
		script.Steps,
		// The dialog, painted verbatim (the builder's CRLF conversion happens
		// in the fake, so this uses plain newlines).
		fakeharness.Step{Frame: &fakeharness.Frame{DelayMs: 40, Screen: strings.ReplaceAll(selectorTrustFrame, "\r\n", "\n")}},
		// Down, in one write, and nothing before it.
		fakeharness.Step{WaitInput: &fakeharness.WaitInput{UntilRegex: `\x1b\[B`, Capture: true, Label: "trust-arrow"}},
		// The dialog acknowledges the arrow by repainting with the marker on
		// "Yes, I trust this folder". Only now may the wrapper confirm.
		fakeharness.Step{Frame: &fakeharness.Frame{DelayMs: 20, Screen: strings.ReplaceAll(selectorTrustFrameMoved, "\r\n", "\n")}},
		// The confirming CR. No Capture: the prompt stashed by the arrow step
		// is what the echo below reports, so stray bytes stay detectable.
		fakeharness.Step{WaitInput: &fakeharness.WaitInput{UntilRegex: `\r`, Label: "trust-enter"}},
		fakeharness.Step{Frame: &fakeharness.Frame{DelayMs: 20, Screen: "Claude Code\n\nTRUSTED [{{prompt}}]\n\n❯ \n", Echo: true}},
		fakeharness.Step{Hold: &fakeharness.Hold{}},
	)

	conv := openFake(t, script, func(o *Options) {
		o.InputPolicy = &InputPolicy{ByKind: map[string]Disposition{
			"trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
		}}
	})

	deadline := time.After(15 * time.Second)
	for {
		text := conv.screen.Snapshot().Text
		if strings.Contains(text, "TRUSTED []") {
			return // answered with exactly ESC [ B + CR, nothing before it
		}
		if strings.Contains(text, "TRUSTED [") {
			t.Fatalf("the dialog was answered, but stray bytes preceded the arrow:\n%s", text)
		}
		select {
		case <-deadline:
			t.Fatalf("the trust dialog was never answered with Down+CR within the deadline; "+
				"last screen:\n%s", text)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
