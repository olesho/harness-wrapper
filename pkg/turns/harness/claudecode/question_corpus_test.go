package claudecode

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// The question recordings (test/corpus/claude-code/question-*): claude 2.1.283
// asking through AskUserQuestion, answered through chat.Conversation.Answer
// (internal/chatcore/question_record_test.go).
var questionCorpus = []struct {
	scenario string
	kinds    []string
	prompt   string // the first request's
}{
	{"question-single", []string{KindQuestion}, "Which colour do you prefer?"},
	{"question-other", []string{KindQuestion}, "Which colour do you prefer?"},
	{"question-chat", []string{KindQuestion}, "Which colour do you prefer?"},
	{"question-multi-select", []string{KindQuestion, KindQuestionReview}, "Which toppings do you want?"},
	{"question-multi-select-other", []string{KindQuestion, KindQuestionReview}, "Which toppings do you want?"},
	{"question-two", []string{KindQuestion, KindQuestion, KindQuestionReview}, "Which colour do you prefer?"},
}

// TestQuestionCorpus replays each recording a byte at a time, so the adapter
// reads every frame claude passes through, mid-paint ones included, some of
// which mix a pane with the one it replaces. Such a frame may raise a request
// of its own — answers are guarded against it, internal/chatcore writing keys
// only into the pane a plan was made for — but every pane's request must
// still come, in order, every request must be resolved, and nothing may be
// reported as an error.
func TestQuestionCorpus(t *testing.T) {
	for _, c := range questionCorpus {
		t.Run(c.scenario, func(t *testing.T) {
			t.Parallel()
			raw := corpusBytes(t, c.scenario)
			bytewise := make([][]byte, len(raw))
			for i := range raw {
				bytewise[i] = raw[i : i+1]
			}
			evs, last := replayChunks(t, c.scenario, bytewise)
			requested := eventsOfKind(evs, turns.InputRequested)
			if len(requested) == 0 || requested[0].Input.Prompt != c.prompt {
				t.Fatalf("first request %+v, want the prompt %q", requested, c.prompt)
			}
			want := c.kinds
			for _, ev := range requested {
				if len(want) > 0 && ev.Input.Kind == want[0] {
					want = want[1:]
				}
			}
			if len(want) > 0 {
				t.Fatalf("no request for the panes %v", want)
			}
			assertAllResolved(t, evs, last)
		})
	}
}

func assertAllResolved(t *testing.T, evs []turns.Event, last screen.Snapshot) {
	t.Helper()
	open := map[string]int{}
	for _, ev := range evs {
		switch ev.Kind {
		case turns.InputRequested:
			open[ev.Input.ID]++
		case turns.InputResolved:
			open[ev.Input.ID]--
		case turns.Errored:
			t.Errorf("Errored: %s", ev.Reason)
		}
	}
	for id, n := range open {
		if n != 0 {
			t.Errorf("request %s left unresolved", id)
		}
	}
	if req, det := DetectInputDetail(last.Text); det != DetectNone {
		t.Errorf("the last frame still reads as a dialog (%v): %+v", det, req)
	}
}

// replayChunks writes chunks to a fresh screen one at a time, as the wrapper
// does, and returns every event the adapter raised and the final frame.
func replayChunks(t *testing.T, scenario string, chunks [][]byte) ([]turns.Event, screen.Snapshot) {
	t.Helper()
	scr := screen.New(120, 40)
	a := New()
	var evs []turns.Event
	var last screen.Snapshot
	for _, c := range chunks {
		_, _ = scr.Write(c)
		last = scr.Snapshot()
		evs = append(evs, a.OnScreen(last)...)
	}
	if len(chunks) < 2 {
		t.Fatalf("%s: %d chunks", scenario, len(chunks))
	}
	return evs, last
}
