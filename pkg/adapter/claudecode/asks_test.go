package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// A permission is a prompt of claude's three options, "always" only when
// claude suggests a rule to keep; each choice becomes claude's decision.
func TestPermissionPromptAndDecision(t *testing.T) {
	p := Permission{
		Tool:        "Bash",
		Input:       json.RawMessage(`{"command":"touch asked","description":"mock"}`),
		Suggestions: json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"touch asked *"}],"behavior":"allow","destination":"localSettings"}]`),
	}
	pi := p.Prompt("p1")
	var opts []string
	for _, o := range pi.Options {
		opts = append(opts, o.OptionID)
	}
	if pi.PromptID != "p1" || pi.Kind != PromptKindPermission || pi.Prompt != "Allow Bash: touch asked?" || !reflect.DeepEqual(opts, []string{OptionYes, OptionAlways, OptionNo}) {
		t.Errorf("prompt %+v", pi)
	}
	for _, q := range []Permission{{Tool: "Bash", Input: p.Input}, {Tool: "Bash", Input: p.Input, Suggestions: p.Suggestions, NoAlways: true}} {
		if n := len(q.Prompt("p").Options); n != 2 {
			t.Errorf("%+v: %d options, want yes and no", q, n)
		}
	}
	if got := (Permission{Tool: "Mcp", Input: json.RawMessage(`{"x":1}`)}).Prompt("p").Prompt; got != `Allow Mcp: {"x":1}?` {
		t.Errorf("an input with no command: %q", got)
	}
	if got := (Permission{Tool: "Bash", Input: json.RawMessage(`{"command":"` + strings.Repeat("é", 4<<10) + `"}`)}).Prompt("p").Prompt; len(got) > maxPromptText || !strings.HasSuffix(got, "…") {
		t.Errorf("a long command: %d bytes", len(got))
	}

	enc := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	for _, c := range []struct {
		opt      string
		classify bool
		want     string
	}{
		{OptionYes, true, `{"behavior":"allow","decisionClassification":"user_temporary","updatedInput":{"command":"touch asked","description":"mock"}}`},
		{OptionAlways, true, `{"behavior":"allow","decisionClassification":"user_permanent","updatedInput":{"command":"touch asked","description":"mock"},"updatedPermissions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"touch asked *"}],"behavior":"allow","destination":"localSettings"}]}`},
		{OptionNo, true, `{"behavior":"deny","decisionClassification":"user_reject","message":"` + DeniedMessage + `"}`},
		{OptionYes, false, `{"behavior":"allow","updatedInput":{"command":"touch asked","description":"mock"}}`},
		{OptionNo, false, `{"behavior":"deny","message":"` + DeniedMessage + `"}`},
	} {
		if got := enc(p.Decision(contract.Choice{OptionID: c.opt}, c.classify)); got != c.want {
			t.Errorf("%s (classify %v): %s, want %s", c.opt, c.classify, got, c.want)
		}
	}
}

type nopWriteCloser struct {
	mu sync.Mutex
	bytes.Buffer
}

func (w *nopWriteCloser) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Buffer.Write(b)
}
func (*nopWriteCloser) Close() error { return nil }

// responses are the control responses written, by request id.
func (w *nopWriteCloser) responses(t *testing.T) map[string]map[string]any {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	out := map[string]map[string]any{}
	sc := bufio.NewScanner(bytes.NewReader(w.Bytes()))
	for sc.Scan() {
		var f struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string         `json:"request_id"`
				Response  map[string]any `json:"response"`
			} `json:"response"`
		}
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil || f.Type != "control_response" {
			t.Fatalf("a frame written: %s (%v)", sc.Bytes(), err)
		}
		out[f.Response.RequestID] = f.Response.Response
	}
	return out
}

func askFrame(id string) *frame {
	var f frame
	_ = json.Unmarshal([]byte(`{"type":"control_request","request_id":"`+id+`","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"touch `+id+`"}}}`), &f)
	return &f
}

