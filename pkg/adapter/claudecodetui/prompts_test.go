package claudecodetui

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// kind waits for an observation of kind and origin, of the input named.
func (p *pump) kind(t *testing.T, kind contract.Kind, origin contract.Origin, inputID string) contract.Observation {
	t.Helper()
	return p.await(t, string(kind)+" ("+string(origin)+") of "+inputID, func(o contract.Observation) bool {
		return o.Kind == kind && o.Origin == origin && (inputID == "" || o.InputID == inputID)
	})
}

// promptCheck drives one gated Session's prompts.
type promptCheck struct {
	t *testing.T
	s contract.Session
	p *pump
}

func (c promptCheck) send(id, text string) {
	c.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for c.s.State().Phase != contract.PhaseIdle && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := c.s.Send(context.Background(), contract.Text(id, text)); err != nil {
		c.t.Fatalf("Send %s: %v", id, err)
	}
}

func (c promptCheck) raise(id string) contract.PromptInfo {
	c.t.Helper()
	c.send(id, "ASK")
	var pi contract.PromptInfo
	_ = c.p.kind(c.t, contract.KindPromptRaised, contract.OriginLive, id).Decode(&pi)
	if pi.Kind != claudecode.PromptKindPermission || !strings.Contains(pi.Prompt, "Bash: touch asked") {
		c.t.Errorf("%s: prompt %+v", id, pi)
	}
	return pi
}

func (c promptCheck) resolved(id string) contract.PromptResolvedData {
	c.t.Helper()
	var d contract.PromptResolvedData
	_ = c.p.kind(c.t, contract.KindPromptResolved, contract.OriginLive, id).Decode(&d)
	return d
}

func (c promptCheck) ended(id string) contract.TurnEndedData {
	c.t.Helper()
	var d contract.TurnEndedData
	_ = c.p.kind(c.t, contract.KindTurnEnded, contract.OriginLive, id).Decode(&d)
	return d
}

// At the gated posture, against the pinned claude on a terminal and the
// mock's ASK: "no" denies the tool and the model goes on, told so; "yes" runs
// it; an interrupt while the prompt waits is Esc, which stops the turn, and
// the prompt is resolved by the harness; Close while one waits stops claude.
func TestClaudeTUIPrompts(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	s, p, l := tuiSessionAt(t, mock, contract.PostureGated)
	c := promptCheck{t: t, s: s, p: p}
	ctx := context.Background()
	asked := filepath.Join(l.Workspace, "asked")

	pi := c.raise("in-no")
	if err := s.Answer(ctx, pi.PromptID, contract.Choice{OptionID: claudecode.OptionNo}); err != nil {
		t.Fatalf("Answer no: %v", err)
	}
	if d := c.resolved("in-no"); d.By != "answer" {
		t.Errorf("no: prompt_resolved %+v", d)
	}
	if d := c.ended("in-no"); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "denied") {
		t.Errorf("no: turn_ended %+v", d)
	}
	if _, err := os.Stat(asked); err == nil {
		t.Error("no: the tool ran")
	}

	pi = c.raise("in-yes")
	if err := s.Answer(ctx, pi.PromptID, contract.Choice{OptionID: claudecode.OptionYes}); err != nil {
		t.Fatalf("Answer yes: %v", err)
	}
	if d := c.ended("in-yes"); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "TOOL DONE: asked") {
		t.Errorf("yes: turn_ended %+v", d)
	}
	if _, err := os.Stat(asked); err != nil {
		t.Errorf("yes: the tool did not run: %v", err)
	}
	// claude logs taking each answer, as the profile reads it.
	tr, _ := adapter.TransportOf(s).(*transport)
	debug, _ := os.ReadFile(filepath.Join(tr.dir, "debug.log"))
	decisions := 0
	for _, line := range strings.Split(string(debug), "\n") {
		if parseDebugLine(line).kind == lineDecision {
			decisions++
			t.Logf("decision: %s", line)
		}
	}
	if decisions != 2 {
		t.Errorf("the debug log logs %d permission decisions, want 2", decisions)
	}

	pi = c.raise("in-int")
	out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: "in-int", DeadlineMS: 15000})
	if err != nil || out != contract.InterruptStopped {
		t.Errorf("interrupt while a prompt waits: %v %v, want stopped", out, err)
	}
	if d := c.resolved("in-int"); d.By != "harness" {
		t.Errorf("interrupt: prompt_resolved %+v", d)
	}
	if d := c.ended("in-int"); d.Outcome != contract.TurnInterrupted {
		t.Errorf("interrupt: turn_ended %+v", d)
	}
	if err := s.Answer(ctx, pi.PromptID, contract.Choice{OptionID: claudecode.OptionYes}); err == nil {
		t.Error("an answer after the interrupt was taken")
	}
	c.send("in-after", "PING 4")
	if d := c.ended("in-after"); d.Outcome != contract.TurnCompleted || d.Text != "PONG 4" {
		t.Errorf("the turn after the interrupt: %+v", d)
	}

	c.raise("in-close")
	res, err := s.Close(ctx, contract.ClosePark, 10*time.Second)
	if err != nil || !res.Stopped {
		t.Errorf("Close while a prompt waits: %+v %v", res, err)
	}
}

