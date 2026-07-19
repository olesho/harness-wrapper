package chat

import (
	"context"
	"regexp"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/pi"
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

// chatClaudeCode is the adapter-style name for the Claude Code harness.
const chatClaudeCode = "claude-code"

func requiresPromptReadiness(harness string) bool {
	switch harness {
	case chatClaudeCode, "codex", "pi":
		return true
	default:
		return false
	}
}

func readyForInput(harness, text string) bool {
	switch harness {
	case chatClaudeCode:
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
	case "pi":
		// pi has a noisy, network-touching startup (model resolution, optional
		// fd/ripgrep download, an "Update Available" banner) during which the
		// composer is painted but not yet listening. Gate Send until pi's idle
		// status line is up and no turn is mid-flight, so the prompt + CR aren't
		// typed into a composer that drops them.
		return pi.PromptReady(text)
	default:
		return true
	}
}

// Logged-out / re-authentication banners, per harness. A harness whose CLI login
// has expired or was never established produces NO assistant output for the turn.
// The anchors are grounded in real observed CLI output, not invented:
//   - claude-code: "Not logged in · Please run /login" (printed then exit); the
//     "run /login" family of re-auth banners.
//   - codex:       the turn fails with "401 Unauthorized: missing bearer or basic
//     authentication" on screen; a logged-out TUI / `codex login status` say "Not
//     logged in"; codex's own remediation is "run `codex login`".
//
// These are matched ONLY at a turn's terminal point, and only once the turn has
// already ended in failure (see Conversation.handleTurnsEvent) — they EXPLAIN a
// failed turn, they never complete one. That gating is what keeps a genuine reply
// mentioning logins, or a benign "your login expires in N days" WARNING on a
// still-valid session, from being scanned and mislabeled.
var (
	claudeAuthRE = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\brun /login\b`),
		regexp.MustCompile(`(?i)\bnot logged in\b`),
	}
	codexAuthRE = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b401 unauthorized\b`),
		regexp.MustCompile(`(?i)missing bearer or basic authentication`),
		regexp.MustCompile(`(?i)\bnot logged in\b`),
		regexp.MustCompile(`(?i)\bcodex(?: mcp)? login\b`),
	}
)

func anyMatch(res []*regexp.Regexp, text string) bool {
	for _, re := range res {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// authRequired reports whether the rendered screen shows a harness login-expiry /
// logged-out banner. Callers MUST gate this on a turn that has already ended in
// failure — it is a failure EXPLANATION, not a turn-completion signal. Returns
// false for any harness without a known banner set.
func authRequired(harness, text string) bool {
	switch harness {
	case chatClaudeCode:
		return anyMatch(claudeAuthRE, text)
	case "codex":
		return anyMatch(codexAuthRE, text)
	default:
		return false
	}
}

func submitKeyForHarness(harness, screenText string) []byte {
	switch harness {
	case chatClaudeCode:
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
	case "pi":
		// pi's composer submits on a carriage return (the actual Enter byte); a bare
		// "\n" (line feed) is NOT treated as submit — it leaves the typed prompt sitting
		// in the composer unsent (verified live against pi 0.76.0: the prompt rendered
		// in the input box but the turn never ran). pi does NOT enable the kitty keyboard
		// protocol (only bracketed-paste / synchronized-output), so the enhanced CSI 13u
		// that claude-code/codex need is unnecessary — a plain CR submits.
		return []byte("\r")
	default:
		return []byte("\n")
	}
}
