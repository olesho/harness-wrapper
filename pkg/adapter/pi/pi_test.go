package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
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

// liveAgent is one agent's roots, provisioned for the mock.
type liveAgent struct {
	layout contract.Layout
	result contract.ProvisionResult
	cred   *contract.CredentialFile
}

// newLiveAgent makes an agent's roots and renders pi's configuration into
// them, on model, with tune adjusting settings.json.
func newLiveAgent(t *testing.T, root, model string, mock *mockapi.Server, tune func(settings map[string]any)) liveAgent {
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
	res, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: root, Layout: l, Spec: contract.AgentSpec{
		Model: model, PermissionPosture: contract.PostureBypass, Credential: &contract.CredentialRef{Kind: CredentialKind},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pointAt(mock)(conformance.Testing(t), l, &res)
	for i, f := range res.Files {
		if f.Root == contract.RootConfig && f.Path == settingsFile && tune != nil {
			var s map[string]any
			if err := json.Unmarshal(f.Content(), &s); err != nil {
				t.Fatal(err)
			}
			tune(s)
			b, _ := json.Marshal(s)
			res.Files[i] = contract.TextFile(f.Root, f.Path, f.Mode, string(b))
		}
	}
	if err := conformance.Apply(l, res); err != nil {
		t.Fatal(err)
	}
	cred := filepath.Join(l.Secrets, CredentialKind)
	if err := os.WriteFile(cred, []byte("sk-mock-placeholder-not-a-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return liveAgent{layout: l, result: res, cred: &contract.CredentialFile{Kind: CredentialKind, File: cred}}
}

// open opens a Session for the agent and pumps it.
func (ag liveAgent) open(t *testing.T, req contract.OpenRequest) (contract.Session, *pump) {
	t.Helper()
	req.OpenConfig, req.Layout, req.Credential = ag.result.OpenConfig, ag.layout, ag.cred
	s, err := adapter.New(Profile{}).NewSession(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	p := startPump(t, s)
	t.Cleanup(func() {
		_, _ = s.Close(context.Background(), contract.ClosePark, 0)
		p.halt()
	})
	return s, p
}

func send(t *testing.T, s contract.Session, id, text string) {
	t.Helper()
	if _, err := s.Send(context.Background(), contract.Text(id, text)); err != nil {
		t.Fatalf("Send %s: %v", text, err)
	}
}

// An interrupt while pi waits to retry a failed call ends the wait: the
// failed attempt is already dropped and pi writes nothing more for the run.
// The run is interrupted, live; in the record, once the next input's user
// message says the run is over, by the note the profile kept.
func TestPiInterruptsARetry(t *testing.T) {
	bin := realPi(t)
	mock := mockapi.Start()
	defer mock.Close()
	ag := newLiveAgent(t, distribution(t, bin), "anthropic/claude-haiku-4-5", mock, func(s map[string]any) {
		s["retry"].(map[string]any)["baseDelayMs"] = 30000
	})
	s, p := ag.open(t, contract.OpenRequest{Mode: contract.OpenFresh})

	send(t, s, "in-1", "ERR 529 99")
	p.await(t, "a retry of in-1", func(o contract.Observation) bool {
		return o.Kind == contract.KindRetrying && o.InputID == "in-1"
	})
	out, err := s.Interrupt(context.Background(), contract.InterruptRequest{InputID: "in-1", DeadlineMS: 20000})
	if err != nil || out != contract.InterruptStopped {
		t.Fatalf("Interrupt: %s %v, want stopped", out, err)
	}
	if d := p.ended(t, "in-1", contract.OriginLive); d.Outcome != contract.TurnInterrupted {
		t.Errorf("in-1, live: %+v, want interrupted", d)
	}
	send(t, s, "in-2", "PING 2")
	if d := p.ended(t, "in-2", contract.OriginLive); d.Outcome != contract.TurnCompleted {
		t.Errorf("in-2, live: %+v, want completed", d)
	}
	if d := p.ended(t, "in-1", contract.OriginRecord); d.Outcome != contract.TurnInterrupted {
		t.Errorf("in-1, in the record: %+v, want interrupted", d)
	}
	if d := p.ended(t, "in-2", contract.OriginRecord); d.Outcome != contract.TurnCompleted {
		t.Errorf("in-2, in the record: %+v, want completed", d)
	}
}
