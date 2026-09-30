package saveload

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// watch counts the notifications the app-server sends for d, by method.
func (a *appServer) watch(d time.Duration) map[string]int {
	seen := map[string]int{}
	deadline := time.After(d)
	for {
		select {
		case line, ok := <-a.lines:
			if !ok {
				return seen
			}
			var msg struct {
				Method string `json:"method"`
			}
			if json.Unmarshal(line, &msg) == nil && msg.Method != "" {
				seen[msg.Method]++
			}
		case <-deadline:
			return seen
		}
	}
}

// activeGoal is what TestCodexActiveGoal saw.
type activeGoal struct {
	Codex string `json:"codex"`
	// Paused: a thread with a paused goal, resumed and left alone.
	PausedTurns    int `json:"paused_turns_started"`
	PausedRequests int `json:"paused_model_requests"`
	// Set: the goal made active, and the thread left alone.
	SetTurns    int `json:"active_turns_started_after_set"`
	SetRequests int `json:"active_model_requests_after_set"`
	// Resumed: the thread, its goal active, resumed in a new process and left
	// alone.
	ResumedTurns    int `json:"active_turns_started_after_resume"`
	ResumedRequests int `json:"active_model_requests_after_resume"`
	// Adapter: the same thread reopened through the Harness Adapter.
	AdapterIdleRequests     int    `json:"adapter_model_requests_before_any_input"`
	AdapterIdlePhase        string `json:"adapter_phase_before_any_input"`
	AdapterIdleObservations int    `json:"adapter_record_observations_before_any_input"`
	AdapterInputEnded       bool   `json:"adapter_input_turn_ended"`
	AdapterInputWait        string `json:"adapter_input_wait"`
	AdapterInputError       string `json:"adapter_input_error,omitempty"`
	// AdapterInputSeen: the observations the adapter delivered for the input,
	// by kind and origin; AdapterInputRequests: the model requests that
	// carried its prompt.
	AdapterInputSeen     map[string]int `json:"adapter_input_observations"`
	AdapterInputRequests int            `json:"adapter_model_requests_with_the_input"`
	AdapterPhaseAfter    string         `json:"adapter_phase_after"`
	AdapterRequests      int            `json:"adapter_model_requests_in_all"`
}

// TestCodexActiveGoal is beside the gate: it shows why the probe saves a
// paused goal, and what a Load of an active one has to reckon with. codex
// works on an active goal by itself — it starts a turn when the goal is set,
// and again when the thread is resumed — and the mock, which answers every
// request at once and never completes the goal, makes it do so without end.
// The Harness Adapter's Codex transport knows the turns it started; here it is
// given a thread that starts its own.
func TestCodexActiveGoal(t *testing.T) {
	p := start(t, codexCLI{}, modeMock)
	const window = 2 * time.Second
	env := p.newEnvironment("goal", filepath.Join(p.work, "goal"), sourceDirs)
	env.provision(p.h.Spec(p, nil))
	s, err := env.open(contract.OpenFresh, "", nil)
	fatalIf(t, err, "opening a fresh Session")
	id := s.id
	_, err = s.turn("PING one")
	fatalIf(t, err, "the turn")
	_, cp, err := s.close()
	fatalIf(t, err, "closing")
	env.quiet(10 * time.Second)
	got := activeGoal{Codex: p.rep.Versions.Binary}
	goal := func(a *appServer, status string) {
		t.Helper()
		fatalIf(t, a.call("thread/goal/set", map[string]any{"threadId": id, "objective": "Keep the probe's notes", "status": status}, nil), "thread/goal/set %s", status)
	}

	// The control: paused, the goal starts nothing.
	a, err := startAppServer(env)
	fatalIf(t, err, "codex")
	fatalIf(t, a.resume(env, id), "thread/resume")
	goal(a, "paused")
	n := p.requestCount()
	got.PausedTurns = a.watch(window)["turn/started"]
	got.PausedRequests = p.requestCount() - n

	// Active: codex starts turns nobody asked for.
	n = p.requestCount()
	goal(a, "active")
	got.SetTurns = a.watch(window)["turn/started"]
	got.SetRequests = p.requestCount() - n
	a.close()
	env.quiet(10 * time.Second)

	// A new process, the thread resumed and nothing else.
	b, err := startAppServer(env)
	fatalIf(t, err, "codex")
	n = p.requestCount()
	fatalIf(t, b.resume(env, id), "thread/resume")
	got.ResumedTurns = b.watch(window)["turn/started"]
	got.ResumedRequests = p.requestCount() - n
	b.close()
	env.quiet(10 * time.Second)

	// Through the adapter: a reopen from the record's end, no input, then one.
	read, err := env.readRecord(id, cp)
	fatalIf(t, err, "reading the record")
	n = p.requestCount()
	s2, err := env.open(contract.OpenReopen, id, read.CP)
	fatalIf(t, err, "the reopen")
	time.Sleep(window)
	got.AdapterIdleRequests = p.requestCount() - n
	got.AdapterIdlePhase = string(s2.s.State().Phase)
	got.AdapterIdleObservations = len(recordOf(s2.snapshot()))
	p.wait = 10 * time.Second
	got.AdapterInputWait = p.wait.String()
	const prompt = "PING two"
	if _, err := s2.turn(prompt); err != nil {
		got.AdapterInputError = p.rep.cleanErr(err)
	} else {
		got.AdapterInputEnded = true
	}
	got.AdapterPhaseAfter = string(s2.s.State().Phase)
	got.AdapterInputSeen = map[string]int{}
	for _, o := range s2.snapshot() {
		if o.InputID != "" {
			got.AdapterInputSeen[string(o.Kind)+" ("+string(o.Origin)+")"]++
		}
	}
	for _, r := range p.requests(n) {
		if conv, err := p.h.Conversation(r.Body); err == nil {
			for _, it := range conv {
				if it.Role == "user" && it.Text == prompt {
					got.AdapterInputRequests++
					break
				}
			}
		}
	}
	got.AdapterRequests = p.requestCount() - n
	res, _, _ := s2.close()
	if left := env.quiet(10 * time.Second); !res.Stopped || len(left) > 0 {
		t.Errorf("codex did not stop: %+v, %d processes left", res, len(left))
	}

	out, _ := json.MarshalIndent(got, "", "  ")
	t.Logf("%s", out)
	if dir := os.Getenv(envEvidence); dir != "" {
		fatalIf(t, os.MkdirAll(dir, 0o755), "evidence")                                                              //nolint:gosec // evidence meant to be read
		fatalIf(t, os.WriteFile(filepath.Join(dir, "codex-active-goal.json"), append(out, '\n'), 0o644), "evidence") //nolint:gosec // evidence meant to be read
	}
	if got.PausedTurns != 0 || got.PausedRequests != 0 {
		t.Errorf("a paused goal started %d turn(s), %d model request(s): the probe's saved goal would race its turns", got.PausedTurns, got.PausedRequests)
	}
	if got.SetTurns == 0 || got.ResumedTurns == 0 {
		t.Errorf("an active goal started %d turn(s) when set and %d when resumed: codex no longer works on a goal by itself, and FINDINGS.md is out of date", got.SetTurns, got.ResumedTurns)
	}
}
