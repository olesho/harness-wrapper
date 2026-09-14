//go:build screenbench

package main

import (
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

// awaitComposerEcho blocks until the composer echoes needle on the RENDERED
// screen, the bound elapses, or ctx is cancelled. It reports whether the echo
// was seen. preWrite is the rendered screen from just before the body was
// written.
//
// It mirrors pkg/chat's twin rule for rule. The needle is looked for on the
// rendered screen, not in the raw byte buffer: Claude sometimes paints a
// composer echo with cursor-column jumps instead of space bytes
// ("what\x1b[8Gis…"), so the text is contiguous on screen but never in the
// stream, and a raw search waited out the whole bound on every such send.
// Past half the bound, ANY screen change since preWrite counts, which is what
// covers a composer that transforms the echo.
//
// The return value is advisory. Callers write the submit key either way: a
// missed echo degrades to the pre-echo timing, it must never skip the submit
// and never write it twice.
func (d *scriptDriver) awaitComposerEcho(ctx context.Context, needle, preWrite string) bool {
	bound := d.echoGap
	if bound <= 0 {
		bound = submitEchoGap
	}
	deadline := time.NewTimer(bound)
	defer deadline.Stop()
	half := time.NewTimer(bound / 2)
	defer half.Stop()
	halfDone := false
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()

	for {
		cur := d.screenText()
		if needle != "" && strings.Contains(cur, needle) {
			return true
		}
		if (halfDone || needle == "") && cur != preWrite {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-half.C:
			halfDone = true
		case <-tick.C:
		}
	}
}
