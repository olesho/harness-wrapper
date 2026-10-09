package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// At the gated posture, against the pinned claude and the mock's ASK (a
// write claude asks permission for): "no" denies the tool and the model goes
// on, told so; "yes" runs it; an interrupt while the prompt waits stops the
// turn, and the prompt is resolved by the harness; Close while it waits stops
// claude.
func TestClaudePrompts(t *testing.T) {
	bin := realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	ag := newLiveAgentAt(t, distribution(t, bin), mock, contract.PostureGated)
	a := adapter.New(Profile{})
	s, err := a.NewSession(contract.OpenRequest{Mode: contract.OpenFresh, OpenConfig: ag.result.OpenConfig, Layout: ag.layout, Credential: ag.cred})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.Open(ctx); err != nil {
		t.Fatal(err)
	}
	p := startPump(t, s)
	defer func() { p.halt() }()
	asked := filepath.Join(ag.layout.Workspace, "asked")
	raise := func(id string) contract.PromptInfo {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for s.State().Phase != contract.PhaseIdle && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := s.Send(ctx, contract.Text(id, "ASK")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		var pi contract.PromptInfo
		_ = p.kind(t, contract.KindPromptRaised, contract.OriginLive, id).Decode(&pi)
		var opts []string
		for _, o := range pi.Options {
			opts = append(opts, o.OptionID)
		}
		if pi.Kind != PromptKindPermission || !strings.Contains(pi.Prompt, "Bash: touch asked") ||
			!slices.Contains(opts, OptionYes) || !slices.Contains(opts, OptionNo) {
			t.Errorf("%s: prompt %+v", id, pi)
		}
		t.Logf("%s: prompt %q, options %v", id, pi.Prompt, opts)
		return pi
	}
	resolved := func(id string) contract.PromptResolvedData {
		t.Helper()
		var d contract.PromptResolvedData
		_ = p.kind(t, contract.KindPromptResolved, contract.OriginLive, id).Decode(&d)
		return d
	}
	ended := func(id string) contract.TurnEndedData {
		t.Helper()
		var d contract.TurnEndedData
		_ = p.kind(t, contract.KindTurnEnded, contract.OriginLive, id).Decode(&d)
		return d
	}

	// No: the tool does not run, and the model is told.
	pi := raise("in-no")
	if err := s.Answer(ctx, pi.PromptID, contract.Choice{OptionID: OptionNo}); err != nil {
		t.Fatalf("Answer no: %v", err)
	}
	if d := resolved("in-no"); d.By != "answer" {
		t.Errorf("no: prompt_resolved %+v", d)
	}
	if d := ended("in-no"); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "denied") {
		t.Errorf("no: turn_ended %+v", d)
	}
	if _, err := os.Stat(asked); err == nil {
		t.Error("no: the tool ran")
	}

	// Yes: it runs.
	pi = raise("in-yes")
	if err := s.Answer(ctx, pi.PromptID, contract.Choice{OptionID: OptionYes}); err != nil {
		t.Fatalf("Answer yes: %v", err)
	}
	if d := ended("in-yes"); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "TOOL DONE: asked") {
		t.Errorf("yes: turn_ended %+v", d)
	}
	if _, err := os.Stat(asked); err != nil {
		t.Errorf("yes: the tool did not run: %v", err)
	}

	// An interrupt while the prompt waits stops the turn; the prompt is the
	// harness's to resolve, and an answer after it finds it gone.
	pi = raise("in-int")
	out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: "in-int", DeadlineMS: 15000})
	if err != nil || out != contract.InterruptStopped {
		t.Errorf("interrupt while a prompt waits: %v %v, want stopped", out, err)
	}
	if d := resolved("in-int"); d.By != "harness" {
		t.Errorf("interrupt: prompt_resolved %+v", d)
	}
	if d := ended("in-int"); d.Outcome != contract.TurnInterrupted {
		t.Errorf("interrupt: turn_ended %+v", d)
	}
	if err := s.Answer(ctx, pi.PromptID, contract.Choice{OptionID: OptionYes}); err == nil {
		t.Error("an answer after the interrupt was taken")
	}

	// Close while a prompt waits stops claude.
	raise("in-close")
	res, err := s.Close(ctx, contract.ClosePark, 10*time.Second)
	if err != nil || !res.Stopped {
		t.Errorf("Close while a prompt waits: %+v %v", res, err)
	}

	p.halt()

	// A fresh Session of a fresh agent, as claude may keep the rule where
	// every Session of the agent finds it.
	ag = newLiveAgentAt(t, distribution(t, bin), mock, contract.PostureGated)
	s, err = a.NewSession(contract.OpenRequest{Mode: contract.OpenFresh, OpenConfig: ag.result.OpenConfig, Layout: ag.layout, Credential: ag.cred})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx); err != nil {
		t.Fatal(err)
	}
	p = startPump(t, s)
	defer func() { _, _ = s.Close(ctx, contract.ClosePark, 5*time.Second) }()
	// Always: it runs, and claude keeps the rule it suggested, so the same
	// command is not asked about again in the Session.
	pi = raise("in-always")
	if err := s.Answer(ctx, pi.PromptID, contract.Choice{OptionID: OptionAlways}); err != nil {
		t.Fatalf("Answer always: %v", err)
	}
	if d := ended("in-always"); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "TOOL DONE: asked") {
		t.Errorf("always: turn_ended %+v", d)
	}
	if _, err := s.Send(ctx, contract.Text("in-again", "ASK")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if d := ended("in-again"); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "TOOL DONE: asked") {
		t.Errorf("after always: turn_ended %+v", d)
	}
	p.mu.Lock()
	for _, o := range p.seen {
		if o.Kind == contract.KindPromptRaised && o.InputID == "in-again" {
			t.Error("after always the same command was asked about again")
		}
	}
	p.mu.Unlock()
}
