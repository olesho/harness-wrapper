package claudecode

import (
	"context"
	"encoding/json"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// At the gated posture claude asks the host for each permission a tool needs
// (--permission-prompt-tool stdio): a can_use_tool control request, which
// waits for its control response. Each is the Session's prompt in turn, named
// by the request's id: raised when claude asks, resolved by the answer, which
// becomes the response, or by claude withdrawing it (control_cancel_request),
// or with the turn it was asked in. A permission asked while claude is on no
// input's turn — a turn of its own — is denied at once: nothing can answer it
// there (the Session raises prompts in an input's turn), and claude would
// wait for ever.

// ask is one permission claude waits on.
type ask struct {
	id     string // the control request's id: the prompt's id
	native string // the input whose turn it was asked in
	perm   Permission
	raised bool
}

// onAsk takes a can_use_tool request.
func (t *transport) onAsk(requestID string, raw json.RawMessage) {
	var r struct {
		Tool        string          `json:"tool_name"`
		Input       json.RawMessage `json:"input"`
		Suggestions json.RawMessage `json:"permission_suggestions"`
		NoAlways    bool            `json:"suppress_always_allow_rule"`
	}
	_ = json.Unmarshal(raw, &r)
	t.mu.Lock()
	native := ""
	if t.turn != nil {
		native = t.turn.native
	}
	t.mu.Unlock()
	if native == "" || requestID == "" {
		_ = t.respond(requestID, NoOneToAsk())
		return
	}
	t.amu.Lock()
	defer t.amu.Unlock()
	t.asks = append(t.asks, &ask{
		id: requestID, native: native,
		perm: Permission{Tool: r.Tool, Input: r.Input, Suggestions: r.Suggestions, NoAlways: r.NoAlways},
	})
	t.raiseNextLocked()
}

// raiseNextLocked raises the first permission waiting, unless it is raised
// already. It is called with t.amu held.
func (t *transport) raiseNextLocked() {
	if len(t.asks) == 0 || t.asks[0].raised {
		return
	}
	a := t.asks[0]
	a.raised = true
	p := a.perm.Prompt(a.id)
	t.report(adapter.Event{Kind: adapter.PromptRaised, Native: a.native, Prompt: &p})
}

// takeLocked removes the permission id from those waiting, and reports
// whether it was raised. It is called with t.amu held.
func (t *transport) takeLocked(id string) (*ask, bool) {
	for i, a := range t.asks {
		if a.id == id {
			t.asks = append(t.asks[:i:i], t.asks[i+1:]...)
			return a, true
		}
	}
	return nil, false
}

// onCancelAsk takes claude withdrawing a request: a permission it no longer
// waits on, resolved by the harness.
func (t *transport) onCancelAsk(requestID string) {
	t.amu.Lock()
	defer t.amu.Unlock()
	a, ok := t.takeLocked(requestID)
	if !ok {
		return
	}
	if a.raised {
		t.report(adapter.Event{Kind: adapter.PromptResolved, Native: a.native, PromptID: a.id, By: "harness"})
	}
	t.raiseNextLocked()
}

// dropAsks forgets the permissions of a turn that ended, or of a claude that
// exited: the Session resolves the raised one with its turn.
func (t *transport) dropAsks(native string) {
	t.amu.Lock()
	defer t.amu.Unlock()
	keep := t.asks[:0]
	for _, a := range t.asks {
		if native != "" && a.native != native {
			keep = append(keep, a)
		}
	}
	clear(t.asks[len(keep):])
	t.asks = keep
	t.raiseNextLocked()
}

// Answer answers the permission promptID as the choice says: the control
// response claude waits on. The prompt is resolved before the response is
// written, so its turn cannot end first.
func (t *transport) Answer(_ context.Context, promptID string, c contract.Choice) error {
	t.amu.Lock()
	defer t.amu.Unlock()
	if len(t.asks) == 0 || t.asks[0].id != promptID || !t.asks[0].raised {
		return contract.Errorf(contract.CodePromptGone, "claude no longer waits on %q", promptID)
	}
	a, _ := t.takeLocked(promptID)
	t.report(adapter.Event{Kind: adapter.PromptResolved, Native: a.native, PromptID: a.id, By: "answer"})
	err := t.respond(a.id, a.perm.Decision(c, true))
	t.raiseNextLocked()
	if err != nil {
		return &contract.Error{Code: contract.CodeExited, Message: "claude exited before it took the answer"}
	}
	return nil
}

// respond answers claude's control request id with a permission decision.
func (t *transport) respond(id string, decision map[string]any) error {
	return t.write(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": decision,
	}})
}
