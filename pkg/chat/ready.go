package chat

import (
	"context"
	"strings"
)

func (c *Conversation) waitReadyForSend(ctx context.Context) error {
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
		case _, ok := <-notifyCh:
			if !ok {
				return ErrClosed
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
		return strings.Contains(text, "Claude Code") && strings.Contains(text, "❯")
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
