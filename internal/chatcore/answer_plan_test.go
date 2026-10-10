package chatcore

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// Claude Code 2.1.283's single-select AskUserQuestion pane, as the screen
// driver read it live: the highlight on its first row, then on the text field,
// then the field with an answer typed in; and the screen once it is answered,
// whose conversation still shows the question.
var (
	questionRule = strings.Repeat("─", 120)

	questionFrameFirst = questionFrame("❯ 1. Red", "  3. Type something.")
	questionFrameField = questionFrame("  1. Red", "❯ 3. Type something.")
	questionFrameTyped = questionFrame("  1. Red", "❯ 3. Green")

	questionAnsweredFrame = strings.Join([]string{
		"❯ HW_ASK: ask me",
		"⏺ User answered Claude's questions:",
		"  ⎿  · Which colour do you prefer? → Green",
		"⏺ GOT_ANSWER",
		questionRule,
		"❯ ",
		questionRule,
	}, "\r\n") + "\r\n"
)

func questionFrame(first, field string) string {
	return strings.Join([]string{
		"❯ HW_ASK: ask me",
		questionRule,
		" ☐ Colour",
		"",
		"Which colour do you prefer?",
		"",
		first,
		"     You prefer red",
		"  2. Blue",
		"     You prefer blue",
		field,
		questionRule,
		"  4. Chat about this",
		"",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
	}, "\r\n") + "\r\n"
}

func questionRequest(t *testing.T) *turns.InputRequest {
	t.Helper()
	req, ok := claudecode.DetectInput(questionFrameFirst)
	if !ok || req.Kind != claudecode.KindQuestion {
		t.Fatalf("fixture no longer detects as a question: %+v", req)
	}
	return req
}

func planFor(t *testing.T, req *turns.InputRequest, ids []string, text string) []turns.AnswerStep {
	t.Helper()
	steps, ok, err := claudecode.New().PlanAnswer(req, ids, text)
	if err != nil || !ok {
		t.Fatalf("PlanAnswer = ok %v, %v", ok, err)
	}
	return steps
}

