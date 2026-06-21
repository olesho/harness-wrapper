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

	// Subscribe BEFORE the readiness check to avoid a lost-wakeup race: if the
	// prompt-ready frame lands between the check and Subscribe and the harness
	// then paints nothing further (it can sit idle at a static prompt — no
	// spinner, no cursor blink), the notification is missed and we block until
	// ctx. Subscribing first guarantees any later frame wakes us; the check
	// below still returns immediately for a prompt that was already ready.
	notifyCh, unsubscribe := c.screen.Subscribe()
	defer unsubscribe()

	if readyForInput(c.opts.Harness, c.screen.Snapshot().Text) {
		return nil
	}

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
		// Claude Code enables enhanced keyboard handling in its TUI and does
		// not submit the input box when a synthetic PTY writer sends plain
		// CR/LF — it only inserts a newline and the turn never runs. CSI 13 u
		// is the unmodified Enter key in that mode. Recent versions (≥2.1.x)
		// turn enhanced mode on unconditionally at startup — the auto-mode
		// composer shows neither "bypass permissions" nor "ctrl+g to edit in
		// Vim" — so we always send the enhanced Enter, mirroring codex below.
		return []byte("\x1b[13u")
	case "codex":
		// codex 0.141.0 turns on the enhanced (kitty) keyboard protocol at startup,
		// so a plain CR/LF from a synthetic PTY writer is NOT treated as submit — it
		// only inserts a newline in the composer and the turn never runs. CSI 13 u is
		// the unmodified Enter key in that mode (same as claude-code's enhanced TUI).
		// 0.140.0 accepted "\n", but enhanced mode is unconditional now.
		return []byte("\x1b[13u")
	default:
		return []byte("\n")
	}
}
