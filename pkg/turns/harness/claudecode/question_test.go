package claudecode

import (
	"errors"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// The frames below are Claude Code 2.1.283's AskUserQuestion dialog as the
// screen driver read it, live, against a local Messages API that asked the
// question (trailing padding trimmed).

var rule = strings.Repeat("─", 120)

func frame(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// singleFrame is a single-select question, the highlight on its first row.
var singleFrame = frame(
	"❯ HW_ASK: ask me",
	rule,
	" ☐ Colour",
	"",
	"Which colour do you prefer?",
	"",
	"❯ 1. Red",
	"     You prefer red",
	"  2. Blue",
	"     You prefer blue",
	"  3. Type something.",
	rule,
	"  4. Chat about this",
	"",
	"Enter to select · ↑/↓ to navigate · Esc to cancel",
)

// singleOtherFrame is singleFrame after "3": the highlight on the text field,
// and typed, its text in place of the placeholder.
func singleOtherFrame(typed string) string {
	label := "Type something."
	if typed != "" {
		label = typed
	}
	return frame(
		"❯ HW_ASK: ask me",
		rule,
		" ☐ Colour",
		"",
		"Which colour do you prefer?",
		"",
		"  1. Red",
		"     You prefer red",
		"  2. Blue",
		"     You prefer blue",
		"❯ 3. "+label,
		rule,
		"  4. Chat about this",
		"",
		"Enter to select · ↑/↓ to navigate · ctrl+g to edit in Vim · Esc to cancel",
	)
}

// multiFrame is a multi-select question. checked names the ticked rows, at
// which row the highlight is ("Submit" for the Submit row), and typed what
// the text field shows (its row ticked once it does).
func multiFrame(checked map[string]bool, at, typed string) string {
	box := func(id string) string {
		if checked[id] {
			return "[✔]"
		}
		return "[ ]"
	}
	mark := func(id string) string {
		if at == id {
			return "❯ "
		}
		return "  "
	}
	tab := "☐"
	if len(checked) > 0 || typed != "" {
		tab = "☒"
	}
	other := box("4") + " Type something"
	if typed != "" {
		other = "[✔] " + typed
	}
	return frame(
		"❯ HW_ASK: ask me",
		rule,
		"←  "+tab+" Toppings  ✔ Submit  →",
		"",
		"Which toppings do you want?",
		"",
		mark("1")+"1. "+box("1")+" Cheese",
		"         Melted cheese",
		mark("2")+"2. "+box("2")+" Mushrooms",
		"         Sliced mushrooms",
		mark("3")+"3. "+box("3")+" Olives",
		"         Black olives",
		mark("4")+"4. "+other,
		mark("Submit")+"   Submit",
		rule,
		"  5. Chat about this",
		"",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
	)
}

// twoQuestionFrames are a two-question dialog's first and second panes.
var twoQuestionFrames = [2]string{
	frame(
		"❯ HW_ASK: ask me",
		rule,
		"←  ☐ Colour  ☐ Size  ✔ Submit  →",
		"",
		"Which colour do you prefer?",
		"",
		"❯ 1. Red",
		"     You prefer red",
		"  2. Blue",
		"     You prefer blue",
		"  3. Type something.",
		"  4. Chat about this",
		"",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	),
	frame(
		"❯ HW_ASK: ask me",
		rule,
		"←  ☒ Colour  ☐ Size  ✔ Submit  →",
		"",
		"Which size do you need?",
		"",
		"❯ 1. Small",
		"     A small one",
		"  2. Large",
		"     A large one",
		"  3. Type something.",
		"  4. Chat about this",
		"",
		"Enter to select · Tab/Arrow keys to navigate · Esc to cancel",
	),
}

// reviewFrame is the two-question dialog's review pane.
var reviewFrame = frame(
	"❯ HW_ASK: ask me",
	rule,
	"←  ☒ Colour  ☒ Size  ✔ Submit  →",
	"",
	"Review your answers",
	"",
	" ● Which colour do you prefer?",
	"   → Blue",
	" ● Which size do you need?",
	"   → Small",
	"",
	"Ready to submit your answers?",
	"",
	"❯ 1. Submit answers",
	"  2. Cancel",
)

// answeredFrame is the screen once the question is answered: the answer is
// in the conversation, its question text with it, and the composer is back.
var answeredFrame = frame(
	"❯ HW_ASK: ask me",
	"⏺ User answered Claude's questions:",
	"  ⎿  · Which colour do you prefer? → Blue",
	"⏺ GOT_ANSWER",
	"✻ Crunched for 0s · done 9:08 AM",
	rule,
	"❯ ",
	rule,
	"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents",
)

func mustQuestion(t *testing.T, text string) *turns.InputRequest {
	t.Helper()
	req, det := DetectInputDetail(text)
	if det != DetectOK || req == nil {
		t.Fatalf("DetectInputDetail = %v, want DetectOK:\n%s", det, text)
	}
	return req
}

func TestDetectQuestion_SingleSelect(t *testing.T) {
	req := mustQuestion(t, singleFrame)
	if req.Kind != KindQuestion || req.Prompt != "Which colour do you prefer?" || req.Header != "Colour" || req.MultiSelect {
		t.Fatalf("request = kind %q prompt %q header %q multi %v", req.Kind, req.Prompt, req.Header, req.MultiSelect)
	}
	want := []turns.InputOption{
		{ID: "1", Label: "Red", Description: "You prefer red", Keys: []byte("1"), Highlighted: true},
		{ID: "2", Label: "Blue", Description: "You prefer blue", Keys: []byte("2")},
		{ID: "3", Alias: "other", Label: "Type something", Keys: []byte("3")},
		{ID: "4", Alias: "chat", Label: "Chat about this", Keys: []byte("4")},
	}
	assertOptions(t, req.Options, want)
}

func TestDetectQuestion_TypedTextKeepsTheRequestID(t *testing.T) {
	base := mustQuestion(t, singleFrame)
	highlighted := mustQuestion(t, singleOtherFrame(""))
	typed := mustQuestion(t, singleOtherFrame("Green"))
	if highlighted.ID != base.ID || typed.ID != base.ID {
		t.Fatalf("ids %s / %s / %s: moving the highlight or typing must not make a new request", base.ID, highlighted.ID, typed.ID)
	}
	other := optionByID(typed, "3")
	if other.Label != "Type something" || other.Typed != "Green" || !other.Highlighted || other.Alias != "other" {
		t.Fatalf("text field = %+v, want the placeholder label, Typed \"Green\", highlighted", *other)
	}
	if o := optionByID(highlighted, "3"); o.Typed != "" {
		t.Fatalf("an empty field reads as Typed %q", o.Typed)
	}
}

func TestDetectQuestion_MultiSelect(t *testing.T) {
	req := mustQuestion(t, multiFrame(nil, "1", ""))
	if !req.MultiSelect || req.Header != "Toppings" || req.Prompt != "Which toppings do you want?" {
		t.Fatalf("request = multi %v header %q prompt %q", req.MultiSelect, req.Header, req.Prompt)
	}
	want := []turns.InputOption{
		{ID: "1", Label: "Cheese", Description: "Melted cheese", Keys: []byte("1"), Highlighted: true},
		{ID: "2", Label: "Mushrooms", Description: "Sliced mushrooms", Keys: []byte("2")},
		{ID: "3", Label: "Olives", Description: "Black olives", Keys: []byte("3")},
		{ID: "4", Alias: "other", Label: "Type something", Keys: []byte("4")},
		{ID: "5", Alias: "chat", Label: "Chat about this", Keys: []byte("5")},
	}
	assertOptions(t, req.Options, want)

	toggled := mustQuestion(t, multiFrame(map[string]bool{"1": true, "3": true}, "1", ""))
	typed := mustQuestion(t, multiFrame(map[string]bool{"1": true}, "4", "Green"))
	onSubmit := mustQuestion(t, multiFrame(map[string]bool{"1": true}, "Submit", "Green"))
	for _, r := range []*turns.InputRequest{toggled, typed, onSubmit} {
		if r.ID != req.ID {
			t.Fatalf("id %s, want %s: ticking, typing and moving the highlight must not make a new request", r.ID, req.ID)
		}
	}
	if !optionByID(toggled, "1").Checked || optionByID(toggled, "2").Checked || !optionByID(toggled, "3").Checked {
		t.Fatalf("checked = %v %v %v, want rows 1 and 3", optionByID(toggled, "1").Checked, optionByID(toggled, "2").Checked, optionByID(toggled, "3").Checked)
	}
	if o := optionByID(typed, "4"); o.Typed != "Green" || !o.Highlighted || o.Label != "Type something" {
		t.Fatalf("text field = %+v", *o)
	}
	for _, o := range onSubmit.Options {
		if o.Highlighted {
			t.Fatalf("option %s reads as highlighted while the Submit row is", o.ID)
		}
	}
}

func TestDetectQuestion_EachQuestionIsItsOwnRequest(t *testing.T) {
	q1, q2 := mustQuestion(t, twoQuestionFrames[0]), mustQuestion(t, twoQuestionFrames[1])
	if q1.Header != "Colour" || q2.Header != "Size" || q2.Prompt != "Which size do you need?" {
		t.Fatalf("headers %q, %q, prompt %q", q1.Header, q2.Header, q2.Prompt)
	}
	if q1.ID == q2.ID {
		t.Fatal("the two questions share a request id")
	}
}

func TestDetectQuestion_ReviewPane(t *testing.T) {
	req := mustQuestion(t, reviewFrame)
	wantPrompt := "Review your answers\n● Which colour do you prefer?\n→ Blue\n● Which size do you need?\n→ Small\nReady to submit your answers?"
	if req.Kind != KindQuestionReview || req.Prompt != wantPrompt {
		t.Fatalf("request = kind %q prompt %q", req.Kind, req.Prompt)
	}
	assertOptions(t, req.Options, []turns.InputOption{
		{ID: "1", Alias: "proceed", Label: "Submit answers", Keys: []byte("1"), Highlighted: true},
		{ID: "2", Alias: "deny", Label: "Cancel", Keys: []byte("2")},
	})
}

func TestDetectQuestion_NotADialog(t *testing.T) {
	reply := frame(
		"⏺ Plan:",
		"  ☐ Wire the adapter",
		"  ☐ Write the tests",
		"  1. First step",
		"  2. Second step",
		rule,
		"❯ ",
		rule,
	)
	for name, text := range map[string]string{"a reply with a to-do list": reply, "the answered question": answeredFrame} {
		if req, det := DetectInputDetail(text); det != DetectNone || req != nil {
			t.Errorf("%s: DetectInputDetail = %v, want DetectNone", name, det)
		}
		if AnchorPresent(text) {
			t.Errorf("%s: AnchorPresent = true", name)
		}
	}
	midPaint := frame(rule, " ☐ Colour", "", "Enter to select · ↑/↓ to navigate · Esc to cancel")
	if _, det := DetectInputDetail(midPaint); det != DetectPending {
		t.Errorf("a pane with no question or rows yet: %v, want DetectPending", det)
	}
}

// The failure this file exists for: with the dialog up, the screen must not
// read as ready, or the chat layer completes the turn as "prompt not
// accepted" while the question waits for an answer.
func TestQuestion_TheScreenIsNotReady(t *testing.T) {
	a := New()
	for name, text := range map[string]string{"question": singleFrame, "multi-select": multiFrame(nil, "1", ""), "review": reviewFrame} {
		if a.ReadyForInput(text) {
			t.Errorf("%s: ReadyForInput = true with the dialog up", name)
		}
		if !AnchorPresent(text) {
			t.Errorf("%s: AnchorPresent = false with the dialog up", name)
		}
	}
}

func assertOptions(t *testing.T, got, want []turns.InputOption) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d options %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.Alias != w.Alias || g.Label != w.Label || g.Description != w.Description ||
			string(g.Keys) != string(w.Keys) || g.Highlighted != w.Highlighted || g.Checked != w.Checked || g.Typed != w.Typed {
			t.Errorf("option %d = %+v, want %+v", i, g, w)
		}
	}
}

