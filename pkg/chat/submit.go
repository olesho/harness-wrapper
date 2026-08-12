package chat

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Submitting a prompt used to be one write: text and the submit key appended
// together. That works until the harness decides the burst is a PASTE.
//
// Claude Code (and codex) collapse a large multi-line arrival into a single
// composer entry — "[Pasted text #1 +157 lines]" — and while they are
// assembling it, a submit key sitting in the same write is taken as pasted
// CONTENT rather than acted on as a keypress. The prompt then sits in the
// composer, unsent, and the harness looks idle. Because no turn ever starts,
// the run ends "successfully" with an empty reply: zero assistant turns, zero
// tokens, exit 0.
//
// Observed in the field 2026-08-12: a 6304-byte / 157-line agent prompt did
// this on eight consecutive runs under load, while an 81-byte prompt on the
// same binary in the same container worked every time. On an idle machine the
// same large prompt usually submits — the two writes race, and load decides the
// winner. A silent empty turn is the worst possible failure mode for a caller
// that pays per run, so this cannot be left to timing.
//
// The fix is to stop racing: write the prompt, let the composer settle, send
// the submit key as its OWN write, and then CONFIRM the composer let go of it.
const (
	// submitSettle is the pause between the prompt write and the submit key,
	// long enough for a paste-collapsing composer to finish assembling.
	submitSettle = 200 * time.Millisecond

	// submitConfirmWindow is how long to wait for the composer to release the
	// input after a submit key before trying again.
	submitConfirmWindow = 3 * time.Second

	// submitPoll is the screen-sampling interval while confirming.
	submitPoll = 100 * time.Millisecond

	// submitAttempts bounds the retries. A composer that has not accepted the
	// input after this many tries is not going to.
	submitAttempts = 3

	// pastePlaceholder is how a collapsed paste renders in the composer. Its
	// presence after a submit means the input is still sitting there.
	pastePlaceholder = "[Pasted text #"
)

// writeAndConfirmSubmit writes text, then submits it as a separate write, and
// verifies the composer actually released the input.
//
// For an input the harness did not collapse (the common small-prompt case)
// there is nothing to confirm — composerHoldsPaste is false immediately — so
// this costs one settle interval and behaves exactly as before.
func (c *Conversation) writeAndConfirmSubmit(ctx context.Context, text string, submitKey []byte) error {
	if _, err := c.sess.WriteStdin([]byte(text)); err != nil {
		return err
	}
	var lastScreen string
	for attempt := 1; attempt <= submitAttempts; attempt++ {
		if err := sleepCtx(ctx, submitSettle); err != nil {
			return err
		}
		if _, err := c.sess.WriteStdin(submitKey); err != nil {
			return err
		}
		released, screen := c.awaitComposerReleased(ctx, submitConfirmWindow)
		if released {
			return nil
		}
		lastScreen = screen
	}
	return fmt.Errorf(
		"chat: prompt (%d bytes, %d lines) was not submitted after %d attempts — the composer still holds it as a collapsed paste; last screen tail: %q",
		len(text), strings.Count(text, "\n")+1, submitAttempts, screenTail(lastScreen, 200))
}

// awaitComposerReleased polls the CURRENT screen (never an accumulated
// transcript — the placeholder legitimately appears there when the paste first
// renders) until the collapsed-paste entry is gone. Returns the last screen it
// saw so a failure can say what it was looking at.
func (c *Conversation) awaitComposerReleased(ctx context.Context, window time.Duration) (bool, string) {
	deadline := time.Now().Add(window)
	for {
		screen := c.screen.Snapshot().Text
		if !composerHoldsPaste(screen) {
			return true, screen
		}
		if time.Now().After(deadline) {
			return false, screen
		}
		select {
		case <-ctx.Done():
			return false, screen
		case <-c.closed:
			return false, screen
		case <-time.After(submitPoll):
		}
	}
}

// composerHoldsPaste reports whether the screen shows a collapsed-paste entry
// still waiting in the composer.
func composerHoldsPaste(screen string) bool {
	return strings.Contains(screen, pastePlaceholder)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func screenTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
