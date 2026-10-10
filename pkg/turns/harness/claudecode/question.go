package claudecode

// AskUserQuestion: the dialog Claude Code paints when the model asks the user
// a clarifying question mid-turn, read as a turns.InputRequest, and the
// answers to it planned as turns.AnswerSteps.
//
// While that dialog is up the harness is IDLE BUT NOT READY: no busy marker,
// no end-of-turn marker, no composer. Unread, it looked like a prompt claude
// never accepted, and the chat layer failed the turn with "prompt not
// accepted / no assistant output" while the question sat on screen.
//
// THIS IS TUI PATTERN-MATCHING, verified live against 2.1.283 (it was first
// written against 2.1.210 for the screen driver of the time). If a later
// release restyles the dialog — renames the "Enter to select ·" footer, drops
// the ☐/☒ tab strip, renumbers the rows — detection goes SILENT rather than
// wrong: no request is raised, and the turn fails as it did before.
//
// What 2.1.283 does with keys, measured against a local Messages API:
//
//   - A digit on an ordinary single-select row selects it, and either answers
//     the question or moves the dialog on to its next question.
//   - A digit on a multi-select row toggles its checkbox and leaves the
//     highlight where it was; Tab then opens the review pane.
//   - "Type something" is a text field: a digit (or the arrows, on a
//     multi-select question) puts the highlight on it, typing replaces its
//     label, and Enter answers with the text. Enter with no text declines the
//     whole question. On a multi-select question, Down moves from the field
//     to the Submit row, whose Enter opens the review pane.
//   - "Chat about this" answers on its digit: it declines the question, and
//     claude goes on to ask what the user wants to clarify.
//   - On the review pane, a digit chooses Submit answers or Cancel.
//   - A burst of plain keys in one write reaches claude as a paste: "3Green\r"
//     answered "Red". Hence PlanAnswer, whose steps the chat layer writes one
//     at a time, each seen to land before the next.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// Question-dialog request kinds. Both reach a client through the same
// turns.InputRequest channel as KindTrustPrompt, so an InputPolicy can
// dispose of them by kind.
const (
	// KindQuestion is a pane asking one of the dialog's questions.
	KindQuestion = "question"
	// KindQuestionReview is the Submit/Cancel pane after the last answer of
	// a multi-question or multi-select dialog.
	KindQuestionReview = "question_review"
)

// AskUserQuestion dialog anchors and rows (verified live against 2.1.283).
// The dialog paints a tab strip ("☐ Colour", or "←  ☒ Colour  ☐ Size  ✔
// Submit  →" for a multi-question or multi-select one), the question, a
// numbered option menu, and a footer. The footer anchors a question pane;
// the review pane has none and anchors on its confirmation line.
const (
	// questionFooterAnchor starts a question pane's footer ("Enter to select
	// · ↑/↓ to navigate · Esc to cancel"). Matched as a line PREFIX: the
	// hints after it differ between the single- and multi-question panes,
	// and grow "ctrl+g to edit in Vim" while the text field has the
	// highlight.
	questionFooterAnchor = "Enter to select ·"
	// questionReviewAnchor is the review pane's confirmation line.
	questionReviewAnchor = "Ready to submit your answers?"
	// questionSubmitTab is the last tab-strip entry of a multi-question or
	// multi-select dialog. It makes the review pane worth looking for, but is
	// no proof of one: the dialog's question panes carry it too.
	questionSubmitTab = "✔ Submit"
	// questionOtherLabel is the text field's placeholder ("Type something."
	// on a single-select question, "Type something" on a multi-select one).
	questionOtherLabel = "Type something"
	// questionChatLabel is the row that declines the question to talk it
	// over; it is the pane's last row.
	questionChatLabel = "Chat about this"
	// questionSubmitRow is a multi-select question's Submit row, below the
	// text field: widget chrome, not an option.
	questionSubmitRow = "Submit"
)

// questionTabRE matches the dialog's tab-strip line: an optional "←", then a
// ☐/☒ glyph starting the first entry. Claude's to-do lists draw the same
// glyphs inside replies, so a match is a dialog only with the footer (or the
// review anchor) below it.
var questionTabRE = regexp.MustCompile(`^[^\S\r\n]*(?:←[^\S\r\n]+)?[☐☒][^\S\r\n]`)

// questionOptionRE matches an option row: an optional "❯" highlight (group
// 1), the number (group 2), then the label and any trailing padding (group
// 3). Only the "❯" may precede the number, so a numbered list in a reply is
// not read as a menu.
var questionOptionRE = regexp.MustCompile(`^[^\S\r\n]*(❯[^\S\r\n]+)?(\d+)\.[^\S\n]+(\S[^\n]*)$`)

