package claudecodetui

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

const (
	ownPrompt = "138161d3-0e83-436b-972e-3b41f400b5de"
	notice    = "<task-notification>\n<task-id>bdq37quon</task-id>\n<status>completed</status>\n</task-notification>"
)

func turnEnd(stop string) debugLine {
	return line("[engine] turn 1 end (turns=1 usage in=10 out=5 cost=$0.0001 api=47ms stop=" + stop + " resultLen=6)")
}

// kinds lists a step's events' kinds.
func kinds(s step) []adapter.EventKind {
	var out []adapter.EventKind
	for _, e := range s.events {
		out = append(out, e.Kind)
	}
	return out
}

// backgroundOf is the work the Background event of s lists.
func backgroundOf(t *testing.T, s step) []contract.BackgroundTask {
	t.Helper()
	for _, e := range s.events {
		if e.Kind == adapter.Background {
			return e.Tasks
		}
	}
	t.Fatalf("no Background event in %+v", s)
	return nil
}

// background work started by an input's turn, as probes/tui-hybrid found it
// (2026-10-09): the input's Stop lists the command running; when it ends,
// claude logs a turn's start, fires UserPromptSubmit with a task notification
// as its prompt, and ends that turn with its own Stop. The work is reported
// running, then ended; the turn is claude's own, named as the record names
// it, and the next input waits for its end.
func TestTrackerBackgroundTurn(t *testing.T) {
	k := started(t)
	k.debug(line("[engine] turn 1 start"), t0)
	s := k.live(live.Event{
		Hook: live.HookStop, PromptID: prompt, Message: "TOOL DONE: ", Listed: true,
		Background: []live.Task{{ID: "bdq37quon", Type: "shell", Description: "mock"}},
	}, t0)
	if got := kinds(s); len(got) != 2 || got[0] != adapter.Background || got[1] != adapter.Ended {
		t.Fatalf("the input's Stop: %+v", s)
	}
	if b := backgroundOf(t, s); len(b) != 1 || b[0].ID != "bdq37quon" || b[0].Kind != contract.BackgroundCommand || b[0].Description != "mock" {
		t.Errorf("background %+v", b)
	}
	k.debug(turnEnd("end_turn"), t0)
	if !k.settled() {
		t.Fatal("not settled after the input's turn")
	}

	// claude starts its own turn: the debug log first.
	k.debug(line("[engine] turn 2 start"), t0)
	if k.settled() {
		t.Error("settled while claude's own turn runs")
	}
	s = k.live(live.Event{Hook: live.HookUserPrompt, PromptID: ownPrompt, Prompt: notice}, t0)
	if b := backgroundOf(t, s); len(b) != 0 {
		t.Errorf("the task ended, and background %+v", b)
	}
	var st adapter.Event
	for _, e := range s.events {
		if e.Kind == adapter.Started {
			st = e
		}
	}
	if st.Auto != "task-bdq37quon" || st.Native != "" {
		t.Fatalf("claude's own turn started %+v", st)
	}
	// Its Stop ends it, and lists no work: nothing changed.
	s = k.live(live.Event{Hook: live.HookStop, PromptID: ownPrompt, Message: "BG DONE", Listed: true}, t0)
	if len(s.events) != 1 || s.events[0].Kind != adapter.Ended || s.events[0].Auto != "task-bdq37quon" ||
		s.events[0].Outcome != contract.TurnCompleted || s.events[0].Text != "BG DONE" {
		t.Fatalf("claude's own Stop: %+v", s)
	}
	if k.settled() {
		t.Error("settled before the debug log logged the end of claude's own turn")
	}
	k.debug(line("[engine] turn 2 end (turns=1 usage in=10 out=5 cost=$0.0001 api=47ms stop=end_turn resultLen=7)"), t0)
	if !k.settled() {
		t.Error("not settled after claude's own turn")
	}
}

// When the hooks of the input's end and of claude's own turn are read before
// the debug log's lines, the first turn end logged is still the input's.
func TestTrackerOwnTurnBeforeLines(t *testing.T) {
	k := started(t)
	k.debug(line("[engine] turn 1 start"), t0)
	ended(t, k.live(live.Event{Hook: live.HookStop, PromptID: prompt, Message: "TOOL DONE"}, t0))
	if s := k.live(live.Event{Hook: live.HookUserPrompt, PromptID: ownPrompt, Prompt: notice}, t0); len(s.events) != 1 || s.events[0].Auto == "" {
		t.Fatalf("claude's own turn: %+v", s)
	}
	k.debug(turnEnd("end_turn"), t0)
	if !k.turn.endLine || k.own.endLine {
		t.Fatalf("the input's end line went to claude's own turn: input %v own %v", k.turn.endLine, k.own.endLine)
	}
	k.debug(line("[engine] turn 2 start"), t0)
	k.live(live.Event{Hook: live.HookStop, PromptID: ownPrompt, Message: "BG DONE"}, t0)
	k.debug(line("[engine] turn 2 end (turns=1 usage in=10 out=5 cost=$0.0001 api=47ms stop=end_turn resultLen=7)"), t0)
	if !k.settled() {
		t.Error("not settled after both turns")
	}
}

