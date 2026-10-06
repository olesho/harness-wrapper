package claudecode

import (
	"context"
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

// pump observes a Session and acknowledges every batch, keeping what it saw.
type pump struct {
	mu   sync.Mutex
	seen []contract.Observation
	stop chan struct{}
	done chan struct{}
}

func startPump(t *testing.T, s contract.Session) *pump {
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
	close(p.stop)
	<-p.done
}

// await waits for an observation matching pred.
func (p *pump) await(t *testing.T, what string, pred func(contract.Observation) bool) contract.Observation {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, o := range p.seen {
			if pred(o) {
				p.mu.Unlock()
				return o
			}
		}
		p.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no %s", what)
	return contract.Observation{}
}

func (p *pump) kind(t *testing.T, kind contract.Kind, origin contract.Origin, inputID string) contract.Observation {
	t.Helper()
	return p.await(t, string(kind)+" ("+string(origin)+") of "+inputID, func(o contract.Observation) bool {
		return o.Kind == kind && o.Origin == origin && (inputID == "" || o.InputID == inputID)
	})
}

type liveAgent struct {
	layout contract.Layout
	result contract.ProvisionResult
	cred   *contract.CredentialFile
}

func newLiveAgent(t *testing.T, root string, mock *mockapi.Server) liveAgent {
	t.Helper()
	base := t.TempDir()
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
		PermissionPosture: contract.PostureBypass, Credential: &contract.CredentialRef{Kind: CredentialKind},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pointAt(mock)(conformance.Testing(t), l, &res)
	if err := conformance.Apply(l, res); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(l.Secrets, CredentialKind)
	if err := os.WriteFile(cred, []byte("mock-placeholder-not-a-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return liveAgent{layout: l, result: res, cred: &contract.CredentialFile{Kind: CredentialKind, File: cred}}
}

// Against the pinned claude and the mock, each kind of turn reports what the
// profile promises: the input and the reply from the transcript, keyed by
// entry; a tool call from the transcript and from the hooks; interrupts that
// stop; errors classed by what claude wrote; a usage wall that blocks, with
// its reset; and a spool left empty.
func TestClaudeObservations(t *testing.T) {
	bin := realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	root := distribution(t, bin)
	ag := newLiveAgent(t, root, mock)
	a := adapter.New(Profile{})
	s, err := a.NewSession(contract.OpenRequest{Mode: contract.OpenFresh, OpenConfig: ag.result.OpenConfig, Layout: ag.layout, Credential: ag.cred})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	opened, err := s.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := startPump(t, s)
	defer func() {
		_, _ = s.Close(ctx, contract.ClosePark, 5*time.Second)
		p.halt()
	}()
	send := func(id, text string) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for s.State().Phase != contract.PhaseIdle && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := s.Send(ctx, contract.Text(id, text)); err != nil {
			t.Fatalf("Send %s: %v", text, err)
		}
	}
	ended := func(id string, origin contract.Origin) contract.TurnEndedData {
		t.Helper()
		var d contract.TurnEndedData
		_ = p.kind(t, contract.KindTurnEnded, origin, id).Decode(&d)
		return d
	}

	// A reply.
	send("in-ping", "PING 1")
	if d := ended("in-ping", contract.OriginLive); d.Outcome != contract.TurnCompleted || d.Text != "PONG 1" {
		t.Errorf("live turn_ended %+v", d)
	}
	if d := ended("in-ping", contract.OriginRecord); d.Outcome != contract.TurnCompleted || d.Text != "PONG 1" {
		t.Errorf("record turn_ended %+v", d)
	}
	p.kind(t, contract.KindTurnStarted, contract.OriginLive, "in-ping")
	mk, _, _ := mustMarkers(t, ag.layout).Lookup("in-ping")
	if o := p.kind(t, contract.KindUserInput, contract.OriginRecord, "in-ping"); o.Entry != mk.Native || o.ID != "user_input:"+mk.Native+":0" {
		t.Errorf("user_input %s entry %s: the prompt's entry is the message's uuid %s", o.ID, o.Entry, mk.Native)
	}
	if o := p.kind(t, contract.KindAssistantText, contract.OriginRecord, "in-ping"); o.Entry == "" {
		t.Errorf("assistant_text %s has no entry", o.ID)
	}

	// A tool call, from the transcript and from the hooks.
	send("in-tool", "TOOL echo hi")
	if d := ended("in-tool", contract.OriginLive); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "TOOL DONE: hi") {
		t.Errorf("tool turn %+v", d)
	}
	use := p.kind(t, contract.KindToolUse, contract.OriginRecord, "in-tool")
	var ud contract.ToolUseData
	_ = use.Decode(&ud)
	if ud.Name != "Bash" || ud.ToolUseID == "" || use.ID != "tool_use:"+ud.ToolUseID {
		t.Errorf("tool_use %s %+v", use.ID, ud)
	}
	p.await(t, "tool_result of "+ud.ToolUseID, func(o contract.Observation) bool {
		return o.Kind == contract.KindToolResult && o.ID == "tool_result:"+ud.ToolUseID
	})
	p.await(t, "tool_started of "+ud.ToolUseID, func(o contract.Observation) bool {
		return o.Kind == contract.KindToolStarted && o.ID == "tool_started:"+ud.ToolUseID && o.Origin == contract.OriginRecord
	})
	fin := p.await(t, "tool_finished of "+ud.ToolUseID, func(o contract.Observation) bool {
		return o.Kind == contract.KindToolFinished && o.ID == "tool_finished:"+ud.ToolUseID
	})
	var fd contract.ToolFinishedData
	_ = fin.Decode(&fd)
	if fd.Failed || !strings.Contains(fd.Output, "hi") {
		t.Errorf("tool_finished %+v", fd)
	}

	// Interrupts mid-reply and before the first token stop the turn.
	for _, c := range []struct{ id, text string }{{"in-slow", "SLOW 50"}, {"in-stall", "STALL 30"}} {
		send(c.id, c.text)
		p.kind(t, contract.KindTurnStarted, contract.OriginLive, c.id)
		time.Sleep(300 * time.Millisecond)
		out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: c.id, DeadlineMS: 15000})
		if err != nil || out != contract.InterruptStopped {
			t.Errorf("interrupting %s: %v %v, want stopped", c.text, out, err)
		}
		if d := ended(c.id, contract.OriginRecord); d.Outcome != contract.TurnInterrupted {
			t.Errorf("%s: record turn_ended %+v", c.text, d)
		}
	}

	// An exhausted 529 is overloaded, live and in the record.
	send("in-529", "ERR 529 99")
	for _, origin := range []contract.Origin{contract.OriginLive, contract.OriginRecord} {
		if d := ended("in-529", origin); d.Outcome != contract.TurnErrored || d.Error == nil || d.Error.Class != contract.ErrorOverloaded {
			t.Errorf("ERR 529: %s turn_ended %+v", origin, d)
		}
	}
	var ad contract.APIErrorData
	_ = p.kind(t, contract.KindAPIError, contract.OriginRecord, "in-529").Decode(&ad)
	if ad.HTTPStatus != 529 || ad.Message == "" {
		t.Errorf("api_error %+v", ad)
	}

	// A usage wall errors the turn and blocks the Session until its reset.
	send("in-limit", "LIMIT")
	d := ended("in-limit", contract.OriginLive)
	if d.Outcome != contract.TurnErrored || d.Error == nil || d.Error.Class != contract.ErrorUsageLimit || d.Error.ResumeAt == nil {
		t.Errorf("LIMIT: turn_ended %+v", d)
	}
	var rl contract.RateLimitData
	_ = p.kind(t, contract.KindRateLimit, contract.OriginLive, "").Decode(&rl)
	var b contract.Block
	_ = p.kind(t, contract.KindBlocked, contract.OriginLive, "").Decode(&b)
	if b.Reason != contract.BlockUsageLimited || b.ResumeAt == nil || time.Until(*b.ResumeAt) < 30*time.Minute {
		t.Errorf("blocked %+v", b)
	}
	if st := s.State(); st.Phase != contract.PhaseBlocked {
		t.Errorf("after the wall the Session is %s", st.Phase)
	}

	// The spool holds nothing the Host has acknowledged.
	res, err := s.Close(ctx, contract.ClosePark, 10*time.Second)
	if err != nil || !res.Stopped || !res.Drained {
		t.Errorf("Close = %+v %v", res, err)
	}
	cfg, _ := parseOpenConfig(ag.result.OpenConfig)
	for _, dir := range []string{cfg.Spool, sessionSpool(cfg.Spool, opened.SessionID)} {
		left, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		if len(left) > 0 {
			t.Errorf("spool files left after a drained close: %v", left)
		}
	}
	if opened.SessionID == "" {
		t.Error("Open returned no session id")
	}
}