// --- PlanAnswer

func gone() turns.AnswerEvidence { return turns.AnswerEvidence{Kind: turns.EvidenceGone} }

func until(kind turns.EvidenceKind, id, text string) turns.AnswerEvidence {
	return turns.AnswerEvidence{Kind: kind, OptionID: id, Text: text}
}

func plan(t *testing.T, req *turns.InputRequest, ids []string, text string) []turns.AnswerStep {
	t.Helper()
	steps, ok, err := New().PlanAnswer(req, ids, text)
	if err != nil || !ok {
		t.Fatalf("PlanAnswer(%v, %q) = ok %v, %v", ids, text, ok, err)
	}
	return steps
}

// settled is want with its first step written only once the pane has been
// up for questionSettle — every plan's first step.
func settled(want ...turns.AnswerStep) []turns.AnswerStep {
	want[0].After = questionSettle
	return want
}

func assertSteps(t *testing.T, got, want []turns.AnswerStep) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d steps %+v, want %d %+v", len(got), got, len(want), want)
	}
	for i := range want {
		if string(got[i].Keys) != string(want[i].Keys) || got[i].Until != want[i].Until || got[i].After != want[i].After {
			t.Errorf("step %d = keys %q until %+v after %v, want keys %q until %+v after %v", i, got[i].Keys, got[i].Until, got[i].After, want[i].Keys, want[i].Until, want[i].After)
		}
	}
}