// A prompt claude submits itself that is no task notification — the
// automatic continue after a usage limit — is a turn of claude's own too,
// named after its prompt id. A prompt claude queued, or one while an input's
// turn runs, starts none.
func TestTrackerOtherOwnTurns(t *testing.T) {
	k := newTracker(sid)
	k.live(live.Event{Hook: live.HookSessionStart, SessionID: sid}, t0)
	s := k.live(live.Event{Hook: live.HookUserPrompt, PromptID: ownPrompt, Prompt: "continue"}, t0)
	if len(s.events) != 1 || s.events[0].Kind != adapter.Started || s.events[0].Auto != "prompt-"+ownPrompt {
		t.Fatalf("the automatic continue: %+v", s)
	}
	// A second unbound prompt while it runs starts nothing.
	if s := k.live(live.Event{Hook: live.HookUserPrompt, PromptID: "other", Prompt: "x"}, t0); len(s.events) != 0 {
		t.Errorf("a prompt during claude's own turn: %+v", s)
	}
	s = k.live(live.Event{Hook: live.HookStopFailure, PromptID: ownPrompt, Error: "rate_limit", Message: "You've hit your limit"}, t0)
	if len(s.events) != 1 || s.events[0].Auto != "prompt-"+ownPrompt || s.events[0].Outcome != contract.TurnErrored {
		t.Fatalf("its end: %+v", s)
	}

	k = started(t)
	if s := k.live(live.Event{Hook: live.HookUserPrompt, PromptID: prompt, Prompt: "queued", Queued: true}, t0); len(s.events) != 0 {
		t.Errorf("a queued prompt: %+v", s)
	}
	if s := k.live(live.Event{Hook: live.HookUserPrompt, PromptID: "p-x", Prompt: notice}, t0); len(s.events) != 0 {
		t.Errorf("a notification while the input's turn runs: %+v", s)
	}
}

// An interrupt of claude's own turn ends it as an input's: [onCancel], then a
// turn end with an interrupt's stop reason.
func TestTrackerOwnTurnInterrupted(t *testing.T) {
	k := newTracker(sid)
	k.debug(line("[engine] turn 1 start"), t0)
	k.live(live.Event{Hook: live.HookUserPrompt, PromptID: ownPrompt, Prompt: notice}, t0)
	k.debug(line("[onCancel] source=local streamMode=responding"), t0)
	s := k.debug(turnEnd("null"), t0)
	if len(s.events) != 1 || s.events[0].Auto != "task-bdq37quon" || s.events[0].Outcome != contract.TurnInterrupted {
		t.Fatalf("interrupted: %+v", s)
	}
	if !k.settled() {
		t.Error("not settled after the interrupt")
	}
}

// The background work is reported when it changes, and only then: a
// subagent is a subagent, and a notification with no task id takes nothing
// off.
func TestTrackerBackgroundList(t *testing.T) {
	k := newTracker(sid)
	two := []live.Task{{ID: "b1", Type: "shell"}, {ID: "a1", Type: "subagent", Description: "review"}}
	b := backgroundOf(t, k.live(live.Event{Hook: live.HookStop, Listed: true, Background: two}, t0))
	if len(b) != 2 || b[1].Kind != contract.BackgroundSubagent {
		t.Errorf("background %+v", b)
	}
	if s := k.live(live.Event{Hook: live.HookStop, Listed: true, Background: two}, t0); len(s.events) != 0 {
		t.Errorf("the same list again: %+v", s)
	}
	if s := k.live(live.Event{Hook: live.HookStop}, t0); len(s.events) != 0 {
		t.Errorf("a Stop that lists nothing: %+v", s)
	}
	s := k.live(live.Event{Hook: live.HookUserPrompt, PromptID: "p1", Prompt: "<task-notification><summary>x</summary>"}, t0)
	for _, e := range s.events {
		if e.Kind == adapter.Background {
			t.Errorf("a notification naming no task: %+v", e)
		}
	}
	k.own = nil
	s = k.live(live.Event{Hook: live.HookUserPrompt, PromptID: "p2", Prompt: "<task-notification><task-id>a1</task-id>"}, t0)
	if b := backgroundOf(t, s); len(b) != 1 || b[0].ID != "b1" {
		t.Errorf("after a1 ended: %+v", b)
	}
	if b := backgroundOf(t, k.live(live.Event{Hook: live.HookStop, Listed: true}, t0)); len(b) != 0 {
		t.Errorf("an empty list: %+v", b)
	}
}
