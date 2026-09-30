package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// pump observes a Session or a record handle and acknowledges every batch,
// keeping what it saw.
type pump struct {
	mu   sync.Mutex
	seen []contract.Observation
	stop chan struct{}
	done chan struct{}
}

type observer interface {
	Observe(ctx context.Context, wait time.Duration, maxBytes int) (contract.Batch, error)
	Ack(batchID string) error
}

func startPump(t *testing.T, s observer) *pump {
	p := &pump{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		for {
			select {
			case <-p.stop:
				return
			default:
			}
			b, err := s.Observe(context.Background(), 200*time.Millisecond, contract.MaxObserveBytes)
			if err != nil {
				t.Errorf("Observe: %v", err)
				return
			}
			p.mu.Lock()
			p.seen = append(p.seen, b.Items...)
			p.mu.Unlock()
			if b.NeedsAck() {
				if err := s.Ack(b.BatchID); err != nil {
					t.Errorf("Ack: %v", err)
					return
				}
			}
		}
	}()
	return p
}

func (p *pump) halt() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}

// find is the first observation pred holds of, if any came yet.
func (p *pump) find(pred func(contract.Observation) bool) (contract.Observation, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, o := range p.seen {
		if pred(o) {
			return o, true
		}
	}
	return contract.Observation{}, false
}

