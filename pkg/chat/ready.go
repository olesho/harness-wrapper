package chat

import (
	"context"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
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
	case "claude-code", "codex":
		return true
	default:
		return false
	}
}

func readyForInput(harness, text string) bool {
	switch harness {
	case "claude-code":
		// A blocking dialog (folder-trust, bypass acceptance) renders its own
		// "❯" selector and the "Claude Code" header, which would otherwise
		// look ready. Treat the dialog as not-ready so Send waits for it to
		// clear instead of typing the message into the menu.
		if _, blocking := claudecode.DetectInput(text); blocking {
			return false
		}
		return strings.Contains(text, "Claude Code") && strings.Contains(text, "❯")
	case "codex":
		// A blocking startup interstitial (update notice, model migration)
		// renders its own "›" highlight and looks ready. Treat it as not-ready
		// so Send waits for the auto-dismiss to clear it instead of typing the
		// message into the menu. Once cleared, the idle composer's "›" prompt
		// (PromptReady) means Codex is accepting input. In-flight turns are
		// gated by currentTurn before waitReadyForSend is consulted, so the
		// composer prompt alone suffices here.
		if _, blocking := codex.DetectInput(text); blocking {
			return false
		}
		return codex.PromptReady(text)
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
