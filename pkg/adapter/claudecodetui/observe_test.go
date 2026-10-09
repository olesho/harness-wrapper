package claudecodetui

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
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
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

// tuiSession opens a fresh Session of the profile against the mock.
func tuiSession(t *testing.T, mock *mockapi.Server) (contract.Session, *pump) {
	t.Helper()
	root := distribution(t, realClaude(t))
	// claude keys its workspace trust by the real path.
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
		PermissionPosture: contract.PostureBypass, Credential: &contract.CredentialRef{Kind: claudecode.CredentialKind},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pointAt(mock)(conformance.Testing(t), l, &res)
	if err := conformance.Apply(l, res); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(l.Secrets, claudecode.CredentialKind)
	if err := os.WriteFile(cred, []byte("mock-placeholder-not-a-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := a.NewSession(contract.OpenRequest{
		Mode: contract.OpenFresh, OpenConfig: res.OpenConfig, Layout: l,
		Credential: &contract.CredentialFile{Kind: claudecode.CredentialKind, File: cred},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := startPump(t, s)
	t.Cleanup(func() {
		_, _ = s.Close(context.Background(), contract.ClosePark, 5*time.Second)
		p.halt()
	})
	return s, p
}

// Against the pinned claude on a terminal and the mock: a tool call is
// observed started and finished, from the hooks, keyed by its tool use id as
// under stream-json; and a subagent started and stopped, keyed by its agent
// id. claude's TUI runs the subagent in the background (claude 2.1.283): the
// input's turn ends once it is launched, and claude takes its result up in a
// turn of its own (probes/tui-hybrid, 2026-10-09).
func TestClaudeTUIObservations(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	s, p := tuiSession(t, mock)
	ctx := context.Background()
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
	ended := func(id string) contract.TurnEndedData {
		t.Helper()
		var d contract.TurnEndedData
		_ = p.await(t, "turn_ended of "+id, func(o contract.Observation) bool {
			return o.Kind == contract.KindTurnEnded && o.Origin == contract.OriginLive && o.InputID == id
		}).Decode(&d)
		return d
	}

	send("in-tool", "TOOL echo tui-tool")
	if d := ended("in-tool"); d.Outcome != contract.TurnCompleted || !strings.Contains(d.Text, "TOOL DONE: tui-tool") {
		t.Errorf("tool turn %+v", d)
	}
	var ud contract.ToolUseData
	_ = p.await(t, "tool_use", func(o contract.Observation) bool {
		return o.Kind == contract.KindToolUse && o.InputID == "in-tool"
	}).Decode(&ud)
	if ud.Name != "Bash" || ud.ToolUseID == "" {
		t.Fatalf("tool_use %+v", ud)
	}
	var sd contract.ToolUseData
	_ = p.await(t, "tool_started of "+ud.ToolUseID, func(o contract.Observation) bool {
		return o.Kind == contract.KindToolStarted && o.ID == "tool_started:"+ud.ToolUseID
	}).Decode(&sd)
	if sd.Name != "Bash" || !strings.Contains(string(sd.Input), "tui-tool") {
		t.Errorf("tool_started %+v", sd)
	}
	var fd contract.ToolFinishedData
	_ = p.await(t, "tool_finished of "+ud.ToolUseID, func(o contract.Observation) bool {
		return o.Kind == contract.KindToolFinished && o.ID == "tool_finished:"+ud.ToolUseID
	}).Decode(&fd)
	if fd.Failed || !strings.Contains(fd.Output, "tui-tool") {
		t.Errorf("tool_finished %+v", fd)
	}

	send("in-agent", "AGENT PING 7")
	if d := ended("in-agent"); d.Outcome != contract.TurnCompleted {
		t.Errorf("agent turn %+v", d)
	}
	var started, stopped contract.SubagentData
	_ = p.await(t, "subagent_started", func(o contract.Observation) bool { return o.Kind == contract.KindSubagentStarted }).Decode(&started)
	_ = p.await(t, "subagent_stopped", func(o contract.Observation) bool { return o.Kind == contract.KindSubagentStopped }).Decode(&stopped)
	if started.SubagentID == "" || started.Type != "general-purpose" || stopped.SubagentID != started.SubagentID || stopped.LastMessage != "PONG 7" {
		t.Errorf("subagent started %+v, stopped %+v", started, stopped)
	}
	// claude takes the subagent's result up in a turn of its own.
	own := adapter.AutoTurnID(claudecode.OwnNative(started.SubagentID))
	var d contract.TurnEndedData
	_ = p.await(t, "the end of claude's own turn "+own, func(o contract.Observation) bool {
		return o.Kind == contract.KindTurnEnded && o.Origin == contract.OriginLive && o.TurnID == own && o.InputID == ""
	}).Decode(&d)
	if d.Outcome != contract.TurnCompleted || d.Text != "BG DONE" {
		t.Errorf("claude's own turn ended %+v", d)
	}
}
