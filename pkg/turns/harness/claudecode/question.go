package claudecode

// AskUserQuestion dialog detection.
//
// Claude Code renders a full-screen-width dialog when the model calls its
// AskUserQuestion tool to ask the user a clarifying question mid-turn. While
// that dialog is up the harness is IDLE BUT NOT READY: there is no busy marker
// ("esc to interrupt" is gone), no end-of-turn "✻ <verb> for Ns" marker, and
// no empty composer — so nothing else in this package can tell the difference
// between "waiting on the user" and "wedged". Without the detection below the
// turn hangs silently until a caller-side deadline or the liveness watchdog
// kills it; with it, the dialog surfaces as a turns.InputRequest that the chat
// layer can auto-answer from policy or hand to a live client.
//
// THIS IS TUI PATTERN-MATCHING, pinned to a specific Claude Code release: the
// anchors, glyphs and row shapes below were verified live against 2.1.210 (the
// same build the meta-harness TypeScript sibling this is ported from was
// verified against). If a later release restyles the dialog — renames the
// "Enter to select ·" footer, drops the ☐/☒ tab strip, or renumbers the rows —
// detection goes SILENT rather than wrong: DetectQuestion returns nil, no
// InputRequest is raised, and the turn hangs exactly as it did before this
// detector existed. That is the failure mode to look for when a version bump
// makes clarifying questions start timing out.