func TestPlanAnswer_SingleSelect(t *testing.T) {
	req := mustQuestion(t, singleFrame)
	assertSteps(t, plan(t, req, []string{"2"}, ""), settled(turns.AnswerStep{Keys: []byte("2"), Until: gone()}))
	assertSteps(t, plan(t, req, []string{"4"}, ""), settled(turns.AnswerStep{Keys: []byte("4"), Until: gone()}))
	// Whitespace runs collapse and a newline cannot submit the field early.
	assertSteps(t, plan(t, req, []string{"3"}, " Green\nish  shade "), settled(
		turns.AnswerStep{Keys: []byte("3"), Until: until(turns.EvidenceHighlighted, "3", "")},
		turns.AnswerStep{Keys: []byte("Green ish shade"), Until: until(turns.EvidenceTyped, "3", "Green ish shade")},
		turns.AnswerStep{Keys: []byte("\r"), Until: gone()},
	))
}

func TestPlanAnswer_MultiSelect(t *testing.T) {
	req := mustQuestion(t, multiFrame(nil, "1", ""))
	assertSteps(t, plan(t, req, []string{"1", "3"}, ""), settled(
		turns.AnswerStep{Keys: []byte("1"), Until: until(turns.EvidenceChecked, "1", "")},
		turns.AnswerStep{Keys: []byte("3"), Until: until(turns.EvidenceChecked, "3", "")},
		turns.AnswerStep{Keys: []byte("\t"), Until: gone()},
	))
	assertSteps(t, plan(t, req, []string{"1", "4"}, "Green"), settled(
		turns.AnswerStep{Keys: []byte("1"), Until: until(turns.EvidenceChecked, "1", "")},
		turns.AnswerStep{Keys: []byte("\x1b[B\x1b[B\x1b[B"), Until: until(turns.EvidenceHighlighted, "4", "")},
		turns.AnswerStep{Keys: []byte("Green"), Until: until(turns.EvidenceTyped, "4", "Green")},
		turns.AnswerStep{Keys: []byte("\x1b[B"), Until: until(turns.EvidenceUnhighlighted, "4", "")},
		turns.AnswerStep{Keys: []byte("\r"), Until: gone()},
	))
	// A row already ticked is not toggled again, which would untick it.
	ticked := mustQuestion(t, multiFrame(map[string]bool{"1": true}, "1", ""))
	assertSteps(t, plan(t, ticked, []string{"1", "2"}, ""), settled(
		turns.AnswerStep{Keys: []byte("2"), Until: until(turns.EvidenceChecked, "2", "")},
		turns.AnswerStep{Keys: []byte("\t"), Until: gone()},
	))
	assertSteps(t, plan(t, req, []string{"5"}, ""), settled(turns.AnswerStep{Keys: []byte("5"), Until: gone()}))
}