func mustMarkers(t *testing.T, l contract.Layout) *adapter.Markers {
	t.Helper()
	m, err := adapter.OpenMarkers(l.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A reopen of a session claude never wrote a transcript for opens it under its
// id and runs its turns; the reopen after that resumes the transcript.
func TestReopenWithoutTranscript(t *testing.T) {
	bin := realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	ag := newLiveAgent(t, distribution(t, bin), mock)
	a := adapter.New(Profile{})
	ctx := context.Background()
	id := "5e1ad6a8-8f4e-4b43-9a4f-0c7a1d2b3c4d"
	for i, input := range []string{"in-first", "in-second"} {
		s, err := a.NewSession(contract.OpenRequest{Mode: contract.OpenReopen, SessionID: id, OpenConfig: ag.result.OpenConfig, Layout: ag.layout, Credential: ag.cred})
		if err != nil {
			t.Fatal(err)
		}
		opened, err := s.Open(ctx)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		if opened.SessionID != id {
			t.Errorf("reopen %d opened %s, want %s", i, opened.SessionID, id)
		}
		p := startPump(t, s)
		if _, err := s.Send(ctx, contract.Text(input, "PING "+input)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		var d contract.TurnEndedData
		_ = p.kind(t, contract.KindTurnEnded, contract.OriginLive, input).Decode(&d)
		if d.Outcome != contract.TurnCompleted || d.Text != "PONG "+input {
			t.Errorf("reopen %d: turn_ended %+v", i, d)
		}
		_, _ = s.Close(ctx, contract.ClosePark, 5*time.Second)
		p.halt()
	}
}