// questionCheckboxRE matches a multi-select row's checkbox ("[ ]", "[✔]"),
// group 1 its mark. The checkbox is kept out of the label, so a toggle does
// not change the request id.
var questionCheckboxRE = regexp.MustCompile(`^\[([^\]]*)\][^\S\n]*`)

// questionTabEntryRE captures one "☐ <label>" tab-strip entry. A label ends
// at the next glyph or a gap of two spaces, so the "✔ Submit" and "→"
// chrome never bleeds into it.
var questionTabEntryRE = regexp.MustCompile(`([☐☒])[^\S\r\n]+([^☐☒✔←→\s](?:[^☐☒✔←→]*[^☐☒✔←→\s])?)`)

// detectQuestion reads a question pane or a review pane: DetectNone when
// neither pane's anchors are up, DetectPending when one is up but its
// question or options have not painted yet, and DetectOK with the request.
func detectQuestion(text string) (*turns.InputRequest, Detection) {
	lines := strings.Split(text, "\n")
	tabIdx := questionTabLine(lines)
	if tabIdx < 0 {
		return nil, DetectNone
	}
	tabLine := lines[tabIdx]
	if strings.Contains(tabLine, questionSubmitTab) {
		if anchorIdx := lastLineAfter(lines, tabIdx, func(ln string) bool { return strings.Contains(ln, questionReviewAnchor) }); anchorIdx >= 0 {
			return reviewRequest(lines, tabIdx, anchorIdx)
		}
	}
	footerIdx := lastLineAfter(lines, tabIdx, isQuestionFooter)
	if footerIdx < 0 {
		return nil, DetectNone
	}
	parsed := parseQuestionRegion(lines, tabIdx+1, footerIdx)
	if len(parsed.options) == 0 || parsed.preamble == "" {
		return nil, DetectPending
	}
	return questionRequest(tabLine, parsed), DetectOK
}

// questionPresent reports whether a question or review pane is painted, read
// or not.
func questionPresent(text string) bool {
	lines := strings.Split(text, "\n")
	tabIdx := questionTabLine(lines)
	if tabIdx < 0 {
		return false
	}
	return lastLineAfter(lines, tabIdx, func(ln string) bool {
		return isQuestionFooter(ln) || strings.Contains(ln, questionReviewAnchor)
	}) >= 0
}

// questionTabLine returns the index of the LAST tab-strip line: the dialog is
// painted below the conversation, so an earlier ☐/☒ is reply content.
func questionTabLine(lines []string) int {
	idx := -1
	for i, ln := range lines {
		if questionTabRE.MatchString(ln) {
			idx = i
		}
	}
	return idx
}

func lastLineAfter(lines []string, after int, match func(string) bool) int {
	idx := -1
	for i := after + 1; i < len(lines); i++ {
		if match(lines[i]) {
			idx = i
		}
	}
	return idx
}

func isQuestionFooter(ln string) bool {
	return strings.HasPrefix(strings.TrimSpace(ln), questionFooterAnchor)
}

// questionOption is one parsed option row.
type questionOption struct {
	id, label, description string
	highlighted, checked   bool
}

// parsedQuestion is one read of a pane's region.
type parsedQuestion struct {
	// preamble is the text before the first option row: the question.
	preamble    string
	options     []questionOption
	multiSelect bool
}

// parseQuestionRegion reads lines[from:to]: the question, then the numbered
// options with their description lines. A row painted twice by a redraw is
// read once.
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
		if strings.TrimSpace(strings.TrimPrefix(trimmed, selectorGlyph)) == questionSubmitRow {
			continue
		}
		if m := questionOptionRE.FindStringSubmatch(ln); m != nil {
			o := questionOption{id: m[2], label: cleanLabel(m[3]), highlighted: m[1] != ""}
			if c := questionCheckboxRE.FindStringSubmatch(o.label); c != nil {
				out.multiSelect = true
				o.checked = strings.TrimSpace(c[1]) != ""
				o.label = strings.TrimSpace(o.label[len(c[0]):])
			}
			if o.id == "0" || seen[o.id] || o.label == "" {
				continue
			}
			seen[o.id] = true
			out.options = append(out.options, o)
			continue
		}
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