import (
	"regexp"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// AskUserQuestion dialog anchors (verified live against 2.1.210). The dialog
// renders a tab-strip line ("☐ Color", or "←  ☒ Color  ☐ Size  ✔ Submit  →"
// for a multi-question / multi-select dialog), the question text, a numbered
// option menu, and a footer. The footer is the question pane's REQUIRED
// anchor; the review pane (shown after the last question) has no such footer
// and anchors on its own confirmation line instead.
const (
	// questionFooterAnchor starts the question pane's keybinding footer
	// ("Enter to select · ↑/↓ to navigate · Esc to cancel"). Matched as a
	// line PREFIX so the navigation hints — which differ between the
	// single-question and multi-question widgets — do not have to be known.
	questionFooterAnchor = "Enter to select ·"
	// questionReviewAnchor is the review pane's confirmation line.
	questionReviewAnchor = "Ready to submit your answers?"
	// questionSubmitTab is the trailing tab-strip entry a multi-question or
	// multi-select dialog carries. Its presence on the tab line is what makes
	// the review pane worth looking for; it is NOT proof of one, because an
	// unanswered question pane of the same dialog shows it too.
	questionSubmitTab = "✔ Submit"
	// questionOtherLabel is the UI-injected free-text escape hatch, rendered
	// as "Type something." on a single-select dialog and as a "Type something"
	// checkbox row on a multi-select one. Selecting it declines the structured
	// question and returns control to the composer.
	questionOtherLabel = "Type something"
	// questionChatLabel is the UI-injected "Chat about this" affordance
	// rendered below the option rule. Like the escape hatch it closes the
	// whole dialog rather than answering the question.
	questionChatLabel = "Chat about this"
)

// questionTabRE matches the dialog's tab-strip line: optional "←", then a
// ☐/☒ checkbox glyph starting the first tab entry.
//
// Checkbox glyphs ALSO occur in the to-do lists Claude Code renders inside
// replies ("  ☐ Wire the adapter"), so a tab-strip match is never on its own
// treated as a dialog — DetectQuestion additionally requires the footer (or
// the review anchor) below it. That footer/anchor requirement is the entire
// disambiguation between "a dialog is up" and "the reply happens to contain
// checkboxes".
var questionTabRE = regexp.MustCompile(`^[^\S\r\n]*(?:←[^\S\r\n]+)?[☐☒][^\S\r\n]`)

// questionOptionRE matches one option row: optional "❯" highlight, a number,
// then the label ("❯ 1. Red", "  2. [ ] Mushrooms"). Group 1 is the choice
// number, group 2 the rest of the line (label plus any trailing column
// padding), which cleanLabel then trims.
//
// Unlike menuRE it allows a MULTI-digit number and requires the "❯" highlight
// to be the only glyph before the number, so a numbered list inside a reply
// ("  1. First step") is not silently reinterpreted as a menu row — the
// footer requirement above is what keeps that list out of range anyway.
var questionOptionRE = regexp.MustCompile(`^[^\S\r\n]*(?:❯[^\S\r\n]+)?(\d+)\.[^\S\n]+(\S[^\n]*)$`)

// questionCheckboxRE strips the multi-select checkbox marker off a label
// ("[ ] Cheese" → "Cheese"). Stripping it is what keeps the request id stable
// as the user toggles rows: the marker flips between "[ ]" and "[✔]" on every
// keystroke, and an id that moved with it would make every toggle look like a
// brand-new dialog.
var questionCheckboxRE = regexp.MustCompile(`^\[[^\]]*\][^\S\n]+`)

// questionTabEntryRE captures one "☐ <label>" tab-strip entry. A label ends at
// the next glyph or a multi-space gap, so the trailing "✔ Submit" / "→" chrome
// never bleeds into it.
var questionTabEntryRE = regexp.MustCompile(`([☐☒])[^\S\r\n]+([^☐☒✔←→\s](?:[^☐☒✔←→]*[^☐☒✔←→\s])?)`)

// Question-dialog request kinds. Both are surfaced through the same
// turns.InputRequest channel as trust_prompt, so an InputPolicy can dispose of
// them by kind without knowing anything about Claude Code's TUI.
const (
	// kindQuestion is a pane asking one of the dialog's questions.
	kindQuestion = "question"
	// kindQuestionReview is the Submit/Cancel confirmation shown after the
	// last question of a multi-question or multi-select dialog.
	kindQuestionReview = "question_review"
)

// questionSubmitKeys commits a multi-select answer once the chosen rows have
// been toggled: Tab, which jumps the widget to its review pane. It is NOT the
// composer's enhanced Enter (see chat.submitKeyForHarness) — Enter on a
// checkbox dialog acts on the highlighted row, so it would toggle rather than
// commit. Carried on the request as turns.InputRequest.SubmitKeys.
var questionSubmitKeys = []byte("\t")

// DetectQuestion recognizes the AskUserQuestion dialog Claude Code renders
// when the model asks the user a clarifying question mid-turn. Two panes
// exist and each maps to its own request kind:
//
//   - a QUESTION pane (kind "question"): tab-strip line, question text,
//     numbered options, "Enter to select ·…" footer. Digit keys select an
//     option directly (single-select) or toggle its checkbox (multi-select).
//   - a REVIEW pane (kind "question_review"): shown after the last question of
//     a multi-question or multi-select dialog — an answers summary plus a
//     "Ready to submit your answers?" Submit/Cancel menu, and no select footer.
//
// Returns (nil, false) when neither pane is FULLY rendered. Half-detection is
// the dangerous case, not the missed one: an anchor lands on screen a repaint
// before its option rows do, and a request built from that intermediate frame
// would carry an empty (or truncated) option set that a policy could then
// "answer" with keystrokes aimed at rows the widget has not painted yet. So an
// anchor with no options is treated as "no dialog" and picked up on the next
// snapshot instead.
func DetectQuestion(text string) (*turns.InputRequest, bool) {
	lines := strings.Split(text, "\n")

	// The tab-strip line is the dialog's TOP EDGE, and the LAST match wins:
	// the dialog is painted below whatever reply content is still on screen,
	// so an earlier ☐/☒ (a to-do list in the reply, or a stale dialog from an
	// earlier question) must never be mistaken for this dialog's top edge.
	tabIdx := -1
	for i, ln := range lines {
		if questionTabRE.MatchString(ln) {
			tabIdx = i
		}
	}
	if tabIdx < 0 {
		return nil, false
	}
	tabLine := lines[tabIdx]

	// Review pane: the Submit tab plus the confirmation anchor BELOW the tab
	// line. Both are required — the Submit tab alone also appears on every
	// unanswered question pane of the same dialog.
	if strings.Contains(tabLine, questionSubmitTab) {
		anchorIdx := -1
		for i := tabIdx + 1; i < len(lines); i++ {
			if strings.Contains(lines[i], questionReviewAnchor) {
				anchorIdx = i
			}
		}
		if anchorIdx >= 0 {
			return reviewRequest(lines, tabIdx, anchorIdx)
		}
		// No review anchor: this is an unanswered question pane that merely
		// carries the Submit tab — fall through to the question path below.
	}

	// Question pane: the footer is required below the tab line. Without it a
	// bare ☐/☒ line is reply content, not a dialog.
	footerIdx := -1
	for i := tabIdx + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), questionFooterAnchor) {
			footerIdx = i
		}
	}
	if footerIdx < 0 {
		return nil, false
	}

	parsed := parseQuestionRegion(lines, tabIdx+1, footerIdx)
	if len(parsed.options) == 0 || parsed.preamble == "" {
		// Footer painted but the question text or the option rows are not —
		// partially rendered, see the doc comment above.
		return nil, false
	}
	return questionRequest(tabLine, parsed), true
}