// The permissions claude asks for are raised one at a time, each in the turn
// of the input claude is on; an answer resolves the prompt before claude is
// told, and the next is raised; claude's withdrawal resolves one by the
// harness; a turn's end forgets the rest; a permission asked on no input's
// turn is denied at once.
func TestAsks(t *testing.T) {
	var mu sync.Mutex
	var events []adapter.Event
	in := &nopWriteCloser{}
	tr := &transport{stdin: in, report: func(ev adapter.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}}
	take := func() []adapter.Event {
		mu.Lock()
		defer mu.Unlock()
		out := events
		events = nil
		return out
	}

	tr.onFrame(askFrame("r0"), nil)
	if evs := take(); len(evs) != 0 {
		t.Errorf("a permission on no turn raised %+v", evs)
	}
	if r := in.responses(t)["r0"]; r["behavior"] != "deny" {
		t.Errorf("a permission on no turn: %v, want denied", r)
	}

	tr.turn = &turnState{native: "n1", started: true}
	tr.onFrame(askFrame("r1"), nil)
	tr.onFrame(askFrame("r2"), nil)
	evs := take()
	if len(evs) != 1 || evs[0].Kind != adapter.PromptRaised || evs[0].Native != "n1" || evs[0].Prompt.PromptID != "r1" {
		t.Fatalf("two asks: %+v, want r1 raised alone", evs)
	}
	if err := tr.Answer(t.Context(), "r2", contract.Choice{OptionID: OptionYes}); codeOf(err) != contract.CodePromptGone {
		t.Errorf("answering one not raised: %v, want prompt_gone", err)
	}
	if err := tr.Answer(t.Context(), "r1", contract.Choice{OptionID: OptionYes}); err != nil {
		t.Fatal(err)
	}
	evs = take()
	if len(evs) != 2 || evs[0].Kind != adapter.PromptResolved || evs[0].PromptID != "r1" || evs[0].By != "answer" ||
		evs[1].Kind != adapter.PromptRaised || evs[1].Prompt.PromptID != "r2" {
		t.Fatalf("after the answer: %+v", evs)
	}
	if r := in.responses(t)["r1"]; r["behavior"] != "allow" || r["decisionClassification"] != "user_temporary" {
		t.Errorf("r1's response %v", r)
	}
	if err := tr.Answer(t.Context(), "r1", contract.Choice{OptionID: OptionYes}); codeOf(err) != contract.CodePromptGone {
		t.Errorf("answering r1 again: %v, want prompt_gone", err)
	}

	var cancel frame
	_ = json.Unmarshal([]byte(`{"type":"control_cancel_request","request_id":"r2"}`), &cancel)
	tr.onFrame(&cancel, nil)
	evs = take()
	if len(evs) != 1 || evs[0].Kind != adapter.PromptResolved || evs[0].PromptID != "r2" || evs[0].By != "harness" {
		t.Fatalf("claude withdrew r2: %+v", evs)
	}

	tr.onFrame(askFrame("r3"), nil)
	tr.onFrame(askFrame("r4"), nil)
	take()
	var res frame
	_ = json.Unmarshal([]byte(`{"type":"result","result":"done"}`), &res)
	tr.onFrame(&res, nil)
	evs = take()
	if len(evs) != 1 || evs[0].Kind != adapter.Ended {
		t.Fatalf("the turn's end: %+v, want it alone", evs)
	}
	if len(tr.asks) != 0 {
		t.Errorf("asks left after the turn: %d", len(tr.asks))
	}
	if err := tr.Answer(t.Context(), "r3", contract.Choice{OptionID: OptionYes}); codeOf(err) != contract.CodePromptGone {
		t.Errorf("answering after the turn: %v, want prompt_gone", err)
	}
}

func codeOf(err error) contract.Code {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		return ""
	}
	return ce.Code
}
