package chat

import (
	"context"
	"strings"
	"time"
)

// Submitting used to be one write: the prompt text and the submit key
// concatenated. That works until the harness decides the burst is a PASTE.
// Claude Code collapses a large multi-line arrival into a single composer entry
// ("[Pasted text #1 +157 lines]"), and while it assembles that, a submit key
// riding in the same write is taken as pasted CONTENT rather than acted on as a
// keypress. The prompt then sits in the composer unsent: no turn starts, and the
// run "completes" with nothing in it — zero assistant output, zero tokens.
//
// Reproduced in a Linux container running loom's local-mode stack: the agent
// hung at launch on every run, while the identical code on macOS submitted
// correctly every time. The two writes race and the environment decides the
// winner, which is why this cannot be left to timing.
//
// Ported from meta-harness (Conversation.writeMessageAndSubmit /
// awaitComposerEcho); the two implementations are kept in step. Note the
// direction of the check: wait for the composer to ECHO the text, THEN press
// Enter once. Submitting first and retrying — the obvious alternative — risks
// double-submitting a prompt that did land, which is worse than the bug.
const (
	// submitEchoGap bounds the wait for the composer echo before the submit key
	// is written anyway. Degrading to the old single-burst timing is the right
	// failure mode: it can delay a send, never drop or duplicate one.
	submitEchoGap = 1500 * time.Millisecond

	// echoNeedleLen is how much of the text's first line to look for on screen.
	// Short enough to survive wrapping at any supported width.
	echoNeedleLen = 24
)

// writeMessageAndSubmit writes text, waits for the composer to show it, and only
// then writes the submit key.
//
// Harnesses that do not gate on prompt readiness keep the single combined
// write: they have no composer to echo into, so there is nothing to wait for.
//
// Writes go through c.write, not c.sess.WriteStdin directly, so the interactive
// answer path (input.go) — which the same paste-collapse hazard reaches, and
// which is tested without a live session — shares this exact code. In
// production c.writeStdin is nil and c.write is sess.WriteStdin.
func (c *Conversation) writeMessageAndSubmit(ctx context.Context, text, preWriteScreen string, submitKey []byte) error {
	if !requiresPromptReadiness(c.opts.Harness) {
		return c.write(append([]byte(text), submitKey...))
	}
	if err := c.write([]byte(text)); err != nil {
		return err
	}
	if err := c.awaitComposerEcho(ctx, text, preWriteScreen); err != nil {
		return err
	}
	return c.write(submitKey)
}

// echoBoundDur is how long to wait for the echo.
//
// The submit key must land well inside the idle-completion window: an echo wait
// outliving it would let the swallowed-prompt check judge — and error — the turn
// before the submit was even written. That matters when a caller shrinks idleGap
// (tests) without shrinking the echo bound to match.
func (c *Conversation) echoBoundDur() time.Duration {
	bound := submitEchoGap
	if half := c.idleGapDur() / 2; half < bound {
		bound = half
	}
	return bound
}

// awaitComposerEcho waits until the composer echoes the just-written text.
//
// Primary signal: the screen contains the first line of the text, truncated to
// echoNeedleLen. Fallback: past the halfway mark — or immediately, when the text
// has no matchable first line — ANY screen change since the pre-write snapshot
// counts, which is what covers composers that TRANSFORM the echo. A collapsed
// paste placeholder is exactly that case: the text never appears, but the screen
// does change.
//
// On the local deadline it returns nil, degrading to the old single-burst
// timing: the submit is written regardless, so this can delay a send but never
// hang or drop it. A cancelled ctx is different — the whole run is over — so it
// returns that error instead of degrading.
func (c *Conversation) awaitComposerEcho(ctx context.Context, text, preWriteScreen string) error {
	needle := text
	if i := strings.IndexByte(needle, '\n'); i >= 0 {
		needle = needle[:i]
	}
	needle = strings.TrimSpace(needle)
	if len(needle) > echoNeedleLen {
		needle = needle[:echoNeedleLen]
	}

	bound := c.echoBoundDur()
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	half := time.NewTimer(bound / 2)
	defer half.Stop()
	halfDone := false

	notifyCh, unsubscribe := c.screen.Subscribe()
	defer unsubscribe()

	for {
		cur := c.screen.Snapshot().Text
		if needle != "" && strings.Contains(cur, needle) {
			return nil
		}
		if (halfDone || needle == "") && cur != preWriteScreen {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return nil
		case <-deadline.C:
			return nil
		case <-half.C:
			halfDone = true
		case <-notifyCh:
		}
	}
}
