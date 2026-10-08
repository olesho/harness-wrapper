package codexturns

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
)

// What codex does with an input it folds into the turn it is on, at the
// edges: sent as that turn ends, into a turn that fails, and sent twice. The
// adapter sends an input a turn of codex's own dropped again under the same
// client id, so it has to know when codex might hold one still.

// agentDone waits for the turn's agentMessage item to complete after from —
// its model call's answer — and returns the index after it.
func (s *server) agentDone(t *testing.T, from int, turn string) int {
	t.Helper()
	for {
		n, next, ok := s.await("item/completed", from, 30*time.Second)
		if !ok {
			t.Fatalf("turn %s never completed an agent message", turn)
		}
		var p struct {
			TurnID string `json:"turnId"`
			Item   struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal(n.params, &p) == nil && p.Item.Type == "agentMessage" && p.TurnID == turn {
			return next
		}
		from = next
	}
}

// inputAsync starts a turn on text, and returns the channel its answer comes
// on.
func (s *server) inputAsync(thread, text, client string) chan json.RawMessage {
	return s.callAsync("turn/start", map[string]any{
		"threadId": thread, "input": []map[string]string{{"type": "text", "text": text}}, "clientUserMessageId": client,
	})
}

func answeredTurn(r json.RawMessage) string {
	var a struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(r, &a)
	return a.Turn.ID
}

// userMessages lists the turns of every user message codex announced with
// the client id, one per message.
func (s *server) userMessages(client string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var turns []string
	seen := map[string]bool{}
	for _, n := range s.notes {
		var p struct {
			TurnID string `json:"turnId"`
			Item   struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				ClientID string `json:"clientId"`
			} `json:"item"`
		}
		if n.method == "item/completed" && json.Unmarshal(n.params, &p) == nil && p.Item.Type == "userMessage" && p.Item.ClientID == client && !seen[p.Item.ID] {
			seen[p.Item.ID] = true
			turns = append(turns, p.TurnID)
		}
	}
	return turns
}

// takenIn waits up to d for codex to take the client's input in, and
// returns the turn that did; "" if none did.
func (s *server) takenIn(client string, d time.Duration) string {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if turn, ok := s.userMessage(client); ok {
			return turn
		}
	}
	return ""
}

// requested reports how many model requests came for the scenario.
func requested(mock *mockapi.Server, scenario string) int {
	n := 0
	for _, r := range mock.Requests() {
		if r.Scenario == scenario {
			n++
		}
	}
	return n
}

// An input sent as an input's turn ends — swept, a millisecond at a time,
// across the instant the turn's last answer gives way to its end — is taken
// in by that turn, or starts a turn of its own: never held past the turn's
// end. codex answers turn/start with the turn that will take the input in.
func TestInputAtATurnsEnd(t *testing.T) {
	_, s, _, l := setup(t)
	id := newThread(s, l)
	ping(t, s, id, "PING one", "client-1")
	seen := map[string]int{}
	for i := range 30 {
		m := s.mark()
		running := s.input(id, "SLOW 2", fmt.Sprintf("running-%d", i))
		m = s.agentDone(t, m, running)
		time.Sleep(time.Duration(i) * time.Millisecond)
		client := fmt.Sprintf("edge-%d", i)
		ch := s.inputAsync(id, fmt.Sprintf("PING edge %d", i), client)
		s.completed(t, m, running)
		answered := answeredTurn(<-ch)
		took := s.takenIn(client, 10*time.Second)
		switch {
		case answered == running && took == running:
			seen["taken in by the running turn"]++
		case answered != running && took == answered:
			seen["a turn of its own"]++
			s.completed(t, m, took)
		default:
			t.Errorf("+%dms: turn/start answered %s; the running turn %s ended, and the input was taken in by %q", i, answered, running, took)
		}
	}
	t.Logf("where an input sent at a turn's end went: %v", seen)
	if len(seen) < 2 {
		t.Error("the sweep did not cross the turn's end: widen it")
	}
}

