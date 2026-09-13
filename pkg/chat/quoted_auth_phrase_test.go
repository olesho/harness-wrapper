package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// readyComposerQuoting is a settled claude screen whose last reply quotes a
// sign-in UI phrase in the middle of ordinary prose, above a live composer.
func readyComposerQuoting(phrase string) string {
	return "⏺ When the browser handoff fails claude shows \"" + phrase + "\" and waits;\r\n" +
		"  nothing else on that screen needs an answer from you.\r\n" +
		"\r\n" +
		"╭──────────────────────────────────────────────────────────────╮\r\n" +
		"│ ❯                                                            │\r\n" +
		"╰──────────────────────────────────────────────────────────────╯\r\n" +
		"  ? for shortcuts\r\n"
}

// codexComposerQuoting is the codex twin: a reply that quotes a sign-in phrase
// mid-sentence, above codex's idle composer.
func codexComposerQuoting(phrase string) string {
	return "• If the login lapsed codex shows \"" + phrase + "\" first;\r\n" +
		"  otherwise it opens straight at the prompt.\r\n" +
		"\r\n" +
		"› Find and fix a bug in @filename\r\n" +
		"\r\n" +
		"  ? for shortcuts                                   100% context left\r\n"
}

// assertSendableDespiteQuote paints frame and requires waitReadyForSend to
// report the conversation ready rather than gate it as an auth wall.
func assertSendableDespiteQuote(t *testing.T, harness, frame, phrase string) {
	t.Helper()
	scr := screen.New(120, 40)
	if _, err := scr.Write([]byte("\x1b[2J\x1b[H" + frame)); err != nil {
		t.Fatal(err)
	}
	c := &Conversation{
		opts:         Options{Harness: harness},
		screen:       scr,
		eventCh:      make(chan ConversationEvent, 4),
		closed:       make(chan struct{}),
		inputStateCh: make(chan struct{}, 1),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.waitReadyForSend(ctx); err != nil {
		if errors.Is(err, ErrAuthRequired) {
			t.Fatalf("a reply quoting %q gated Send as an auth wall", phrase)
		}
		t.Fatalf("waitReadyForSend: %v", err)
	}
}

// TestReadyForSend_QuotedSignInPhraseDoesNotGateSend: an onboarding WALL is
// checked before readiness and fails Send at once with ErrAuthRequired, so its
// anchors must only ever match the wall's own UI lines. A reply that merely
// QUOTES one of those phrases — here mid-sentence, above a ready composer —
// must leave the conversation sendable. The anchors used to be whole-screen
// substring matches, so each of these gated Send.
func TestReadyForSend_QuotedSignInPhraseDoesNotGateSend(t *testing.T) {
	for _, phrase := range []string{
		"Paste code here if prompted",
		"Use the url below to sign in",
		"Select login method",
		"Choose the text style that looks best with your terminal",
	} {
		t.Run(phrase, func(t *testing.T) {
			assertSendableDespiteQuote(t, chatClaudeCode, readyComposerQuoting(phrase), phrase)
		})
	}
}

// TestReadyForSend_QuotedCodexSignInPhraseDoesNotGateSend is the codex half of
// the same contract: codex's wall anchors are checked before readiness too.
func TestReadyForSend_QuotedCodexSignInPhraseDoesNotGateSend(t *testing.T) {
	for _, phrase := range []string{
		"Sign in with ChatGPT",
		"Finish signing in via your browser",
	} {
		t.Run(phrase, func(t *testing.T) {
			assertSendableDespiteQuote(t, "codex", codexComposerQuoting(phrase), phrase)
		})
	}
}