// Each step is written only once the screen shows the previous one landed.
// The repaints are deliberately LATE, so a passing run proves the waits
// rather than a lucky order — the order matters: "3Green\r" written in one go
// reaches claude as a paste, and answered "Red".
func TestRunAnswerPlan_WritesEachStepAfterThePreviousLanded(t *testing.T) {
	var landed []time.Time
	late := func(f *answerFake, frame string) {
		go func() {
			time.Sleep(60 * time.Millisecond)
			f.paint(frame)
			f.mu.Lock()
			landed = append(landed, time.Now())
			f.mu.Unlock()
		}()
	}
	f := newAnswerFake(t, 2*time.Second, questionFrameFirst, func(f *answerFake, p []byte) {
		switch string(p) {
		case "3":
			late(f, questionFrameField)
		case "Green":
			late(f, questionFrameTyped)
		case "\r":
			late(f, questionAnsweredFrame)
		}
	})
	req := questionRequest(t)

	if err := f.c.runAnswerPlan(context.Background(), req, planFor(t, req, []string{"3"}, "Green")); err != nil {
		t.Fatalf("runAnswerPlan: %v", err)
	}
	if got := strings.Join(f.writtenStrings(), "|"); got != "3|Green|\r" {
		t.Fatalf("writes = %q, want the digit, the text and Enter as three writes", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 1; i < len(f.stamps); i++ {
		if f.stamps[i].Before(landed[i-1]) {
			t.Fatalf("write %d at %v, before step %d landed at %v", i, f.stamps[i], i-1, landed[i-1])
		}
	}
}

// No evidence, no next key: the highlight never reaches the text field, so
// the text is never typed — into whichever row does have it — and the caller
// gets the bounded error.
func TestRunAnswerPlan_StopsWhenAStepDoesNotLand(t *testing.T) {
	f := newAnswerFake(t, 80*time.Millisecond, questionFrameFirst, nil)
	req := questionRequest(t)

	err := f.c.runAnswerPlan(context.Background(), req, planFor(t, req, []string{"3"}, "Green"))
	var unresolved *InputUnresolvedError
	if !errors.As(err, &unresolved) || unresolved.Attempts != 1 {
		t.Fatalf("err = %v, want an *InputUnresolvedError after 1 step", err)
	}
	if got := f.writtenStrings(); len(got) != 1 || got[0] != "3" {
		t.Fatalf("writes = %q, want only the first step", got)
	}
}

// An answered question stays in the conversation — "· Which colour do you
// prefer? → Green" — so its prompt on screen is no sign the dialog is still
// up. Gone is read from the dialog's parse and its anchors.
func TestRunAnswerPlan_AnsweredWhileThePromptStaysInTheConversation(t *testing.T) {
	f := newAnswerFake(t, 2*time.Second, questionFrameFirst, func(f *answerFake, p []byte) {
		if bytes.Equal(p, []byte("2")) {
			f.paint(questionAnsweredFrame)
		}
	})
	req := questionRequest(t)

	if err := f.c.runAnswerPlan(context.Background(), req, planFor(t, req, []string{"2"}, "")); err != nil {
		t.Fatalf("runAnswerPlan: %v", err)
	}
}

// The first key waits until the pane has been up for the plan's settle,
// counted from when the request was raised: 2.1.283 drops a key written as
// its pane first paints. A request raised long enough ago is answered at once.
func TestRunAnswerPlan_FirstKeyWaitsForThePaneToSettle(t *testing.T) {
	for name, age := range map[string]time.Duration{"just raised": 0, "raised a second ago": time.Second} {
		t.Run(name, func(t *testing.T) {
			f := newAnswerFake(t, 2*time.Second, questionFrameFirst, func(f *answerFake, p []byte) {
				if string(p) == "2" {
					f.paint(questionAnsweredFrame)
				}
			})
			req := questionRequest(t)
			steps := planFor(t, req, []string{"2"}, "")
			raised := time.Now().Add(-age)
			f.c.mu.Lock()
			f.c.currentInput, f.c.currentInputAt = req, raised
			f.c.mu.Unlock()
			start := time.Now()

			if err := f.c.runAnswerPlan(context.Background(), req, steps); err != nil {
				t.Fatalf("runAnswerPlan: %v", err)
			}
			f.mu.Lock()
			first := f.stamps[0]
			f.mu.Unlock()
			if up := first.Sub(raised); up < steps[0].After {
				t.Fatalf("first key written %v after the request was raised, want at least %v", up, steps[0].After)
			}
			if age > steps[0].After && first.Sub(start) > 100*time.Millisecond {
				t.Fatalf("a request up for %v waited %v more", age, first.Sub(start))
			}
		})
	}
}

// Keys go only to the dialog a plan was made for. Here the pane on screen is
// another one — a request read from a frame claude was still painting, or
// one already answered — and it gets nothing.
func TestRunAnswerPlan_WritesNothingToAnotherDialog(t *testing.T) {
	req := questionRequest(t)
	f := newAnswerFake(t, 80*time.Millisecond, questionFrame("❯ 1. Small", "  3. Type something."), nil)

	err := f.c.runAnswerPlan(context.Background(), req, planFor(t, req, []string{"2"}, ""))
	var unresolved *InputUnresolvedError
	if !errors.As(err, &unresolved) || unresolved.Attempts != 0 {
		t.Fatalf("err = %v, want an *InputUnresolvedError with nothing written", err)
	}
	if n := len(f.written()); n != 0 {
		t.Fatalf("wrote %q into another dialog", f.writtenStrings())
	}
}

// Answer reaches the planner: text alone goes to the "other" option, and an
// answer the dialog cannot take is refused before anything is written.
func TestWriteAnswer_PlansAQuestion(t *testing.T) {
	f := newAnswerFake(t, 2*time.Second, questionFrameFirst, func(f *answerFake, p []byte) {
		switch string(p) {
		case "3":
			f.paint(questionFrameField)
		case "Green":
			f.paint(questionFrameTyped)
		case "\r":
			f.paint(questionAnsweredFrame)
		}
	})
	req := questionRequest(t)

	if err := f.c.writeAnswer(context.Background(), req, InputAnswer{OptionID: "2", Text: "Green"}); !errors.Is(err, turns.ErrInvalidAnswer) {
		t.Fatalf("text with an ordinary option: err = %v, want ErrInvalidAnswer", err)
	}
	if n := len(f.written()); n != 0 {
		t.Fatalf("a refused answer wrote %d times", n)
	}
	if err := f.c.writeAnswer(context.Background(), req, InputAnswer{Text: "Green"}); err != nil {
		t.Fatalf("writeAnswer: %v", err)
	}
	if got := strings.Join(f.writtenStrings(), "|"); got != "3|Green|\r" {
		t.Fatalf("writes = %q, want the text field's three steps", got)
	}
}