// questionRequest builds the "question" pane request from a parsed region.
//
// The per-option keystrokes are the delicate part of this whole file. Claude
// Code's dialog widget treats a digit differently depending on the row, and
// getting it wrong does not fail loudly — it answers the WRONG question:
//
//   - multi-select option → the digit ALONE. It toggles that checkbox row; the
//     commit is a separate step (questionSubmitKeys), which is why appending a
//     confirm here would corrupt the toggle stream.
//   - the "Type something" / "Chat about this" affordances → digit + CR. On
//     these UI-injected rows the digit only moves the highlight onto them, so
//     the CR is what actually selects. It cannot leak: both rows close the
//     ENTIRE dialog, so there is no subsequent pane for a stray CR to land on.
//   - an ordinary single-select option → the digit ONLY, no trailing CR. The
//     digit already selects. In a MULTI-question dialog that selection
//     immediately advances to the next question, and a stray CR would then
//     select whatever option that question happens to have highlighted.
func questionRequest(tabLine string, parsed parsedQuestion) *turns.InputRequest {
	req := &turns.InputRequest{Kind: kindQuestion, Prompt: parsed.preamble}

	req.Options = make([]turns.InputOption, 0, len(parsed.options))
	for _, o := range parsed.options {
		// The escape hatch renders with a trailing period on the single-select
		// widget and without one on the multi-select checkbox row.
		other := o.label == questionOtherLabel || o.label == questionOtherLabel+"."
		chat := o.label == questionChatLabel

		var keys string
		switch {
		case parsed.multiSelect:
			keys = o.id
		case other, chat:
			keys = o.id + "\r"
		default:
			keys = o.id
		}

		var alias string
		switch {
		case other:
			alias = "other"
		case chat:
			alias = ""
		default:
			alias = aliasForLabel(o.label)
		}

		req.Options = append(req.Options, turns.InputOption{
			ID:          o.id,
			Alias:       alias,
			Label:       o.label,
			Description: o.description,
			Keys:        []byte(keys),
		})
	}

	// Header and MultiSelect describe the question pane only; the review pane
	// leaves both zero.
	req.Header = questionHeader(tabLine)
	if parsed.multiSelect {
		req.MultiSelect = true
		req.SubmitKeys = questionSubmitKeys
	}
	req.ID = inputID(req)
	return req
}