// await waits for an observation matching pred.
func (p *pump) await(t *testing.T, what string, pred func(contract.Observation) bool) contract.Observation {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if o, ok := p.find(pred); ok {
			return o
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s", what)
	return contract.Observation{}
}

// ended waits for the end of an input's turn, of one origin.
func (p *pump) ended(t *testing.T, inputID string, origin contract.Origin) contract.TurnEndedData {
	t.Helper()
	o := p.await(t, "turn_ended ("+string(origin)+") of "+inputID, func(o contract.Observation) bool {
		return o.Kind == contract.KindTurnEnded && o.Origin == origin && o.InputID == inputID
	})
	var d contract.TurnEndedData
	_ = o.Decode(&d)
	return d
}

// own waits for the start of a turn codex started itself, other than those
// named.
func (p *pump) own(t *testing.T, skip ...string) string {
	t.Helper()
	o := p.await(t, "turn of codex's own", func(o contract.Observation) bool {
		if o.Kind != contract.KindTurnStarted || o.InputID != "" || o.TurnID == "" {
			return false
		}
		for _, id := range skip {
			if o.TurnID == id {
				return false
			}
		}
		return true
	})
	return o.TurnID
}

// ownEnded waits for the end of a turn codex started itself, of one origin.
func (p *pump) ownEnded(t *testing.T, turnID string, origin contract.Origin) contract.TurnEndedData {
	t.Helper()
	o := p.await(t, "turn_ended ("+string(origin)+") of "+turnID, func(o contract.Observation) bool {
		return o.Kind == contract.KindTurnEnded && o.Origin == origin && o.TurnID == turnID && o.InputID == ""
	})
	var d contract.TurnEndedData
	_ = o.Decode(&d)
	return d
}

// liveAgent is one agent's roots, provisioned for the mock.
type liveAgent struct {
	base   string
	layout contract.Layout
	result contract.ProvisionResult
	cred   *contract.CredentialFile
}

// newLiveAgent makes an agent's roots and renders codex's configuration into
// them; load names a saved thread to load there.
func newLiveAgent(t *testing.T, root string, mock *mockapi.Server, load *contract.LoadSource) liveAgent {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"), Workspace: filepath.Join(base, "workspace"),
		Secrets: filepath.Join(base, "secrets"), Scratch: filepath.Join(base, "scratch"),
	}
	for _, d := range []string{l.Home, l.Config, l.Workspace, l.Secrets, l.Scratch} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	a := adapter.New(Profile{})
	res, err := a.Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: root, Layout: l, Spec: contract.AgentSpec{
		PermissionPosture: contract.PostureBypass, Credential: &contract.CredentialRef{Kind: CredentialAPIKey},
	}, Load: load})
	if err != nil {
		t.Fatal(err)
	}
	pointAt(mock)(conformance.Testing(t), l, &res)
	if err := conformance.Apply(l, res); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(l.Secrets, CredentialAPIKey)
	if err := os.WriteFile(cred, []byte("sk-mock-placeholder-not-a-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return liveAgent{base: base, layout: l, result: res, cred: &contract.CredentialFile{Kind: CredentialAPIKey, File: cred}}
}

// open opens a Session for the agent and pumps it.
func (ag liveAgent) open(t *testing.T, req contract.OpenRequest) (contract.Session, *pump, string) {
	t.Helper()
	req.OpenConfig, req.Layout, req.Credential = ag.result.OpenConfig, ag.layout, ag.cred
	s, err := adapter.New(Profile{}).NewSession(req)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := s.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p := startPump(t, s)
	t.Cleanup(func() {
		_, _ = s.Close(context.Background(), contract.ClosePark, 0)
		p.halt()
	})
	return s, p, opened.SessionID
}

// park closes a Session, drained, and stops its pump.
func park(t *testing.T, s contract.Session, p *pump) {
	t.Helper()
	res, err := s.Close(context.Background(), contract.ClosePark, 10*time.Second)
	if err != nil || !res.Stopped || !res.Drained {
		t.Fatalf("Close = %+v %v, want stopped and drained", res, err)
	}
	p.halt()
}

func send(t *testing.T, s contract.Session, id, text string) {
	t.Helper()
	if _, err := s.Send(context.Background(), contract.Text(id, text)); err != nil {
		t.Fatalf("Send %s: %v", text, err)
	}
}

func idle(t *testing.T, s contract.Session) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for s.State().Phase != contract.PhaseIdle {
		if time.Now().After(deadline) {
			t.Fatalf("the Session is %s, want idle", s.State().Phase)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// codexOf is the transport behind a Session.
func codexOf(t *testing.T, s contract.Session) *transport {
	t.Helper()
	tr, ok := adapter.TransportOf(s).(*transport)
	if !ok {
		t.Fatal("no codex behind the Session")
	}
	return tr
}

// A goal the model sets makes codex work by itself, turn after turn, until
// the goal is complete: each turn is reported as codex's own, live and from
// the rollout, and what it says names the turn and no input.
func TestCodexWorksOnItsGoal(t *testing.T) {
	bin := realCodex(t)
	mock := mockapi.Start()
	mock.ChunkDelay = 20 * time.Millisecond
	defer mock.Close()
	ag := newLiveAgent(t, distribution(t, bin), mock, nil)
	s, p, _ := ag.open(t, contract.OpenRequest{Mode: contract.OpenFresh})

	send(t, s, "in-goal", "MKGOAL GOAL 1 3")
	if d := p.ended(t, "in-goal", contract.OriginLive); d.Outcome != contract.TurnCompleted {
		t.Fatalf("the turn that sets the goal: %+v", d)
	}
	first := p.own(t)
	for _, origin := range []contract.Origin{contract.OriginLive, contract.OriginRecord} {
		if d := p.ownEnded(t, first, origin); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "goal step 1.2") {
			t.Errorf("the first turn of codex's own, %s: %+v", origin, d)
		}
	}
	said := p.await(t, "what codex said in its own turn", func(o contract.Observation) bool {
		return o.Kind == contract.KindAssistantText && o.TurnID == first
	})
	if said.InputID != "" || said.Origin != contract.OriginRecord {
		t.Errorf("assistant_text of codex's own turn: input %q, origin %s", said.InputID, said.Origin)
	}
	// The second completes the goal, through the model's update_goal call.
	second := p.own(t, first)
	if d := p.ownEnded(t, second, contract.OriginRecord); d.Outcome != contract.TurnCompleted {
		t.Errorf("the turn that completes the goal: %+v", d)
	}
	p.await(t, "the update_goal call", func(o contract.Observation) bool {
		return o.Kind == contract.KindToolUse && o.TurnID == second && o.InputID == ""
	})
	idle(t, s)
	time.Sleep(time.Second)
	if id, ok := p.find(func(o contract.Observation) bool {
		return o.Kind == contract.KindTurnStarted && o.InputID == "" && o.TurnID != first && o.TurnID != second
	}); ok {
		t.Errorf("codex started turn %s with its goal complete", id.TurnID)
	}
	tr := codexOf(t, s)
	tr.mu.Lock()
	g := tr.goal
	tr.mu.Unlock()
	if g == nil || g.Objective != "GOAL 1 3" || g.Status != "complete" {
		t.Errorf("the goal the transport follows: %+v, want GOAL 1 3, complete", g)
	}
	saved, err := readNative(ag.layout.Scratch, tr.SessionID())
	if err != nil || saved.Goal == nil || *saved.Goal != *g || saved.Home != ag.layout.Config {
		t.Errorf("the thread's native state: %+v %v", saved, err)
	}
}

// Parked while codex is on a turn of its own, the Session stops cleanly: codex
// aborts the turn and says so in its rollout. Reopened, codex takes its goal
// up again, and an input sent then is answered by a turn of its own.
func TestCodexParksOnItsGoal(t *testing.T) {
	bin := realCodex(t)
	mock := mockapi.Start()
	defer mock.Close()
	ag := newLiveAgent(t, distribution(t, bin), mock, nil)
	s, p, id := ag.open(t, contract.OpenRequest{Mode: contract.OpenFresh})

	send(t, s, "in-goal", "MKGOAL GOAL 9 40")
	p.ended(t, "in-goal", contract.OriginLive)
	first := p.own(t)
	time.Sleep(300 * time.Millisecond)
	park(t, s, p)
	if d := p.ownEnded(t, first, contract.OriginRecord); d.Outcome != contract.TurnInterrupted {
		t.Errorf("the turn codex was on as it parked: %+v, want interrupted, from the rollout", d)
	}
	if o, ok := p.find(func(o contract.Observation) bool { return o.Kind == contract.KindSessionExited }); ok {
		var d contract.SessionExitedData
		_ = o.Decode(&d)
		if d.Class != contract.ExitClean {
			t.Errorf("session_exited %+v, want clean", d)
		}
	} else {
		t.Error("no session_exited")
	}

	s2, p2, id2 := ag.open(t, contract.OpenRequest{Mode: contract.OpenReopen, SessionID: id})
	if id2 != id {
		t.Fatalf("reopened %s, want %s", id2, id)
	}
	second := p2.own(t)
	if second == first {
		t.Fatalf("the reopened Session's turn has the id of the one it parked on")
	}
	send(t, s2, "in-ping", "PING 5")
	if d := p2.ended(t, "in-ping", contract.OriginRecord); d.Outcome != contract.TurnCompleted || d.Text != "PONG 5" {
		t.Errorf("an input while codex works on its goal: %+v, want completed with PONG 5", d)
	}
	if d := p2.ownEnded(t, second, contract.OriginLive); d.Outcome != contract.TurnInterrupted {
		t.Errorf("the turn the input stopped: %+v, want interrupted", d)
	}
	// The input's turn over, codex goes back to its goal.
	third := p2.own(t, second)
	if out, err := s2.Interrupt(context.Background(), contract.InterruptRequest{TurnID: third, DeadlineMS: 15000}); err != nil || out != contract.InterruptStopped {
		t.Errorf("interrupting codex's own turn: %v %v, want stopped", out, err)
	}
	idle(t, s2)
}

// An input codex reads while a turn of its own runs is folded into that turn,
// which no Session lets happen — it stops the turn first — unless codex starts
// the turn in the instant before it reads the input. The turn is then the
// input's from its message on: codex's own ends there, live and in the
// rollout, and the input is answered.
func TestCodexFoldsAnInputIntoItsOwnTurn(t *testing.T) {
	bin := realCodex(t)
	mock := mockapi.Start()
	mock.ChunkDelay = 50 * time.Millisecond
	defer mock.Close()
	ag := newLiveAgent(t, distribution(t, bin), mock, nil)
	s, p, id := ag.open(t, contract.OpenRequest{Mode: contract.OpenFresh})

	send(t, s, "in-goal", "MKGOAL GOAL 9 20")
	p.ended(t, "in-goal", contract.OriginLive)
	own := p.own(t)
	tr := codexOf(t, s)
	tr.mu.Lock()
	tr.steer = true
	tr.mu.Unlock()
	send(t, s, "in-folded", "PING 6")
	for _, origin := range []contract.Origin{contract.OriginLive, contract.OriginRecord} {
		if d := p.ownEnded(t, own, origin); d.Outcome != contract.TurnInterrupted {
			t.Errorf("codex's own turn, %s: %+v, want interrupted where it took the input in", origin, d)
		}
		if d := p.ended(t, "in-folded", origin); d.Outcome != contract.TurnCompleted || d.Text != "PONG 6" {
			t.Errorf("the folded input's turn, %s: %+v, want completed with PONG 6", origin, d)
		}
	}
	p.await(t, "the folded input's user_input", func(o contract.Observation) bool {
		return o.Kind == contract.KindUserInput && o.InputID == "in-folded" && o.Origin == contract.OriginRecord
	})
	tr.mu.Lock()
	tr.steer = false
	tr.mu.Unlock()
	// codex goes on with its goal; stop it, and read the record alone.
	next := p.own(t, own)
	if out, err := s.Interrupt(context.Background(), contract.InterruptRequest{TurnID: next, DeadlineMS: 15000}); err != nil || out != contract.InterruptStopped {
		t.Fatalf("interrupting codex's own turn: %v %v", out, err)
	}
	idle(t, s)
	park(t, s, p)
	r, err := adapter.New(Profile{}).OpenRecord(context.Background(), contract.RecordRequest{SessionID: id, OpenConfig: ag.result.OpenConfig, Layout: ag.layout})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if got, err := r.Recover(context.Background(), "in-folded"); err != nil || got.Outcome != contract.RecoveredCompleted {
		t.Errorf("Recover of the folded input = %+v %v, want completed", got, err)
	}
}

// An input codex holds for a turn of its own is dropped when that turn is
// interrupted before codex takes it in. Interrupted by its turn id, the input
// is sent again and answered; interrupted as the input, it never ran and ends
// cancelled.
func TestCodexDropsAnInputWithItsOwnTurn(t *testing.T) {
	bin := realCodex(t)
	mock := mockapi.Start()
	defer mock.Close()
	ag := newLiveAgent(t, distribution(t, bin), mock, nil)
	s, p, _ := ag.open(t, contract.OpenRequest{Mode: contract.OpenFresh})
	ctx := context.Background()

	send(t, s, "in-goal", "MKGOAL GOAL 9 100")
	p.ended(t, "in-goal", contract.OriginLive)
	own := p.own(t)
	tr := codexOf(t, s)
	steer := func(on bool) {
		tr.mu.Lock()
		tr.steer = on
		tr.mu.Unlock()
	}
	steer(true)
	send(t, s, "in-resent", "PING 7")
	steer(false)
	if out, err := s.Interrupt(ctx, contract.InterruptRequest{TurnID: own, DeadlineMS: 15000}); err != nil || out != contract.InterruptStopped {
		t.Fatalf("interrupting codex's own turn: %v %v, want stopped", out, err)
	}
	if d := p.ended(t, "in-resent", contract.OriginRecord); d.Outcome != contract.TurnCompleted || d.Text != "PONG 7" {
		t.Errorf("the input codex dropped, sent again: %+v, want completed with PONG 7", d)
	}

	// Its turn over, codex goes back to its goal; this time the input itself
	// is interrupted while codex holds it.
	next := p.own(t, own)
	steer(true)
	send(t, s, "in-cancelled", "PING 8")
	steer(false)
	if out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: "in-cancelled", DeadlineMS: 15000}); err != nil || out != contract.InterruptCancelled {
		t.Errorf("interrupting an input codex holds for its own turn: %v %v, want cancelled", out, err)
	}
	if d := p.ended(t, "in-cancelled", contract.OriginLive); d.Outcome != contract.TurnCancelled {
		t.Errorf("the input's turn: %+v, want cancelled", d)
	}
	if d := p.ownEnded(t, next, contract.OriginLive); d.Outcome != contract.TurnInterrupted {
		t.Errorf("codex's own turn: %+v, want interrupted", d)
	}
	idle(t, s)
	for _, r := range mock.Requests() {
		if r.Scenario == "PING 8" {
			t.Error("the cancelled input reached the model")
		}
	}
}