func TestPlanAnswer_ReviewPane(t *testing.T) {
	req := mustQuestion(t, reviewFrame)
	assertSteps(t, plan(t, req, []string{"1"}, ""), settled(turns.AnswerStep{Keys: []byte("1"), Until: gone()}))
}

func TestPlanAnswer_RefusesWhatTheDialogCannotTake(t *testing.T) {
	single, multi, review := mustQuestion(t, singleFrame), mustQuestion(t, multiFrame(nil, "1", "")), mustQuestion(t, reviewFrame)
	for name, c := range map[string]struct {
		req  *turns.InputRequest
		ids  []string
		text string
	}{
		"no option":                    {single, nil, ""},
		"two options, single-select":   {single, []string{"1", "2"}, ""},
		"the text field without text":  {single, []string{"3"}, "  "},
		"text without the text field":  {single, []string{"1"}, "Green"},
		"chat with another option":     {multi, []string{"1", "5"}, ""},
		"an unknown option":            {multi, []string{"9"}, ""},
		"text on the review pane":      {review, []string{"1"}, "yes"},
		"two options, the review pane": {review, []string{"1", "2"}, ""},
	} {
		if _, _, err := New().PlanAnswer(c.req, c.ids, c.text); !errors.Is(err, turns.ErrInvalidAnswer) {
			t.Errorf("%s: err = %v, want ErrInvalidAnswer", name, err)
		}
	}
}

func TestPlanAnswer_LeavesOtherDialogsAlone(t *testing.T) {
	trust := &turns.InputRequest{Kind: KindTrustPrompt, Options: []turns.InputOption{{ID: "1", Label: "Yes", Keys: []byte("1")}}}
	freeText := &turns.InputRequest{Kind: KindQuestion}
	for name, req := range map[string]*turns.InputRequest{"trust prompt": trust, "question with no options": freeText} {
		if steps, ok, err := New().PlanAnswer(req, []string{"1"}, ""); ok || err != nil || steps != nil {
			t.Errorf("%s: PlanAnswer = %v, %v, %v; want not planned", name, steps, ok, err)
		}
	}
}
