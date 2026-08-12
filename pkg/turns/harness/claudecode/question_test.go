package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// The fixtures below are AskUserQuestion dialogs as the vt100 emulator renders
// them, captured live against Claude Code 2.1.210 (the build the meta-harness
// TypeScript sibling this detector was ported from was verified against). The
// full-width horizontal rules are the dialog's own chrome, not test padding —
// parseQuestionRegion has to skip them, so they stay.

// singleQuestionScreen is a one-question, single-select dialog: a bare "☐ Color"
// tab strip (no Submit tab, because there is nothing to review), the question,
// two real options, and the two UI-injected affordance rows.
const singleQuestionScreen = `⏺ I'll ask you the question now.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
 ☐ Color

Which color should I use?

❯ 1. Red
     Use red.
  2. Blue
     Use blue.
  3. Type something.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
  4. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel
`

// secondQuestionScreen is the SECOND pane of a two-question dialog: question
// one is answered (☒ Color) and the Submit tab is present, but there is no
// review anchor yet — the detector must fall through to the question path.
const secondQuestionScreen = `⏺ I'll ask both questions in a single call.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
←  ☒ Color  ☐ Size  ✔ Submit  →

Which size should I use?

❯ 1. Small
     Use small.
  2. Large
     Use large.
  3. Type something.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
  4. Chat about this

Enter to select · Tab/Arrow keys to navigate · Esc to cancel
`

// reviewScreen is the pane after the last question: every tab answered, an
// answers summary, and a Submit/Cancel menu with NO select footer.
const reviewScreen = `⏺ I'll ask both questions in a single call.

────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
←  ☒ Color  ☒ Size  ✔ Submit  →

Review your answers

 ● Which color should I use?
   → Blue
 ● Which size should I use?
   → Small

Ready to submit your answers?

❯ 1. Submit answers
  2. Cancel
`

// multiSelectScreen is a checkbox dialog: "[ ]" / "[✔]" markers on the option
// rows and the widget's bare "Submit" commit row below the last one.
const multiSelectScreen = `⏺ I'll ask you the question now.
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
←  ☐ Toppings  ✔ Submit  →

Which toppings should I add?

❯ 1. [ ] Cheese
  Add cheese as a topping.
  2. [✔] Mushrooms
  Add mushrooms as a topping.
  3. [ ] Olives
  Add olives as a topping.
  4. [ ] Type something
     Submit
────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
  5. Chat about this

Enter to select · ↑/↓ to navigate · Esc to cancel
`

// todoReplyScreen is a rendered reply carrying a to-do list and a numbered
// list — checkbox glyphs and menu-shaped rows with NO dialog anywhere. This is
// the false positive the footer requirement exists to prevent.
const todoReplyScreen = `⏺ Here is the plan:
  ☐ Wire the adapter
  ☐ Add tests

  1. First step
  2. Second step

❯
`

// wantOption is the assertable projection of a turns.InputOption: everything
// except Keys is client-visible, and Keys is the whole point of this detector.
type wantOption struct {
	id, alias, label, desc, keys string
}

func checkOptions(t *testing.T, got []turns.InputOption, want []wantOption) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len(Options) = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i, w := range want {
		o := got[i]
		if o.ID != w.id || o.Alias != w.alias || o.Label != w.label ||
			o.Description != w.desc || string(o.Keys) != w.keys {
			t.Errorf("Options[%d] = {id:%q alias:%q label:%q desc:%q keys:%q}, want {id:%q alias:%q label:%q desc:%q keys:%q}",
				i, o.ID, o.Alias, o.Label, o.Description, o.Keys,
				w.id, w.alias, w.label, w.desc, w.keys)
		}
	}
}

// A single-select question pane: header from the tab strip, question text as
// the prompt, and the three keystroke classes side by side — ordinary options
// get the bare digit (a trailing CR would select the NEXT question's
// highlighted option in a multi-question dialog) while the two UI-injected
// affordance rows get digit + CR (the digit only moves the highlight there).
func TestDetectQuestion_SingleSelect(t *testing.T) {
	req, ok := DetectInput(singleQuestionScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the single-select question dialog")
	}
	if req.Kind != kindQuestion {
		t.Errorf("Kind = %q, want %q", req.Kind, kindQuestion)
	}
	if req.Prompt != "Which color should I use?" {
		t.Errorf("Prompt = %q, want %q", req.Prompt, "Which color should I use?")
	}
	if req.Header != "Color" {
		t.Errorf("Header = %q, want Color", req.Header)
	}
	if req.MultiSelect {
		t.Error("MultiSelect = true on a single-select dialog")
	}
	if len(req.SubmitKeys) != 0 {
		t.Errorf("SubmitKeys = %q, want none on a single-select dialog", req.SubmitKeys)
	}
	checkOptions(t, req.Options, []wantOption{
		{id: "1", alias: "", label: "Red", desc: "Use red.", keys: "1"},
		{id: "2", alias: "", label: "Blue", desc: "Use blue.", keys: "2"},
		{id: "3", alias: "other", label: "Type something.", desc: "", keys: "3\r"},
		{id: "4", alias: "", label: "Chat about this", desc: "", keys: "4\r"},
	})
	if req.ID == "" {
		t.Error("request ID is empty")
	}
}