// questionRequest builds a question pane's request.
//
// The text field's row is the one before "Chat about this", the pane's last
// row; once the user types, the text replaces its placeholder on screen. Its
// Label stays the placeholder and the text goes to Typed, so the request id
// — kind, prompt and labels — does not move while an answer is typed.
func questionRequest(tabLine string, parsed parsedQuestion) *turns.InputRequest {
	otherID := ""
	for _, o := range parsed.options {
		if o.label == questionChatLabel {
			if n, err := strconv.Atoi(o.id); err == nil && n > 1 {
				otherID = strconv.Itoa(n - 1)
			}
		}
	}
	req := &turns.InputRequest{Kind: KindQuestion, Prompt: parsed.preamble, Header: questionHeader(tabLine), MultiSelect: parsed.multiSelect}
	req.Options = make([]turns.InputOption, 0, len(parsed.options))
	for _, o := range parsed.options {
		opt := turns.InputOption{
			ID: o.id, Label: o.label, Description: o.description,
			Keys:        []byte(o.id),
			Highlighted: o.highlighted, Checked: o.checked,
		}
		placeholder := strings.TrimSuffix(o.label, ".") == questionOtherLabel
		switch {
		case o.label == questionChatLabel:
			opt.Alias = "chat"
		case o.id == otherID || (otherID == "" && placeholder):
			opt.Alias = "other"
			if !placeholder {
				opt.Typed = o.label
			}
			opt.Label = questionOtherLabel
		default:
			opt.Alias = aliasForLabel(o.label)
		}
		req.Options = append(req.Options, opt)
	}
	req.ID = inputID(req)
	return req
}

// reviewRequest builds the review pane's request: the answers summary and
// the confirmation line are its prompt, Submit answers and Cancel its
// options.
func reviewRequest(lines []string, tabIdx, anchorIdx int) (*turns.InputRequest, Detection) {
	parsed := parseQuestionRegion(lines, anchorIdx+1, len(lines))
	if len(parsed.options) < 2 {
		// Submit answers and Cancel: painted down to its first row, or
		// erased from its last, the pane is not the pane yet.
		return nil, DetectPending
	}
	var body []string
	for _, ln := range lines[tabIdx+1 : anchorIdx+1] {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || boxOrRuleRE.MatchString(ln) {
			continue
		}
		body = append(body, trimmed)
	}
	req := &turns.InputRequest{Kind: KindQuestionReview, Prompt: strings.Join(body, "\n")}
	req.Options = make([]turns.InputOption, 0, len(parsed.options))
	for _, o := range parsed.options {
		req.Options = append(req.Options, turns.InputOption{
			ID: o.id, Alias: reviewAlias(o.label), Label: o.label, Description: o.description,
			Keys: []byte(o.id), Highlighted: o.highlighted,
		})
	}
	req.ID = inputID(req)
	return req, DetectOK
}

// questionHeader returns the active question's tab label: the first
// unanswered (☐) entry, since answered ones stay on the strip as ☒. It falls
// back to the first entry when none is unanswered — which is also what a
// multi-select question shows once one of its rows is ticked.
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

// reviewAlias maps a review-pane label to an intent: "Submit answers" carries
// no word aliasForLabel knows, so it is matched on "submit" first.
func reviewAlias(label string) string {
	if strings.Contains(strings.ToLower(label), "submit") {
		return "proceed"
	}
	return aliasForLabel(label)
}

var _ turns.AnswerPlanner = (*Adapter)(nil)

// questionSettle is how long a pane must have been up before an answer's
// first key: 2.1.283 paints a pane before it takes input, and drops a key
// written as it appears. Measured, a key written 0 ms after the footer
// painted was lost 3 times in 3, at 50 ms once in 3, from 100 ms never.
const questionSettle = 300 * time.Millisecond

