//go:build screenbench

package main

import (
	"bytes"
	"context"
	"strings"
	"time"
)

// The recorder's half of the submit contract. Mirrors pkg/chat/submit.go
// (Conversation.writeMessageAndSubmit / awaitComposerEcho) — the inward
// contract — kept in sync for the same reason submitKeyForHarness is: a
// re-baked corpus must be recorded the way the wrapper actually drives a turn.
//
// The defect being mirrored: Claude Code treats a fast burst of PTY bytes as a
// PASTE, and a submit key riding in the same burst is consumed as pasted
// CONTENT rather than acted on as a keypress. The prompt then sits in the
// composer unsent, the turn never starts, and — in the recorder — the script's
// wait_for falls through on the idle timeout and the bake captures a session
// with no turn in it. That is exactly how test/scripts/claude/tool-call.json
// stopped producing a usable recording.
//
// The direction of the check matters: wait for the composer to ECHO the text,
// THEN write the submit key once. Submitting first and retrying would risk
// double-submitting a prompt that did land, which is the worse failure.
const (
	// submitEchoGap bounds the wait for the composer echo before the submit key
	// is written anyway. Degrading to the old single-burst timing is the right
	// failure mode: it can delay a send, never drop or duplicate one.
	// Same value as pkg/chat.submitEchoGap.
	submitEchoGap = 1500 * time.Millisecond

	// echoNeedleLen is how much of the text's first line to look for in the
	// stream. Short enough to survive wrapping at any recorded width.
	// Same value as pkg/chat.echoNeedleLen.
	echoNeedleLen = 24
)

// echoNeedle reduces a Send body to the substring to look for in the PTY
// stream: the first line, trimmed, capped at echoNeedleLen bytes. Returns ""
// when there is nothing matchable, which callers read as "do not wait".
func echoNeedle(text string) string {
	needle := text
	if i := strings.IndexByte(needle, '\n'); i >= 0 {
		needle = needle[:i]
	}
	needle = strings.TrimSpace(needle)
	if len(needle) > echoNeedleLen {
		needle = needle[:echoNeedleLen]
	}
	return needle
}

// awaitComposerEcho blocks until the harness echoes needle back on the PTY, the
// bound elapses, or ctx is cancelled. It reports whether the echo was seen.
//
// Unlike pkg/chat's twin it polls the RAW byte buffer rather than a rendered
// screen — scriptDriver has no emulator — which is the same buffer wait_for
// matches against (see the schema comment in script.go). A composer echo is
// plain text interleaved with SGR escapes, so a needle taken from the start of
// the first line matches as long as it does not straddle a style change; the
// recorded prompts are plain ASCII well past echoNeedleLen for that reason.
//
// The return value is advisory. Callers write the submit key either way: a
// missed echo degrades to the pre-echo timing, it must never skip the submit
// and never write it twice.
func (d *scriptDriver) awaitComposerEcho(ctx context.Context, needle string) bool {
	if needle == "" {
		return false
	}
	bound := d.echoGap
	if bound <= 0 {
		bound = submitEchoGap
	}
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()

	for {
		if d.bufContains(needle) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

// bufContains reports whether the rolling output buffer currently holds needle.
func (d *scriptDriver) bufContains(needle string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return bytes.Contains(d.buf, []byte(needle))
}