// The second pane of a multi-question dialog carries the "✔ Submit" tab but no
// review anchor, so detection must fall through from the review branch to the
// question branch — and the header must name the first UNANSWERED (☐) tab, not
// the answered ☒ one still on the strip.
func TestDetectQuestion_SecondQuestionFallsThroughToQuestionPane(t *testing.T) {
	req, ok := DetectInput(secondQuestionScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the second question pane")
	}
	if req.Kind != kindQuestion {
		t.Errorf("Kind = %q, want %q", req.Kind, kindQuestion)
	}
	if req.Prompt != "Which size should I use?" {
		t.Errorf("Prompt = %q, want %q", req.Prompt, "Which size should I use?")
	}
	if req.Header != "Size" {
		t.Errorf("Header = %q, want Size (the first unanswered tab)", req.Header)
	}
	first, _ := DetectInput(singleQuestionScreen)
	if req.ID == first.ID {
		t.Error("a different question produced the same request ID")
	}
}

// The review pane: its own kind, the answers summary folded into the prompt so
// a client sees what it is confirming, and digit + CR keys (safe here because
// the dialog is gone once this pane is answered).
func TestDetectQuestion_ReviewPane(t *testing.T) {
	req, ok := DetectInput(reviewScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the review pane")
	}
	if req.Kind != kindQuestionReview {
		t.Errorf("Kind = %q, want %q", req.Kind, kindQuestionReview)
	}
	for _, want := range []string{"Ready to submit your answers?", "→ Blue", "→ Small"} {
		if !strings.Contains(req.Prompt, want) {
			t.Errorf("Prompt = %q, want it to contain %q", req.Prompt, want)
		}
	}
	// Header and MultiSelect belong to the question pane only.
	if req.Header != "" {
		t.Errorf("Header = %q, want empty on the review pane", req.Header)
	}
	if req.MultiSelect {
		t.Error("MultiSelect = true on the review pane")
	}
	checkOptions(t, req.Options, []wantOption{
		{id: "1", alias: "proceed", label: "Submit answers", desc: "", keys: "1\r"},
		{id: "2", alias: "deny", label: "Cancel", desc: "", keys: "2\r"},
	})
}

// A checkbox dialog: markers stripped off the labels, MultiSelect set, Tab as
// the commit sequence, and TOGGLE-ONLY digit keys on every row (including the
// affordance rows, where multi-select wins over the digit+CR rule).
func TestDetectQuestion_MultiSelect(t *testing.T) {
	req, ok := DetectInput(multiSelectScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the multi-select question dialog")
	}
	if req.Kind != kindQuestion {
		t.Errorf("Kind = %q, want %q", req.Kind, kindQuestion)
	}
	if req.Header != "Toppings" {
		t.Errorf("Header = %q, want Toppings", req.Header)
	}
	if !req.MultiSelect {
		t.Error("MultiSelect = false on a checkbox dialog")
	}
	// Tab jumps the widget to its review pane; the composer's enhanced Enter
	// would act on the highlighted row instead.
	if string(req.SubmitKeys) != "\t" {
		t.Errorf("SubmitKeys = %q, want a Tab", req.SubmitKeys)
	}
	checkOptions(t, req.Options, []wantOption{
		{id: "1", alias: "", label: "Cheese", desc: "Add cheese as a topping.", keys: "1"},
		{id: "2", alias: "", label: "Mushrooms", desc: "Add mushrooms as a topping.", keys: "2"},
		{id: "3", alias: "", label: "Olives", desc: "Add olives as a topping.", keys: "3"},
		{id: "4", alias: "other", label: "Type something", desc: "", keys: "4"},
		{id: "5", alias: "", label: "Chat about this", desc: "", keys: "5"},
	})
}

// Toggling a checkbox repaints the row with a different marker. The marker is
// stripped before hashing, so the id must not move — otherwise every keystroke
// would look like a brand-new dialog to OnScreen.
func TestDetectQuestion_MultiSelectIDStableAcrossToggle(t *testing.T) {
	req, _ := DetectInput(multiSelectScreen)
	toggled, ok := DetectInput(strings.ReplaceAll(multiSelectScreen, "[✔]", "[ ]"))
	if !ok {
		t.Fatal("DetectInput did not recognize the toggled multi-select dialog")
	}
	if toggled.ID != req.ID {
		t.Errorf("ID moved when a checkbox toggled: %q vs %q", req.ID, toggled.ID)
	}
}

// Checkbox glyphs in a rendered to-do list, plus a numbered list right below
// them, must NOT be read as a dialog. The tab-strip line alone is never
// sufficient; the missing footer is what rejects this screen.
func TestDetectQuestion_TodoListDoesNotFire(t *testing.T) {
	if req, ok := DetectInput(todoReplyScreen); ok {
		t.Fatalf("DetectInput fired on a to-do list in a reply: %+v", req)
	}
}