// reviewRequest builds the "question_review" pane request. tabIdx is the tab
// strip line and anchorIdx the "Ready to submit your answers?" line; the
// answers summary is everything between them and the Submit/Cancel menu is
// everything below.
func reviewRequest(lines []string, tabIdx, anchorIdx int) (*turns.InputRequest, bool) {
	parsed := parseQuestionRegion(lines, anchorIdx+1, len(lines))
	if len(parsed.options) == 0 {
		// Confirmation line painted, menu not yet — partially rendered.
		return nil, false
	}

	// The prompt is the rendered answers summary plus the confirmation line,
	// so a client (or a transcript) sees WHAT it is confirming, not just that
	// something needs confirming. Blank lines and box chrome are dropped.
	var body []string
	for _, ln := range lines[tabIdx+1 : anchorIdx+1] {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || boxOrRuleRE.MatchString(trimmed) {
			continue
		}
		body = append(body, trimmed)
	}

	req := &turns.InputRequest{Kind: kindQuestionReview, Prompt: strings.Join(body, "\n")}
	req.Options = make([]turns.InputOption, 0, len(parsed.options))
	for _, o := range parsed.options {
		req.Options = append(req.Options, turns.InputOption{
			ID:          o.id,
			Alias:       reviewAlias(o.label),
			Label:       o.label,
			Description: o.description,
			// Digit selects on this widget; the trailing CR is a no-op
			// BACKSTOP for a build where the digit only moves the highlight.
			// Unlike the question pane it is safe here: the review pane is the
			// dialog's last step, so once it is answered the dialog is gone and
			// a surplus CR has nothing left to mis-select.
			Keys: []byte(o.id + "\r"),
		})
	}
	req.ID = inputID(req)
	return req, true
}

// questionOption is one parsed option row of a question/review pane.
type questionOption struct {
	id          string
	label       string
	description string
}

// parsedQuestion is the result of reading one dialog region.
type parsedQuestion struct {
	// preamble is the non-blank lines before the first option row — the
	// question text.
	preamble string
	options  []questionOption
	// multiSelect is true when any option row carried a "[ ]" / "[✔]"
	// checkbox marker.
	multiSelect bool
}

// parseQuestionRegion reads the dialog region [from, to): the question text,
// the numbered options, and each option's description continuation lines.
// Options are de-duplicated by choice number so a redraw that paints a row
// twice yields one option.
func parseQuestionRegion(lines []string, from, to int) parsedQuestion {
	var out parsedQuestion
	var preamble []string
	seen := make(map[string]bool)

	for i := from; i < to && i < len(lines); i++ {
		ln := lines[i]
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || boxOrRuleRE.MatchString(ln) {
			continue
		}
		// The multi-select widget renders its commit control as a bare
		// "Submit" line below the last option. That is widget chrome, not an
		// option description — without this skip it would be glued onto the
		// preceding option's description and change the request id.
		if trimmed == "Submit" {
			continue
		}

		if m := questionOptionRE.FindStringSubmatch(ln); m != nil {
			num, label := m[1], cleanLabel(m[2])
			if questionCheckboxRE.MatchString(label) {
				out.multiSelect = true
				label = strings.TrimSpace(questionCheckboxRE.ReplaceAllString(label, ""))
			}
			if num == "0" || seen[num] || label == "" {
				continue
			}
			seen[num] = true
			out.options = append(out.options, questionOption{id: num, label: label})
			continue
		}

		// Before the first option row every non-blank line is question text;
		// after it, a non-option line is a continuation of the option above.
		if len(out.options) == 0 {
			preamble = append(preamble, trimmed)
			continue
		}
		cur := &out.options[len(out.options)-1]
		if cur.description == "" {
			cur.description = trimmed
		} else {
			cur.description += " " + trimmed
		}
	}

	out.preamble = strings.Join(preamble, "\n")
	return out
}

// questionHeader returns the ACTIVE question's tab label: the first unanswered
// (☐) entry on the tab strip. Answered questions are re-rendered as ☒ and stay
// on the strip, so taking the first entry outright would report the wrong
// question from the second one onward. Falls back to the first entry when
// every tab is already answered.
func questionHeader(tabLine string) string {
	first := ""
	for _, m := range questionTabEntryRE.FindAllStringSubmatch(tabLine, -1) {
		label := strings.TrimSpace(m[2])
		if first == "" {
			first = label
		}
		if m[1] == "☐" {
			return label
		}
	}
	return first
}

// reviewAlias maps a review-pane label to a portable intent. "Submit answers"
// carries none of aliasForLabel's proceed words, so the commit row is matched
// on "submit" first; everything else (notably "Cancel") falls through to the
// shared mapping.
func reviewAlias(label string) string {
	if strings.Contains(strings.ToLower(label), "submit") {
		return "proceed"
	}
	return aliasForLabel(label)
}
