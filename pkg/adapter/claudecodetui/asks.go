package claudecodetui

import (
	"encoding/json"
	"errors"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// At the gated posture each permission claude asks for comes from its
// PermissionRequest hook, which blocks until the transport answers
// (live.HandlePermission). The tracker raises one at a time as the Session's
// prompt, in the turn of the input claude is on, named by the hook's ask id;
// Answer resolves it and writes the answer the hook hands claude. A turn's
// end withdraws the asks left, so their hooks end. A permission asked on no
// input's turn — a turn of claude's own — is denied at once, as nothing can
// answer it there.
//
// claude's own dialog is the fallback when the hook gives no answer, and only
// keystrokes answer it. The profile never answers it. It watches instead for
// the answer path breaking: the hook's process gone before it handed claude
// an answer (killed, or timed out), or an answer handed over that claude does
// not take — its debug log logs no permission decision from the hook
// (decisionWait). Either is a lost prompt: it is resolved by the harness, Esc
// stops the turn, which ends errored (internal), and if claude does not end
// it within lostWait the turn ends errored anyway and claude is stopped.

const (
	// decisionWait bounds how long claude may take to take an answer: from
	// the answer, until the debug log logs the hook's decision.
	decisionWait = 30 * time.Second
	// aliveEvery is how often the hooks waiting are checked for.
	aliveEvery = 500 * time.Millisecond
	// lostWait bounds how long a turn whose prompt was lost may take to end
	// once Esc is pressed.
	lostWait = 15 * time.Second
)

// pendingAsk is one permission the hook asked for.
type pendingAsk struct {
	ask    live.Ask
	perm   claudecode.Permission
	flight *flight
	raised bool
	// answered is when the transport answered it; delivered, that the hook
	// handed claude the answer.
	answered  time.Time
	delivered bool
	checked   time.Time
}

// processAlive reports whether the process pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return true // unknown: never taken for gone
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ask takes the hook's PermissionRequest.
func (k *tracker) ask(ev live.Event, now time.Time) step {
	a := ev.Ask
	if a == nil || a.ID == "" {
		return step{}
	}
	f := k.turn
	if f == nil || !f.started || f.ended || f.lost {
		b, _ := json.Marshal(claudecode.NoOneToAsk())
		return step{answers: []askAnswer{{id: a.ID, answer: live.Answer{Decision: b}}}}
	}
	p := &pendingAsk{
		ask: *a, flight: f, checked: now,
		perm: claudecode.Permission{Tool: a.Tool, Input: a.Input, Suggestions: a.Suggestions},
	}
	k.asks = append(k.asks, p)
	return k.raiseNext(now)
}

// raiseNext raises the first permission waiting, unless it is raised.
func (k *tracker) raiseNext(now time.Time) step {
	if len(k.asks) == 0 || k.asks[0].raised {
		return step{}
	}
	p := k.asks[0]
	p.raised = true
	e := p.flight.event(adapter.PromptRaised, now)
	pi := p.perm.Prompt(p.ask.ID)
	e.Prompt = &pi
	return step{events: []adapter.Event{e}}
}

// answer answers the raised permission promptID with c: the prompt resolved,
// the next one raised, and the answer to write for the hook.
func (k *tracker) answer(promptID string, c contract.Choice, now time.Time) (step, error) {
	if len(k.asks) == 0 || !k.asks[0].raised || k.asks[0].ask.ID != promptID {
		return step{}, contract.Errorf(contract.CodePromptGone, "claude no longer waits on %q", promptID)
	}
	p := k.asks[0]
	k.asks = k.asks[1:]
	p.answered = now
	k.sent = append(k.sent, p)
	b, err := json.Marshal(p.perm.Decision(c, false))
	if err != nil {
		return step{}, err
	}
	e := p.flight.event(adapter.PromptResolved, now)
	e.PromptID, e.By = promptID, "answer"
	s := step{events: []adapter.Event{e}, answers: []askAnswer{{id: promptID, answer: live.Answer{Decision: b}}}}
	s.add(k.raiseNext(now))
	return s, nil
}

// delivered takes the hook's word that it handed claude the answer.
func (k *tracker) delivered(ev live.Event) {
	if ev.Ask == nil {
		return
	}
	for _, p := range k.sent {
		if p.ask.ID == ev.Ask.ID {
			p.delivered = true
		}
	}
}

// decided takes the debug log's word that claude took a hook's decision: the
// oldest answer sent. The log may be read before the hook's word that it
// handed the answer over, which it writes first.
func (k *tracker) decided() {
	if len(k.sent) > 0 {
		k.sent[0] = nil
		k.sent = k.sent[1:]
	}
}

// withdraw forgets the permissions of turn f, which ended: the answers that
// release their hooks, which hand claude nothing.
func (k *tracker) withdraw(f *flight) []askAnswer {
	var out []askAnswer
	keep := k.asks[:0]
	for _, p := range k.asks {
		if p.flight == f {
			out = append(out, askAnswer{id: p.ask.ID})
		} else {
			keep = append(keep, p)
		}
	}
	clear(k.asks[len(keep):])
	k.asks = keep
	sent := k.sent[:0]
	for _, p := range k.sent {
		if p.flight != f {
			sent = append(sent, p)
		}
	}
	clear(k.sent[len(sent):])
	k.sent = sent
	return out
}

// checkAsks finds a lost prompt: a hook gone before it handed claude an
// answer, or an answer claude did not take within decisionWait.
func (k *tracker) checkAsks(now time.Time) step {
	alive := k.alive
	if alive == nil {
		alive = processAlive
	}
	gone := func(p *pendingAsk) bool {
		if now.Sub(p.checked) < aliveEvery {
			return false
		}
		p.checked = now
		return !alive(p.ask.PID)
	}
	for _, p := range k.asks {
		if p.flight.escPrompt {
			continue // Esc is stopping the turn, and claude cancels the hook
		}
		if gone(p) {
			return k.lose(p.flight, "the permission hook ended before the prompt was answered: claude falls back to its own dialog", now)
		}
	}
	for _, p := range k.sent {
		switch {
		case p.delivered && now.Sub(p.answered) >= decisionWait:
			return k.lose(p.flight, "claude did not take the answer the permission hook handed it", now)
		case !p.delivered && gone(p):
			return k.lose(p.flight, "the permission hook ended before it handed claude the answer", now)
		case !p.delivered && now.Sub(p.answered) >= decisionWait:
			return k.lose(p.flight, "the permission hook did not hand claude the answer", now)
		}
	}
	return step{}
}

// lose gives up on turn f, whose prompt was lost: the prompt raised, if any,
// resolved by the harness, the other asks withdrawn, and Esc pressed, which
// ends the turn errored.
func (k *tracker) lose(f *flight, why string, now time.Time) step {
	s := step{note: why}
	for _, p := range k.asks {
		if p.flight == f && p.raised {
			e := f.event(adapter.PromptResolved, now)
			e.PromptID, e.By = p.ask.ID, "harness"
			s.events = append(s.events, e)
		}
	}
	if !f.ended {
		f.lost, f.lostAt = true, now
		s.esc = f
		k.interrupting()
		f.escPrompt = true
	}
	s.answers = k.withdraw(f)
	s.add(k.raiseNext(now))
	return s
}

// askAnswer is an answer for the transport to write: id's hook takes it.
type askAnswer struct {
	id     string
	answer live.Answer
}