// A pane whose anchor has painted but whose rows have not is worse than no
// detection at all: a policy could "answer" it with keystrokes aimed at rows
// the widget has not drawn yet. Every partial shape must return nil and be
// picked up on the next snapshot instead.
func TestDetectQuestion_PartiallyRenderedReturnsNil(t *testing.T) {
	for _, tc := range []struct {
		name   string
		screen string
	}{
		{
			// Footer not painted yet.
			"question pane without its footer",
			strings.Replace(singleQuestionScreen,
				"Enter to select · ↑/↓ to navigate · Esc to cancel", "", 1),
		},
		{
			// Footer painted, option rows not.
			"question pane without its options",
			" ☐ Color\n\nWhich color should I use?\n\nEnter to select · Esc to cancel\n",
		},
		{
			// Footer and options painted, question text not.
			"question pane without its question text",
			" ☐ Color\n\n❯ 1. Red\n  2. Blue\n\nEnter to select · Esc to cancel\n",
		},
		{
			// Review anchor painted, Submit/Cancel menu not.
			"review pane without its menu",
			"←  ☒ Color  ✔ Submit  →\n\nReview your answers\n\nReady to submit your answers?\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if req, ok := DetectInput(tc.screen); ok {
				t.Fatalf("DetectInput fired on a partially rendered pane: %+v", req)
			}
		})
	}
}

// The startup dialogs gate the session before any turn runs, so they take
// precedence — and a screen that somehow carried both must resolve to the
// trust prompt, never to the question dialog.
func TestDetectInput_TrustBeatsQuestion(t *testing.T) {
	both := trustScreen + "\n" + singleQuestionScreen
	req, ok := DetectInput(both)
	if !ok {
		t.Fatal("DetectInput found no dialog on a combined trust + question screen")
	}
	if req.Kind != "trust_prompt" {
		t.Errorf("Kind = %q, want trust_prompt (startup dialogs win)", req.Kind)
	}
	if req.Prompt != trustAnchor {
		t.Errorf("Prompt = %q, want %q", req.Prompt, trustAnchor)
	}
}

// The end-to-end pane walk: question 1 → question 2 → review → cleared. None
// of the intermediate transitions passes through a dialog-free frame, so each
// one must resolve the previous request BEFORE surfacing the next; otherwise a
// client is left waiting on an id that never resolves.
func TestOnScreen_QuestionPanesSupersedeEachOther(t *testing.T) {
	a := New()

	first := findKind(a.OnScreen(screen.Snapshot{Text: singleQuestionScreen}), turns.InputRequested)
	if first == nil {
		t.Fatal("no InputRequested emitted for the first question pane")
	}

	// Same pane re-renders → no duplicate request.
	if dup := findKind(a.OnScreen(screen.Snapshot{Text: singleQuestionScreen}), turns.InputRequested); dup != nil {
		t.Error("duplicate InputRequested emitted on redraw")
	}

	evs := a.OnScreen(screen.Snapshot{Text: secondQuestionScreen})
	resolved, requested := findKind(evs, turns.InputResolved), findKind(evs, turns.InputRequested)
	if resolved == nil || requested == nil {
		t.Fatalf("want both InputResolved and InputRequested on the pane switch, got %+v", evs)
	}
	if resolved.Input.ID != first.Input.ID {
		t.Errorf("InputResolved ID = %q, want the first pane's %q", resolved.Input.ID, first.Input.ID)
	}
	if requested.Input.Prompt != "Which size should I use?" {
		t.Errorf("InputRequested prompt = %q, want the second question", requested.Input.Prompt)
	}
	if indexOfKind(evs, turns.InputResolved) > indexOfKind(evs, turns.InputRequested) {
		t.Error("order matters: resolve the old request before surfacing the new one")
	}

	// Question 2 → review pane: another supersede, and a different kind.
	evs = a.OnScreen(screen.Snapshot{Text: reviewScreen})
	review := findKind(evs, turns.InputRequested)
	if review == nil || review.Input.Kind != kindQuestionReview {
		t.Fatalf("want an InputRequested for the review pane, got %+v", evs)
	}
	if r := findKind(evs, turns.InputResolved); r == nil || r.Input.ID != requested.Input.ID {
		t.Errorf("review pane did not resolve the second question first: %+v", evs)
	}

	// Dialog closes → the review request resolves against a dialog-free frame.
	done := findKind(a.OnScreen(screen.Snapshot{Text: "Claude Code\n❯ \n"}), turns.InputResolved)
	if done == nil {
		t.Fatal("no InputResolved emitted when the dialog cleared")
	}
	if done.Input.ID != review.Input.ID {
		t.Errorf("InputResolved ID = %q, want the review pane's %q", done.Input.ID, review.Input.ID)
	}
}

func indexOfKind(evs []turns.Event, k turns.Kind) int {
	for i := range evs {
		if evs[i].Kind == k {
			return i
		}
	}
	return -1
}
