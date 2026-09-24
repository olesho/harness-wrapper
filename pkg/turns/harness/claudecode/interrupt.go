package claudecode

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// interruptKey is Esc in the kitty keyboard protocol (CSI 27 u), which Claude's
// TUI runs — the reason its Enter is CSI 13 u (pkg/chat.submitKeyForHarness).
// Recorded on 2.1.280 (ADR-007): it stops a turn mid-reply or mid-tool, and
// cancels one before its first token, exactly as a lone 0x1b does. It is used
// over the lone byte because a lone ESC is also the first byte of every escape
// sequence: Claude holds it for its escape timeout before calling it a key
// (~90 ms against ~30 ms, captured), and a write landing inside that window can
// fuse with it into an Alt chord.
var interruptKey = []byte("\x1b[27u")

// Composer editing keys, recorded on 2.1.280 (ADR-007). Ctrl-E moves to the end
// of the cursor's line. Ctrl-K kills to the end of the line, and at the end of
// a line joins the next one. Ctrl-U kills to the start of the line, and on an
// empty line joins it to the one above. None does anything on an empty
// composer. Esc-based clearing ("Esc again to clear") is not used: two Escs on
// an empty composer open Claude's Rewind picker.
const (
	keyEndOfLine = 0x05 // Ctrl-E
	keyKillToEnd = 0x0b // Ctrl-K
	keyKillStart = 0x15 // Ctrl-U
)

// interruptLineRE matches Claude's interrupt marker on its own line — the ⎿
// continuation glyph first, as Claude paints it below the stopped reply or tool
// call. A reply quoting the marker mid-sentence does not match.
var interruptLineRE = regexp.MustCompile(`^[^\S\r\n]*` + regexp.QuoteMeta(interruptMarker))

// echoLineRE matches the first line of a submitted prompt in the conversation:
// the composer glyph at column 0. Replies and tool output are indented, and the
// Rewind picker's selector is too, so neither matches.
var echoLineRE = regexp.MustCompile(`^❯ (.*)$`)

// toolCallRE matches a tool call's bullet text — "Bash(sleep 20)",
// "Read(main.go)", "Web Search(…)" — which is not reply text.
var toolCallRE = regexp.MustCompile(`^[A-Z][A-Za-z]*(?: [A-Z][A-Za-z]*)?\(`)

// placeholderRE matches the example prompt Claude paints, dimmed, in an empty
// composer — `Try "write a test for conversation.go"`, a long one cut short
// inside the quotes. Observed on 2.1.270 through 2.1.281; claude paints it only
// for some profiles, so the interrupt-* recordings never show it.
var placeholderRE = regexp.MustCompile(`^Try "[^"\n]*"$`)

// placeholderCol is where Claude parks the cursor over a placeholder: on the
// composer's first row, just past "❯ ". Text typed there puts it at the end.
const placeholderCol = 2

// minComposerMatch is how much of a prompt a composer must hold, without
// whitespace, before it counts as that prompt put back: a short word a person
// typed must not read as a cancelled turn.
const minComposerMatch = 12

// InterruptSequence returns Esc (see interruptKey). Implements
// turns.Interrupter.
func (*Adapter) InterruptSequence() []byte { return interruptKey }

// InterruptOutcome reads what Claude did with the turn whose prompt is prompt.
// Implements turns.Interrupter.
//
// Recorded on 2.1.280 (test/corpus/claude-code/interrupt-*):
//
//   - Stopped: "⎿  Interrupted · What should Claude do instead?" below the
//     partial reply or the stopped tool call, and an empty composer.
//   - Cancelled: an Esc before the first token takes the prompt's echo off the
//     screen and puts the prompt back in the composer; nothing else is painted.
//   - Finished: the turn's own end-of-turn summary.
//
// Everything is read below this turn's prompt echo — the last echo on screen,
// when it is this prompt's. An interrupt marker above it belongs to an earlier
// turn. When no echo is visible at all, the turn's has scrolled off with every
// earlier one, and the whole conversation on screen is this turn's.
func (a *Adapter) InterruptOutcome(prompt string, snap screen.Snapshot) (turns.InterruptOutcome, string) {
	if a.Busy(snap) {
		return turns.InterruptPending, ""
	}
	lines := strings.Split(snap.Text, "\n")
	top, bottom, ok := composerBounds(lines)
	if !ok {
		return turns.InterruptPending, ""
	}
	if composerHolds(prompt, composerText(lines, top, bottom)) {
		return turns.InterruptCancelled, ""
	}
	if top < 0 {
		// The composer fills the screen and is not this prompt: nothing of
		// the conversation is visible to read.
		return turns.InterruptPending, ""
	}
	region, ok := turnRegion(lines[:top], prompt)
	if !ok {
		return turns.InterruptPending, ""
	}
	marker, summary := -1, -1
	for i, ln := range region {
		switch {
		case interruptLineRE.MatchString(ln):
			marker = i
		case thinkingRE.MatchString(ln):
			summary = i
		}
	}
	switch {
	case summary > marker:
		// A marker above the summary is reply text quoting it.
		return turns.InterruptFinished, ""
	case marker >= 0:
		return turns.InterruptStopped, lastReply(region[:marker])
	default:
		return turns.InterruptPending, ""
	}
}