// The same, across the end of a goal's turns: an input codex answers with
// the goal turn's id is taken in by it. One sent after it ended may land in
// the goal's next turn instead, or a turn of its own; codex answers with
// that turn.
func TestInputAtAGoalTurnsEnd(t *testing.T) {
	_, s, _, l := setup(t)
	id := newThread(s, l)
	ping(t, s, id, "PING one", "client-1")
	m := s.mark()
	s.call("thread/goal/set", map[string]any{"threadId": id, "objective": "GOAL 99 2", "status": "active"})
	seen := map[string]int{}
	inputs := map[string]bool{}
	for i := 0; i < 30; {
		running, next := s.started(t, m, "a turn")
		if inputs[running] {
			// An input's turn of its own, from a round before.
			_, m = s.completed(t, next, running)
			continue
		}
		m = s.agentDone(t, next, running)
		time.Sleep(time.Duration(i) * time.Millisecond)
		client := fmt.Sprintf("edge-%d", i)
		ch := s.inputAsync(id, fmt.Sprintf("PING edge %d", i), client)
		_, end := s.completed(t, m, running)
		answered := answeredTurn(<-ch)
		took := s.takenIn(client, 10*time.Second)
		switch {
		case answered == running && took == running:
			seen["taken in by the running goal turn"]++
		case answered != running && took == answered:
			seen["taken in by the turn after"]++
			inputs[answered] = true
		default:
			t.Errorf("+%dms: turn/start answered %s; the goal turn %s ended, and the input was taken in by %q", i, answered, running, took)
		}
		m = end
		i++
	}
	s.call("thread/goal/set", map[string]any{"threadId": id, "status": "paused"})
	t.Logf("where an input sent at a goal turn's end went: %v", seen)
	if len(seen) < 2 {
		t.Error("the sweep did not cross the goal turn's end: widen it")
	}
}

// An input codex holds for a turn whose model call fails past its retries is
// put in that turn as it fails: its user message is the failed turn's, and
// the model never had it. No turn starts for it after.
func TestInputIntoAFailingTurn(t *testing.T) {
	mock, s, _, l := setup(t)
	id := newThread(s, l)
	ping(t, s, id, "PING one", "client-1")
	m := s.mark()
	failing := s.input(id, "FAILSLOW 2", "client-fail")
	time.Sleep(500 * time.Millisecond)
	if answered := s.input(id, "PING held", "client-held"); answered != failing {
		t.Fatalf("turn/start during the failing turn %s answered %s", failing, answered)
	}
	if status, _ := s.completed(t, m, failing); status != "failed" {
		t.Fatalf("the failing turn ended %s", status)
	}
	if turns := s.userMessages("client-held"); len(turns) != 1 || turns[0] != failing {
		t.Errorf("the held input's user messages are in turns %v, want one, in the failed turn %s", turns, failing)
	}
	if n := requested(mock, "PING held"); n != 0 {
		t.Errorf("the model had the held input %d times", n)
	}
	if !s.quiet(s.mark(), 3*time.Second) {
		t.Error("a turn started after the failed turn")
	}
}

// codex keeps no record of a client id: the same input sent twice under one
// is taken in twice, and one whose turn ran, sent again, runs again.
func TestClientIDIsNoKey(t *testing.T) {
	mock, s, _, l := setup(t)
	id := newThread(s, l)
	ping(t, s, id, "PING one", "client-1")
	m := s.mark()
	s.call("thread/goal/set", map[string]any{"threadId": id, "objective": "GOAL 9 10", "status": "active"})
	own, m := s.started(t, m, "the goal's turn")
	s.input(id, "PING twice", "client-twice")
	s.input(id, "PING twice", "client-twice")
	s.completed(t, m, own)
	if turns := s.userMessages("client-twice"); len(turns) != 2 {
		t.Errorf("an input sent twice under one client id has user messages in turns %v, want two", turns)
	}
	s.call("thread/goal/set", map[string]any{"threadId": id, "status": "paused"})
	time.Sleep(2 * time.Second)

	ping(t, s, id, "PING again", "client-again")
	m = s.mark()
	again := s.input(id, "PING again", "client-again")
	if again == "" {
		t.Fatal("codex refused a client id whose turn ran")
	}
	if status, _ := s.completed(t, m, again); status != "completed" {
		t.Errorf("the input sent again ended %s", status)
	}
	if turns := s.userMessages("client-again"); len(turns) != 2 || turns[0] == turns[1] {
		t.Errorf("an input whose turn ran, sent again, has user messages in turns %v, want two turns", turns)
	}
	if n := requested(mock, "PING again"); n != 2 {
		t.Errorf("the model had the input sent again %d times, want 2", n)
	}
}