// HW_TUI_PROMPT_RUNS prompts, each answered yes, in one Session: whether
// claude takes every answer, as the probe's one lost answer (FINDINGS.md,
// Phase 3: macOS, under load) asks. Run it beside a load to reproduce that
// case; a lost prompt is logged with what the profile found and the debug
// log's permission lines.
func TestClaudeTUIPromptsRepeated(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("HW_TUI_PROMPT_RUNS"))
	if n < 1 {
		t.Skip("HW_TUI_PROMPT_RUNS does not say how many prompts to run")
	}
	mock := mockapi.Start()
	defer mock.Close()
	s, p, _ := tuiSessionAt(t, mock, contract.PostureGated)
	c := promptCheck{t: t, s: s, p: p}
	tr, _ := adapter.TransportOf(s).(*transport)
	lost := 0
	for i := range n {
		id := "in-" + strconv.Itoa(i)
		pi := c.raise(id)
		answered := time.Now()
		if err := s.Answer(context.Background(), pi.PromptID, contract.Choice{OptionID: claudecode.OptionYes}); err != nil {
			t.Fatalf("%s: Answer: %v", id, err)
		}
		d := c.ended(id)
		t.Logf("%s: %s after %s", id, d.Outcome, time.Since(answered).Round(time.Millisecond))
		if d.Outcome == contract.TurnCompleted {
			continue
		}
		lost++
		debug, _ := os.ReadFile(filepath.Join(tr.dir, "debug.log"))
		var lines []string
		for _, l := range strings.Split(string(debug), "\n") {
			if strings.Contains(l, "Permission") || strings.Contains(l, "[engine] turn") || strings.Contains(l, "Hooks:") {
				lines = append(lines, l)
			}
		}
		t.Errorf("%s: %+v; the profile: %s\ndebug:\n%s", id, d, tr.diag.String(), strings.Join(lines[max(0, len(lines)-12):], "\n"))
	}
	t.Logf("%d of %d prompts lost", lost, n)
}

// When the permission hook ends before it answers — killed here; a timeout
// does the same — claude falls back to its own dialog, which only keystrokes
// answer. The profile does not answer it: the prompt is resolved by the
// harness, Esc stops the turn, which ends errored, and the Session goes on.
func TestClaudeTUIPromptLost(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	s, p, _ := tuiSessionAt(t, mock, contract.PostureGated)
	c := promptCheck{t: t, s: s, p: p}
	c.raise("in-lost")
	tr, _ := adapter.TransportOf(s).(*transport)
	tr.mu.Lock()
	pid := 0
	if len(tr.k.asks) > 0 {
		pid = tr.k.asks[0].ask.PID
	}
	tr.mu.Unlock()
	if pid <= 0 {
		t.Fatal("no hook waits on the prompt")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if d := c.resolved("in-lost"); d.By != "harness" {
		t.Errorf("prompt_resolved %+v", d)
	}
	if d := c.ended("in-lost"); d.Outcome != contract.TurnErrored || d.Error == nil || d.Error.Class != contract.ErrorInternal {
		t.Errorf("turn_ended %+v, want errored (internal)", d)
	}
	c.send("in-next", "PING 5")
	if d := c.ended("in-next"); d.Outcome != contract.TurnCompleted || d.Text != "PONG 5" {
		t.Errorf("the turn after a lost prompt: %+v", d)
	}
}
