package claudecode

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// --- the UNNUMBERED (selector-only) menu shape ---------------------------
//
// Claude Code 2.1.251 renders the folder-trust dialog WITHOUT choice numbers
// (captured live, tmux, 2026-08-29):
//
//	Accessing workspace:
//	/private/tmp/trustrepo
//	Quick safety check: Is this a project you created or one you trust? …
//	Claude Code'll be able to read, edit, and execute files here.
//	Security guide
//	 ❯ No, exit
//	   Yes, I trust this folder
//	Enter to confirm · Esc to cancel
//
// menuRE requires "<digit>.", so it yields nothing here and the dialog used to
// read as "no dialog at all" — see DetectInputDetail. Two details make a
// careless parser dangerous rather than merely useless:
//
//   - The default highlight sits on "No, exit". Answering with a bare CR, or
//     assuming the affirmative row comes first, QUITS claude at startup.
//   - The surrounding frame is prose ("Security guide", the workspace path, the
//     "Enter to confirm · Esc to cancel" footer). A parser that took "every line
//     after the anchor" would mint spurious options and shift the arrow offsets
//     of the real rows below them.

// selectorGlyph is the highlight marker Claude Code paints on the currently
// selected row (U+276F). It is ALSO the composer prompt glyph, which is why
// selector scanning only ever runs on the text that FOLLOWS a dialog anchor.
const selectorGlyph = "❯"

// minSelectorRows / maxSelectorRows bound a selector block. A single row is not
// a choice (that is what a stray composer glyph looks like), and a block longer
// than the cap is more likely to be prose that happens to align than a menu —
// both are reported unparseable, which is loud, rather than mis-navigated.
const (
	minSelectorRows = 2
	maxSelectorRows = 8
)

// borderRunes are the box-drawing glyphs Claude Code frames a dialog with. They
// appear as a line's left edge (stripped by lineContent so a boxed dialog
// measures its label column like a bare one) and as whole separator lines
// (terminators, see isTerminator).
const borderRunes = "╭╮╰╯─━│┃┌┐└┘├┤┼═║"

// footerHintRE matches the hint line a dialog closes with ("Enter to confirm ·
// Esc to cancel"). It is a terminator: it can align with the choice rows in
// some renderings, and it is not a choice.
var footerHintRE = regexp.MustCompile(`(?i)(enter to confirm|enter to continue|esc to |·)`)

// numberedLineRE recognizes a choice-SHAPED numbered line for the Pending vs
// Unparseable discrimination in DetectInputDetail. It is deliberately looser
// than menuRE (no label requirement): the question it answers is "did the menu
// render at all", not "can we parse it".
var numberedLineRE = regexp.MustCompile(`(?m)^[^\dA-Za-z\n]*\d\.`)

// parseSelectorMenu extracts the choices of an unnumbered, selector-highlighted
// menu from `after` — the frame text FOLLOWING the dialog anchor. It returns nil
// when the shape is anything other than a confidently identified block of
// sibling choice rows; callers treat nil as "unparseable", never as "no dialog".
//
// The rules, in order (each one exists to reject a specific real line of the
// captured frame):
//
//  1. Take the FIRST line carrying "❯" after the anchor. Its label column — the
//     rune index of the first non-space rune after the glyph and its padding —
//     is the block's alignment key.
//  2. Expand contiguously up and down from that row. A sibling qualifies only if
//     it is non-blank, starts its text at exactly the same label column, carries
//     no "❯" of its own, and is not a terminator (blank, box border, footer
//     hint). The column rule is what excludes "Security guide", the workspace
//     path and the prose line: choice labels start ~3 columns in, surrounding
//     prose starts at the box column.
//  3. The block must hold between minSelectorRows and maxSelectorRows rows.
func parseSelectorMenu(after string) []turns.InputOption {
	lines := strings.Split(after, "\n")

	highlight, labelCol := -1, -1
	for i, ln := range lines {
		col, ok := selectorLabelColumn(lineContent(ln))
		if !ok {
			continue
		}
		highlight, labelCol = i, col
		break
	}
	if highlight < 0 {
		return nil
	}

	start, end := highlight, highlight
	for i := highlight - 1; i >= 0; i-- {
		if !isSelectorSibling(lines[i], labelCol) {
			break
		}
		start = i
	}
	for i := highlight + 1; i < len(lines); i++ {
		if !isSelectorSibling(lines[i], labelCol) {
			break
		}
		end = i
	}

	rows := end - start + 1
	if rows < minSelectorRows || rows > maxSelectorRows {
		return nil
	}

	opts := make([]turns.InputOption, 0, rows)
	for i := start; i <= end; i++ {
		r := []rune(lineContent(lines[i]))
		if labelCol >= len(r) {
			return nil
		}
		label := cleanLabel(string(r[labelCol:]))
		if label == "" {
			return nil
		}
		opts = append(opts, turns.InputOption{
			// ID is the 0-based row index. A distinct namespace from the
			// numbered form's 1-based digits, which is fine: ids need only be
			// unique WITHIN a request and nothing persists them. findOption
			// (pkg/chat/input.go) matches ID, Alias AND Label, so alias-based
			// policy ("proceed"/"deny") keeps working across both shapes.
			ID:    strconv.Itoa(i - start),
			Alias: aliasForLabel(label),
			Label: label,
			Keys:  selectorKeys(i - highlight),
			// Server-side only; excluded from the request id hash (inputID
			// hashes kind + prompt + labels), so an arrow keypress that moves
			// the highlight does NOT mint a "new" request the policy answers a
			// second time.
			Highlighted: i == highlight,
		})
	}
	return opts
}

