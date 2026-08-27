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
//
// This file now guards a SECOND, independent defect of the same write. Where the
// one above loses the SUBMIT KEY (the turn never starts), the other loses the
// TEXT: a >2KB prompt written as one raw burst reaches claude-code truncated to
// its last read chunk, so the turn starts and completes normally while the model
// answers only the tail of a role prompt. Measured 2026-08-27 on claude-code
// 2.1.247: truncation always began at byte offset 2044 of 2608, in 5 of 10 runs
// here and 8 of 10 in the original report. The fix is to stop TYPING a large
// payload and start declaring it as a PASTE — see pasteWrapForHarness in
// ready.go for the framing, the corpus evidence, and the 10/10 measurement.
const (
	// submitEchoGap bounds the wait for the composer echo before the submit key
	// is written anyway. Degrading to the old single-burst timing is the right
	// failure mode: it can delay a send, never drop or duplicate one.
	submitEchoGap = 1500 * time.Millisecond

	// echoNeedleLen is how much of the text's first line to look for on screen.
	// Short enough to survive wrapping at any supported width.
	echoNeedleLen = 24

	// pasteThreshold is the text size at or above which the write is framed as a
	// bracketed paste. Measured loss began at 2044 of 2608 bytes on claude-code
	// 2.1.247 — one read chunk — so 1024 leaves margin below the observed cliff
	// while staying far above any slash command or interactive answer.
	//
	// A threshold rather than framing everything: framing changes how the
	// composer RENDERS short input (claude-code collapses a paste into a
	// "[Pasted text #1 +N lines]" placeholder), and small prompts have never
	// been observed to truncate. Keep the blast radius on the payload that is
	// actually broken.
	pasteThreshold = 1024
)

// shouldPaste reports whether text should go out framed as a bracketed paste for
// this harness, returning the framing to use.
//
// Three conditions, all required: the harness has a MEASURED framing
// (pasteWrapForHarness), the text is big enough to be at risk, and it is not a
// slash command. The slash guard preserves slash-command semantics — "/compact"
// or "/model" typed through Send by a chatd client must still open the command
// palette, which a PASTE does not. A >=1KB slash command is not a real shape;
// the residual hazard is noted here rather than engineered for.
func shouldPaste(harness, text string) (prefix, suffix []byte, framed bool) {
	if len(text) < pasteThreshold {
		return nil, nil, false
	}
	if strings.HasPrefix(strings.TrimSpace(text), "/") {
		return nil, nil, false
	}
	prefix, suffix = pasteWrapForHarness(harness)
	if prefix == nil {
		return nil, nil, false
	}
	return prefix, suffix, true
}

// framePaste builds the ONE write that carries a framed payload: the opening
// marker, the text, the closing marker.
//
// Any bracketed-paste marker already INSIDE text is stripped first. A prompt
// that quotes terminal escapes would otherwise terminate the paste early and
// dump its remainder back onto the typed path — the exact failure this change
// exists to remove, re-entered through the payload. Cheap, and it closes an
// injection-shaped foot-gun.
func framePaste(prefix, suffix []byte, text string) []byte {
	text = strings.ReplaceAll(text, pasteStartCSI200, "")
	text = strings.ReplaceAll(text, pasteEndCSI201, "")
	buf := make([]byte, 0, len(prefix)+len(text)+len(suffix))
	buf = append(buf, prefix...)
	buf = append(buf, text...)
	return append(buf, suffix...)
}

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
	prefix, suffix, framed := shouldPaste(c.opts.Harness, text)
	if framed {
		// The markers and the text go out as ONE write; the submit key stays
		// its own, LATER write. Putting the submit key inside the paste would
		// have it consumed as pasted CONTENT — precisely the swallowed-prompt
		// bug the split above exists to fix.
		if err := c.write(framePaste(prefix, suffix, text)); err != nil {
			return err
		}
	} else if err := c.write([]byte(text)); err != nil {
		return err
	}
	if err := c.awaitComposerEcho(ctx, text, preWriteScreen, framed); err != nil {
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
// does change — and when framed is set the caller has DECLARED that case, so the
// needle is dropped up front rather than waited out.
//
// On the local deadline it returns nil, degrading to the old single-burst
// timing: the submit is written regardless, so this can delay a send but never
// hang or drop it. A cancelled ctx is different — the whole run is over — so it
// returns that error instead of degrading.
func (c *Conversation) awaitComposerEcho(ctx context.Context, text, preWriteScreen string, framed bool) error {
	needle := text
	if i := strings.IndexByte(needle, '\n'); i >= 0 {
		needle = needle[:i]
	}
	needle = strings.TrimSpace(needle)
	if len(needle) > echoNeedleLen {
		needle = needle[:echoNeedleLen]
	}
	// A FRAMED write is rendered as a placeholder ("[Pasted text #1 +42 lines]"),
	// never as the text, so the primary needle can never match and every send
	// would pay the full bound/2 before the fallback arms. Empty needle is the
	// signal the loop below already reads as "any screen change counts" — the
	// same branch a collapsed paste placeholder was always meant to land in.
	if framed {
		needle = ""
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