// PlanAnswer implements turns.AnswerPlanner for the question and review
// panes; every other dialog is answered as before.
func (*Adapter) PlanAnswer(req *turns.InputRequest, optionIDs []string, text string) ([]turns.AnswerStep, bool, error) {
	if len(req.Options) == 0 {
		// Not a pane detectQuestion read — it always has options. A
		// free-text prompt is answered as before.
		return nil, false, nil
	}
	var steps []turns.AnswerStep
	var err error
	switch req.Kind {
	case KindQuestion:
		steps, err = planQuestion(req, optionIDs, text)
	case KindQuestionReview:
		steps, err = planReview(req, optionIDs, text)
	default:
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	steps[0].After = questionSettle
	return steps, true, nil
}

func invalidAnswer(format string, a ...any) error {
	return fmt.Errorf("claude-code: %w: %s", turns.ErrInvalidAnswer, fmt.Sprintf(format, a...))
}

// planReview chooses Submit answers or Cancel.
func planReview(req *turns.InputRequest, ids []string, text string) ([]turns.AnswerStep, error) {
	if len(ids) != 1 || text != "" {
		return nil, invalidAnswer("the review pane takes one option and no text")
	}
	o := optionByID(req, ids[0])
	if o == nil {
		return nil, invalidAnswer("no option %q", ids[0])
	}
	return []turns.AnswerStep{{Keys: o.Keys, Until: turns.AnswerEvidence{Kind: turns.EvidenceGone}}}, nil
}

// planQuestion plans an answer to a question pane; see the file comment for
// what each key does.
func planQuestion(req *turns.InputRequest, ids []string, text string) ([]turns.AnswerStep, error) {
	text = oneLine(text)
	var other, chat *turns.InputOption
	var picks []*turns.InputOption
	for _, id := range ids {
		o := optionByID(req, id)
		switch {
		case o == nil:
			return nil, invalidAnswer("no option %q", id)
		case o.Alias == "other":
			other = o
		case o.Alias == "chat":
			chat = o
		default:
			picks = append(picks, o)
		}
	}
	switch {
	case len(ids) == 0:
		return nil, invalidAnswer("no option chosen")
	case !req.MultiSelect && len(ids) > 1:
		return nil, invalidAnswer("a single-select question takes one option")
	case chat != nil && len(ids) > 1:
		return nil, invalidAnswer("%q declines the question and takes no other option", chat.Label)
	case other != nil && text == "":
		return nil, invalidAnswer("the %q option takes text", other.Label)
	case other == nil && text != "":
		return nil, invalidAnswer("text needs the %q option", questionOtherLabel)
	}
	gone := turns.AnswerEvidence{Kind: turns.EvidenceGone}
	if chat != nil {
		return []turns.AnswerStep{{Keys: chat.Keys, Until: gone}}, nil
	}
	if !req.MultiSelect {
		if other == nil {
			return []turns.AnswerStep{{Keys: picks[0].Keys, Until: gone}}, nil
		}
		return []turns.AnswerStep{
			{Keys: other.Keys, Until: turns.AnswerEvidence{Kind: turns.EvidenceHighlighted, OptionID: other.ID}},
			{Keys: []byte(text), Until: turns.AnswerEvidence{Kind: turns.EvidenceTyped, OptionID: other.ID, Text: text}},
			{Keys: []byte("\r"), Until: gone},
		}, nil
	}
	var steps []turns.AnswerStep
	for _, o := range picks {
		if o.Checked {
			continue
		}
		steps = append(steps, turns.AnswerStep{Keys: o.Keys, Until: turns.AnswerEvidence{Kind: turns.EvidenceChecked, OptionID: o.ID}})
	}
	if other == nil {
		return append(steps, turns.AnswerStep{Keys: []byte("\t"), Until: gone}), nil
	}
	// The digits leave the highlight where the request found it, so the
	// arrows to the text field are counted from there.
	if nav := arrowKeys(optionIndex(req, other.ID) - highlightedIndex(req)); len(nav) > 0 {
		steps = append(steps, turns.AnswerStep{Keys: nav, Until: turns.AnswerEvidence{Kind: turns.EvidenceHighlighted, OptionID: other.ID}})
	}
	return append(
		steps,
		turns.AnswerStep{Keys: []byte(text), Until: turns.AnswerEvidence{Kind: turns.EvidenceTyped, OptionID: other.ID, Text: text}},
		turns.AnswerStep{Keys: []byte("\x1b[B"), Until: turns.AnswerEvidence{Kind: turns.EvidenceUnhighlighted, OptionID: other.ID}},
		turns.AnswerStep{Keys: []byte("\r"), Until: gone},
	), nil
}

// oneLine makes text fit the one-line text field: a newline would submit it
// early, and the screen shows a run of spaces as one gap the label parse
// cuts at, so whitespace runs become one space.
func oneLine(text string) string {
	return strings.Join(strings.FieldsFunc(text, func(r rune) bool { return r == ' ' || r < 0x20 || r == 0x7f }), " ")
}

func optionByID(req *turns.InputRequest, id string) *turns.InputOption {
	for i := range req.Options {
		if req.Options[i].ID == id {
			return &req.Options[i]
		}
	}
	return nil
}

func optionIndex(req *turns.InputRequest, id string) int {
	for i := range req.Options {
		if req.Options[i].ID == id {
			return i
		}
	}
	return 0
}

// highlightedIndex is the highlighted option's index, or the first row's
// when none reads as highlighted.
func highlightedIndex(req *turns.InputRequest) int {
	for i := range req.Options {
		if req.Options[i].Highlighted {
			return i
		}
	}
	return 0
}

// arrowKeys moves the highlight delta rows: down for positive, up for
// negative.
func arrowKeys(delta int) []byte {
	switch {
	case delta > 0:
		return []byte(strings.Repeat("\x1b[B", delta))
	case delta < 0:
		return []byte(strings.Repeat("\x1b[A", -delta))
	}
	return nil
}