// without is saved with the files named dropped.
func without(saved conformance.Saved, drop ...contract.RootPath) conformance.Saved {
	out := conformance.Saved{Source: saved.Source}
	for _, f := range saved.Files {
		keep := true
		for _, d := range drop {
			keep = keep && !f.At.Within(d)
		}
		if keep {
			out.Files = append(out.Files, f)
		}
	}
	return out
}

// A thread's name and its goal are saved with it, and a loaded thread has
// them: codex holds in the new environment what it held in the old. A thread
// that lost either on the way does not open. The thread here is saved after a
// crash, when the goal's row is still in the database's log: copied without
// the log the goal is gone, and codex says nothing.
func TestCodexLoadKeepsNameAndGoal(t *testing.T) {
	bin := realCodex(t)
	mock := mockapi.Start()
	defer mock.Close()
	root := distribution(t, bin)
	src := newLiveAgent(t, root, mock, nil)
	s, p, id := src.open(t, contract.OpenRequest{Mode: contract.OpenFresh})
	ctx := context.Background()
	send(t, s, "in-1", "PING 1")
	p.ended(t, "in-1", contract.OriginRecord)
	idle(t, s)
	tr := codexOf(t, s)
	if _, err := tr.call(ctx, "thread/name/set", map[string]any{"threadId": id, "name": "the saved thread"}); err != nil {
		t.Fatal(err)
	}
	// Paused, the goal starts no turn: the thread rests as it is saved.
	if _, err := tr.call(ctx, "thread/goal/set", map[string]any{"threadId": id, "objective": "Keep the notes", "status": "paused"}); err != nil {
		t.Fatal(err)
	}
	want := nativeState{Thread: id, Name: "the saved thread", Goal: &goal{Objective: "Keep the notes", Status: "paused"}}
	deadline := time.Now().Add(10 * time.Second)
	for {
		// What codex says of the thread is kept as it says it.
		kept, err := readNative(src.layout.Scratch, id)
		if err == nil && kept.same(want) && kept.Home == src.layout.Config {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the native state kept: %+v %v, want %s", kept, err, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	tr.kill()
	<-tr.exited
	p.halt()

	d := Profile{}.Describe()
	saved, err := conformance.Save(d, adapter.ArchiveFormat, src.layout, src.result)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range saved.Files {
		names = append(names, f.At.String())
	}
	for _, need := range []string{"config/" + indexFile, "config/" + goalsFile, "config/" + goalsLogFile, "scratch/" + nativeDir + "/" + id + ".json", "config/" + sessionsDir + "/"} {
		if !strings.Contains(strings.Join(names, "\n")+"\n", need) {
			t.Fatalf("the saved history lacks %s:\n%s", need, strings.Join(names, "\n"))
		}
	}
	for _, n := range names {
		if strings.Contains(n, authFile) || strings.HasSuffix(n, "-shm") {
			t.Errorf("the saved history holds %s", n)
		}
	}
	if err := os.RemoveAll(src.base); err != nil {
		t.Fatal(err)
	}

	load := func(saved conformance.Saved) (liveAgent, contract.OpenRequest) {
		t.Helper()
		loadSrc := saved.Source
		ag := newLiveAgent(t, root, mock, &loadSrc)
		if len(ag.result.HistoryRelocations) != 0 {
			t.Fatalf("relocations %+v, want none", ag.result.HistoryRelocations)
		}
		if err := conformance.Restore(ag.layout, ag.result, saved); err != nil {
			t.Fatal(err)
		}
		return ag, contract.OpenRequest{Mode: contract.OpenReopen, SessionID: id, Loaded: true, OpenConfig: ag.result.OpenConfig, Layout: ag.layout, Credential: ag.cred}
	}
	config := func(path string) contract.RootPath { return contract.RootPath{Root: contract.RootConfig, Path: path} }
	for what, drop := range map[string]contract.RootPath{
		"its goal's log":      config(goalsLogFile),
		"its goal":            config(goalsFile),
		"its name":            config(indexFile),
		"the account of both": {Root: contract.RootScratch, Path: nativeDir},
	} {
		_, req := load(without(saved, drop))
		lost, err := adapter.New(Profile{}).NewSession(req)
		if err != nil {
			t.Fatal(err)
		}
		_, err = lost.Open(ctx)
		var ce *contract.Error
		if !errors.As(err, &ce) || ce.Code != contract.CodeOpenFailed || ce.Reason != contract.OpenStateMismatch {
			t.Errorf("a thread loaded without %s: Open = %v, want open_failed{state_mismatch}", what, err)
		}
		_, _ = lost.Close(ctx, contract.ClosePark, 0)
	}

	ag, req := load(saved)
	s2, p2, id2 := ag.open(t, req)
	if id2 != id {
		t.Fatalf("the loaded thread opened as %s, want %s", id2, id)
	}
	tr2 := codexOf(t, s2)
	got, err := tr2.readState(ctx, id)
	if err != nil || !got.same(want) {
		t.Errorf("codex holds of the loaded thread %s (%v), want %s", got, err, want)
	}
	if kept, err := readNative(ag.layout.Scratch, id); err != nil || !kept.same(want) || kept.Home != ag.layout.Config {
		t.Errorf("the native state after the first open here: %+v %v", kept, err)
	}
	send(t, s2, "in-2", "PING 2")
	if d := p2.ended(t, "in-2", contract.OriginRecord); d.Outcome != contract.TurnCompleted || d.Text != "PONG 2" {
		t.Errorf("a turn of the loaded thread: %+v", d)
	}
	var body string
	for _, r := range mock.Requests() {
		if r.Scenario == "PING 2" {
			body = r.Input
		}
	}
	if !strings.Contains(body, "PING 1") || !strings.Contains(body, "PONG 1") {
		t.Errorf("the loaded thread's model request lacks the saved conversation: %q", body)
	}
	// The goal is paused: codex starts nothing for it.
	idle(t, s2)
	if o, ok := p2.find(func(o contract.Observation) bool { return o.Kind == contract.KindTurnStarted && o.InputID == "" }); ok {
		t.Errorf("codex started turn %s for a paused goal", o.TurnID)
	}
	// It reopens as a loaded thread does from then on, with no check of what
	// this environment already vouched for.
	park(t, s2, p2)
	_, p3, id3 := ag.open(t, req)
	if id3 != id {
		t.Fatalf("reopened as %s, want %s", id3, id)
	}
	_ = p3
}
