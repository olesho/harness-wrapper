package claudecodetui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

func askEvent(id string, pid int) live.Event {
	return live.Event{Hook: live.HookPermission, SessionID: sid, PromptID: prompt, Ask: &live.Ask{
		ID: id, Tool: "Bash", Input: json.RawMessage(`{"command":"touch ` + id + `"}`), PID: pid,
	}}
}

func decision(t *testing.T, a askAnswer) map[string]any {
	t.Helper()
	var d map[string]any
	if len(a.answer.Decision) > 0 {
		if err := json.Unmarshal(a.answer.Decision, &d); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func codeOf(err error) contract.Code {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		return ""
	}
	return ce.Code
}

// The hook's asks are raised one at a time in the input's turn; an answer
// resolves the prompt, raises the next, and is written for the hook; claude
// taking it (the debug log) clears it; the turn's end withdraws what is left.
func TestTrackerAsks(t *testing.T) {
	k := started(t)
	k.alive = func(int) bool { return true }
	k.debug(line("[engine] turn 1 start"), t0)
	s := k.live(askEvent("ask-1", 11), t0)
	if len(s.events) != 1 || s.events[0].Kind != adapter.PromptRaised || s.events[0].Native != native || s.events[0].Prompt.PromptID != "ask-1" ||
		!strings.Contains(s.events[0].Prompt.Prompt, "touch ask-1") {
		t.Fatalf("the first ask: %+v", s)
	}
	if s := k.live(askEvent("ask-2", 12), t0); len(s.events) != 0 {
		t.Fatalf("a second ask while one is raised: %+v", s.events)
	}
	if _, err := k.answer("ask-2", contract.Choice{OptionID: claudecode.OptionYes}, t0); codeOf(err) != contract.CodePromptGone {
		t.Errorf("answering one not raised: %v", err)
	}
	s, err := k.answer("ask-1", contract.Choice{OptionID: claudecode.OptionNo}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(s); len(got) != 2 || got[0] != adapter.PromptResolved || s.events[0].By != "answer" || got[1] != adapter.PromptRaised || s.events[1].Prompt.PromptID != "ask-2" {
		t.Fatalf("after the answer: %+v", s.events)
	}
	if len(s.answers) != 1 || s.answers[0].id != "ask-1" || decision(t, s.answers[0])["behavior"] != "deny" {
		t.Fatalf("the answer written: %+v", s.answers)
	}
	if _, ok := decision(t, s.answers[0])["decisionClassification"]; ok {
		t.Error("a hook's decision carries decisionClassification")
	}
	// The debug log may come before the hook's word that it handed it over.
	k.debug(line(`Hook PermissionRequest (sh -c 'x') returned permissionDecision: deny`), t0)
	k.live(live.Event{Hook: live.HookDelivered, Ask: &live.Ask{ID: "ask-1"}}, t0)
	if len(k.sent) != 0 {
		t.Errorf("an answer claude took is still awaited: %d", len(k.sent))
	}
	if s := k.tick(t0.Add(time.Minute)); s.esc != nil || len(s.events) != 0 {
		t.Errorf("a minute on, with ask-2 waiting on a live hook: %+v", s)
	}
	e := ended(t, k.live(live.Event{Hook: live.HookStop, SessionID: sid, PromptID: prompt, Message: "done"}, t0))
	if e.Outcome != contract.TurnCompleted {
		t.Errorf("end %+v", e)
	}
	if len(k.asks) != 0 {
		t.Errorf("asks left after the turn: %d", len(k.asks))
	}
}

// An ask on no input's turn is denied at once; the turn's end withdraws the
// asks left, which hands their hooks nothing.
func TestTrackerAskOutsideATurn(t *testing.T) {
	k := newTracker(sid)
	s := k.live(askEvent("ask-0", 10), t0)
	if len(s.events) != 0 || len(s.answers) != 1 || decision(t, s.answers[0])["behavior"] != "deny" {
		t.Fatalf("an ask on no turn: %+v", s)
	}
	k = started(t)
	k.alive = func(int) bool { return true }
	k.debug(line("[engine] turn 1 start"), t0)
	k.live(askEvent("ask-1", 11), t0)
	s = k.live(live.Event{Hook: live.HookStop, SessionID: sid, PromptID: prompt, Message: "done"}, t0)
	if len(s.answers) != 1 || s.answers[0].id != "ask-1" || len(s.answers[0].answer.Decision) != 0 {
		t.Fatalf("the turn's end: answers %+v", s.answers)
	}
}

// Interrupt while a prompt waits is Esc on claude's dialog: no [onCancel],
// the turn's end with stop=tool_use is the interrupt, and the hook claude
// cancels is not a lost prompt.
func TestTrackerEscOnAPrompt(t *testing.T) {
	k := started(t)
	alive := true
	k.alive = func(int) bool { return alive }
	k.debug(line("[engine] turn 1 start"), t0)
	k.live(askEvent("ask-1", 11), t0)
	k.interrupting()
	alive = false
	if s := k.tick(t0.Add(time.Second)); s.esc != nil || len(s.events) != 0 {
		t.Errorf("claude cancelled the hook of a prompt Esc stops: %+v", s)
	}
	e := ended(t, k.debug(line("[engine] turn 1 end (turns=3 usage in=50 out=25 cost=$0.0004 api=85ms stop=tool_use resultLen=0)"), t0))
	if e.Outcome != contract.TurnInterrupted {
		t.Errorf("end %+v, want interrupted", e)
	}
}

// A lost prompt — the hook gone before it answered, or an answer claude did
// not take — is resolved by the harness, and Esc pressed: the turn ends
// errored when claude ends it, or after lostWait, with claude stopped.
func TestTrackerLostPrompt(t *testing.T) {
	for _, c := range []struct {
		name string
		lose func(k *tracker, alive *bool) step
	}{
		{"hook gone", func(k *tracker, alive *bool) step {
			*alive = false
			return k.tick(t0.Add(time.Second))
		}},
		{"answer not taken", func(k *tracker, _ *bool) step {
			if _, err := k.answer("ask-1", contract.Choice{OptionID: claudecode.OptionYes}, t0); err != nil {
				t.Fatal(err)
			}
			k.live(live.Event{Hook: live.HookDelivered, Ask: &live.Ask{ID: "ask-1"}}, t0)
			if s := k.tick(t0.Add(decisionWait - time.Second)); s.esc != nil {
				t.Fatal("lost before decisionWait")
			}
			return k.tick(t0.Add(decisionWait))
		}},
		{"answer never handed over", func(k *tracker, _ *bool) step {
			if _, err := k.answer("ask-1", contract.Choice{OptionID: claudecode.OptionYes}, t0); err != nil {
				t.Fatal(err)
			}
			return k.tick(t0.Add(decisionWait))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := started(t)
			alive := true
			k.alive = func(int) bool { return alive }
			k.debug(line("[engine] turn 1 start"), t0)
			k.live(askEvent("ask-1", 11), t0)
			s := c.lose(k, &alive)
			if s.esc == nil || s.note == "" {
				t.Fatalf("no Esc for a lost prompt: %+v", s)
			}
			if c.name == "hook gone" && (len(s.events) != 1 || s.events[0].Kind != adapter.PromptResolved || s.events[0].By != "harness") {
				t.Errorf("the prompt: %+v, want resolved by the harness", s.events)
			}
			e := ended(t, k.debug(line("[engine] turn 1 end (turns=3 usage in=50 out=25 cost=$0.0004 api=85ms stop=tool_use resultLen=0)"), t0))
			if e.Outcome != contract.TurnErrored || e.Error == nil || e.Error.Class != contract.ErrorInternal {
				t.Errorf("end %+v, want errored (internal)", e)
			}
		})
	}
	k := started(t)
	alive := true
	k.alive = func(int) bool { return alive }
	k.debug(line("[engine] turn 1 start"), t0)
	k.live(askEvent("ask-1", 11), t0)
	alive = false
	k.tick(t0.Add(time.Second))
	s := k.tick(t0.Add(time.Second + lostWait))
	if s.fatal == "" || len(s.events) != 1 || s.events[0].Outcome != contract.TurnErrored {
		t.Errorf("Esc not taken: %+v, want the turn errored and claude stopped", s)
	}
}
