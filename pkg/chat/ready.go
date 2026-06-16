package chat

import (
	"context"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

func (c *Conversation) waitReadyForSend(ctx context.Context) error {
	// A prompt awaiting an external answer can never reach the ready state on
	// its own; fail fast so the caller answers it first. (A prompt being
	// auto-answered by a policy/handler is not "surfaced" and we keep waiting
	// for it to clear.)
	if c.inputAwaitingClient() {
		return ErrInputPending
	}
	if !requiresPromptReadiness(c.opts.Harness) {
		return nil
	}
	if readyForInput(c.opts.Harness, c.screen.Snapshot().Text) {
		return nil
	}

	notifyCh, unsubscribe := c.screen.Subscribe()
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return ErrClosed
		case <-c.inputStateCh:
			if c.inputAwaitingClient() {
				return ErrInputPending
			}
			if readyForInput(c.opts.Harness, c.screen.Snapshot().Text) {
				return nil
			}
		case _, ok := <-notifyCh:
			if !ok {
				return ErrClosed
			}
			if c.inputAwaitingClient() {
				return ErrInputPending
			}
			if readyForInput(c.opts.Harness, c.screen.Snapshot().Text) {
				return nil
			}
		}
	}
}

func requiresPromptReadiness(harness string) bool {
	switch harness {
	case "claude-code":
		return true
	default:
		return false
	}
}

func readyForInput(harness, text string) bool {
	switch harness {
	case "claude-code":
		// A blocking dialog (folder-trust, bypass acceptance) renders its own
		// "❯" selector and looks ready. Treat the dialog as not-ready so Send
		// waits for it to clear instead of typing the message into the menu.
		if _, blocking := claudecode.DetectInput(text); blocking {
			return false
		}
		// The "❯" prompt box means Claude is accepting input. We deliberately
		// do NOT also require the "Claude Code" welcome banner: it is painted
		// only on the fresh startup screen and scrolls off once a turn's prompt
		// is tall enough to fill the viewport. Requiring it wedged every
		// subsequent Send in waitReadyForSend until the caller timed out
		// (a tall step prompt reproduces this on the very second turn).
		// In-flight turns are already gated by currentTurn before
		// waitReadyForSend is consulted, so the prompt box alone suffices.
		return strings.Contains(text, "❯")
	default:
		return true
	}
}

func submitKeyForHarness(harness, screenText string) []byte {
	switch harness {
	case "claude-code":
		if strings.Contains(screenText, "bypass permissions") || strings.Contains(screenText, "ctrl+g to edit in Vim") {
			// Claude Code enables enhanced keyboard handling in its TUI and
			// does not submit the input box when a synthetic PTY writer sends
			// plain CR/LF. CSI 13 u is the unmodified Enter key in that mode.
			return []byte("\x1b[13u")
		}
		return []byte("\n")
	default:
		return []byte("\n")
	}
}