// ComposerText returns the text in Claude's composer box, as painted, and ""
// for an empty one showing its placeholder: the screen carries no dimming, so
// the placeholder is told from a draft of the same shape by the cursor, which
// Claude parks at its start. Implements turns.Interrupter.
func (*Adapter) ComposerText(snap screen.Snapshot) (string, bool) {
	lines := strings.Split(snap.Text, "\n")
	top, bottom, ok := composerBounds(lines)
	if !ok {
		return "", false
	}
	text := composerText(lines, top, bottom)
	if top >= 0 && snap.CursorRow == top+1 && snap.CursorCol == placeholderCol && placeholderRE.MatchString(text) {
		return "", true
	}
	return text, true
}

// ClearComposerSequence returns keys that empty a composer holding composer,
// wherever its cursor is: Ctrl-E to the end of the cursor's line, Ctrl-K twice
// per line to take every line below it, Ctrl-U twice per line (and once more)
// to take that line and every line above it. Implements turns.Interrupter.
//
// Lines are counted as painted, so a wrapped line counts once per screen row,
// which only adds presses; a spare press on an empty composer does nothing.
func (*Adapter) ClearComposerSequence(composer string) []byte {
	n := strings.Count(composer, "\n") + 1
	keys := make([]byte, 0, 1+2*n+2*n+1)
	keys = append(keys, keyEndOfLine)
	for range 2 * n {
		keys = append(keys, keyKillToEnd)
	}
	for range 2*n + 1 {
		keys = append(keys, keyKillStart)
	}
	return keys
}

// composerBounds locates the composer box: bottom is its lower rule, the last
// on screen, and top its upper rule — -1 when the composer runs off the top of
// the screen, as a long prompt put back in it does. ok is false when the screen
// shows no composer: no rule, a box whose first line is not the composer's ❯,
// or, with no upper rule, rows above that are not all composer rows (the
// Rewind picker paints one rule below the conversation).
func composerBounds(lines []string) (top, bottom int, ok bool) {
	bottom = -1
	for i := len(lines) - 1; i >= 0; i-- {
		if composerRuleRE.MatchString(lines[i]) {
			bottom = i
			break
		}
	}
	if bottom < 0 {
		return -1, -1, false
	}
	top = -1
	for i := bottom - 1; i >= 0; i-- {
		if composerRuleRE.MatchString(lines[i]) {
			top = i
			break
		}
	}
	if top >= 0 {
		if top+1 >= bottom || !strings.HasPrefix(lines[top+1], "❯") {
			return -1, -1, false
		}
		return top, bottom, true
	}
	rows := 0
	for _, ln := range lines[:bottom] {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		if !strings.HasPrefix(ln, "  ") {
			return -1, -1, false
		}
		rows++
	}
	return -1, bottom, rows > 0
}

// composerText returns the composer's text between the bounds composerBounds
// found: the ❯ glyph and the continuation indent removed, trailing padding
// trimmed.
func composerText(lines []string, top, bottom int) string {
	var body []string
	for i, ln := range lines[top+1 : bottom] {
		ln = strings.TrimRight(ln, " ")
		switch {
		case i == 0 && top >= 0:
			ln = strings.TrimPrefix(strings.TrimPrefix(ln, "❯"), " ")
		default:
			ln = strings.TrimPrefix(ln, "  ")
		}
		body = append(body, ln)
	}
	return strings.TrimSpace(strings.Join(body, "\n"))
}

// composerHolds reports whether composer is prompt put back: all of what it
// shows is prompt, whitespace aside (the composer wraps lines, and a long one
// shows only its tail), and enough of it to be more than a coincidence.
func composerHolds(prompt, composer string) bool {
	c, p := squash(composer), squash(prompt)
	return c != "" && strings.Contains(p, c) && len(c) >= min(len(p), minComposerMatch)
}

// turnRegion returns the conversation rows below this turn's prompt echo: the
// last echo on screen, when it is this prompt's. With no echo on screen the
// whole conversation is this turn's. ok is false when the last echo is another
// prompt's: this turn's echo is gone.
func turnRegion(convo []string, prompt string) ([]string, bool) {
	for i := len(convo) - 1; i >= 0; i-- {
		m := echoLineRE.FindStringSubmatch(convo[i])
		if m == nil {
			continue
		}
		if !echoOf(prompt, m[1]) {
			return nil, false
		}
		return convo[i+1:], true
	}
	return convo, true
}

// echoOf reports whether an echo's first row is prompt's: its text, whitespace
// aside, starts the prompt. A long first line wraps, so the row may be only its
// start. A paste Claude painted as a placeholder is taken as the prompt.
func echoOf(prompt, row string) bool {
	if strings.HasPrefix(strings.TrimSpace(row), "[Pasted text #") {
		return true
	}
	r := squash(row)
	return r != "" && strings.HasPrefix(squash(prompt), r)
}

// lastReply returns the last reply block in rows — the text the turn had
// painted — skipping tool calls; "" when there is none.
func lastReply(rows []string) string {
	for i := len(rows) - 1; i >= 0; i-- {
		m := bulletRE.FindStringSubmatch(rows[i])
		if m == nil || !isBullet(rows[i]) || toolCallRE.MatchString(m[1]) {
			continue
		}
		return assembleMessage(collectBlock(rows, i))
	}
	return ""
}

// squash drops all whitespace, so text compares whatever the rows it was
// wrapped into.
func squash(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