// selectorKeys encodes "move from the highlighted row to the row `delta` below
// it, then confirm". There are no digits to press, so selection is RELATIVE.
//
// Three properties this function must keep:
//
//   - NEVER a bare CR for a non-highlighted row. Claude highlights "No, exit"
//     by default, so a bare CR answers "yes" by quitting — the entire bug class
//     this parser exists for.
//   - The result is written as a SINGLE PTY write (Conversation.write passes the
//     whole opt.Keys to one WriteStdin). "ESC [ B" arriving in one write parses
//     as Down; split across writes it is a lone Esc, which CANCELS the dialog.
//     Do not split it, and do not sleep between the arrows.
//   - Arrow repetition, not absolute addressing: the offsets are only valid
//     against the highlight they were derived from, which is why the highlight
//     row is captured in the same pass as the labels.
func selectorKeys(delta int) []byte {
	switch {
	case delta > 0:
		return []byte(strings.Repeat("\x1b[B", delta) + "\r")
	case delta < 0:
		return []byte(strings.Repeat("\x1b[A", -delta) + "\r")
	default:
		return []byte("\r")
	}
}

// isSelectorSibling reports whether line is another choice row of a block whose
// labels start at labelCol: non-blank, no selector glyph of its own, not a
// terminator, and aligned to exactly that column.
func isSelectorSibling(line string, labelCol int) bool {
	if isTerminator(line) {
		return false
	}
	content := lineContent(line)
	if strings.Contains(content, selectorGlyph) {
		return false
	}
	col, ok := textColumn(content)
	return ok && col == labelCol
}

// isTerminator reports whether line ends a selector block: a blank line, a whole
// line of box-drawing chrome, or the dialog's footer hint.
func isTerminator(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return true
	}
	if footerHintRE.MatchString(trimmed) {
		return true
	}
	for _, r := range trimmed {
		if r != ' ' && !strings.ContainsRune(borderRunes, r) {
			return false
		}
	}
	return true
}

// lineContent strips a leading box border — the frame's left edge and the
// padding before it — so "│ ❯ No, exit" and " ❯ No, exit" measure the same
// label column. Only ONE leading border glyph is removed; everything after it,
// including the padding that sets the column, is preserved verbatim.
func lineContent(line string) string {
	r := []rune(line)
	i := 0
	for i < len(r) && (r[i] == ' ' || r[i] == '\t') {
		i++
	}
	if i < len(r) && strings.ContainsRune(borderRunes, r[i]) {
		return string(r[i+1:])
	}
	return line
}

// selectorLabelColumn returns the rune index at which the label of a
// "❯"-highlighted row begins — past the glyph and its trailing padding — and
// false when content carries no glyph or nothing follows it.
func selectorLabelColumn(content string) (int, bool) {
	r := []rune(content)
	for i, c := range r {
		if string(c) != selectorGlyph {
			continue
		}
		j := i + 1
		for j < len(r) && (r[j] == ' ' || r[j] == '\t') {
			j++
		}
		if j >= len(r) {
			return 0, false
		}
		return j, true
	}
	return 0, false
}

// textColumn returns the rune index of the first non-space rune, and false for
// a blank line.
func textColumn(content string) (int, bool) {
	for i, c := range []rune(content) {
		if c != ' ' && c != '\t' {
			return i, true
		}
	}
	return 0, false
}

// hasChoiceShapedLine reports whether `after` (the frame text following a dialog
// anchor) contains anything that LOOKS like a menu row — a selector-highlighted
// line or a numbered one. It is the discriminator between DetectPending (the
// menu has not painted yet: a mid-render frame, and silence is correct) and
// DetectUnparseable (the menu IS there and we could not read it, which must be
// loud).
func hasChoiceShapedLine(after string) bool {
	return strings.Contains(after, selectorGlyph) || numberedLineRE.MatchString(after)
}

// candidateLines returns up to maxSelectorRows non-blank lines following the
// anchor, trimmed. They are the evidence in an unrecognized-dialog report — and,
// hashed with the anchor, its dedup fingerprint — so an operator reading the log
// sees the shape that defeated the parser rather than just its name.
func candidateLines(after string) []string {
	var out []string
	for _, ln := range strings.Split(after, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		out = append(out, t)
		if len(out) == maxSelectorRows {
			break
		}
	}
	return out
}
