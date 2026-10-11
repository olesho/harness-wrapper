package claudecode

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// Against the pinned claude and the mock: under stream-json an interrupt
// before the first token leaves claude's interrupt entry, after the
// prompt's entry when claude wrote one, and the next prompt follows it — no
// prompt is withdrawn. A turn whose prompt is in the record ends interrupted
// there, and Recover says so. One interrupted before claude wrote its prompt
// — often at once, now and then 300 ms in — has nothing in the record:
// Recover cannot say, and the next input's turn is the next input's alone.
func TestClaudePreTokenRecord(t *testing.T) {
	bin := realClaude(t)
	mock := mockapi.Start()
	defer mock.Close()
	ag := newLiveAgent(t, distribution(t, bin), mock)
	a := adapter.New(Profile{})
	ctx := context.Background()
	s, err := a.NewSession(contract.OpenRequest{Mode: contract.OpenFresh, OpenConfig: ag.result.OpenConfig, Layout: ag.layout, Credential: ag.cred})
	if err != nil {
		t.Fatal(err)
	}
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

	send("in-ping", "PING 0")
	p.kind(t, contract.KindTurnEnded, contract.OriginLive, "in-ping")
	cases := []struct {
		id, text string
		after    time.Duration
	}{
		{"in-stall", "STALL 30", 300 * time.Millisecond},
		{"in-hush", "HUSH 30", 0},
	}
	for i, c := range cases {
		send(c.id, c.text)
		p.kind(t, contract.KindTurnStarted, contract.OriginLive, c.id)
		time.Sleep(c.after)
		out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: c.id, DeadlineMS: 15000})
		if err != nil || out != contract.InterruptStopped {
			t.Fatalf("interrupting %s: %v %v, want stopped", c.text, out, err)
		}
		next, want := c.id+"-next", "PONG "+string(rune('1'+i))
		send(next, "PING "+want[5:])
		var d contract.TurnEndedData
		_ = p.kind(t, contract.KindTurnEnded, contract.OriginRecord, next).Decode(&d)
		if d.Outcome != contract.TurnCompleted || d.Text != want {
			t.Errorf("%s: record turn_ended %+v", next, d)
		}
	}
	time.Sleep(time.Second)

	rec, err := a.OpenRecord(ctx, contract.RecordRequest{SessionID: opened.SessionID, OpenConfig: ag.result.OpenConfig, Layout: ag.layout})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Close() }()
	for _, c := range cases {
		written, end := false, contract.TurnOutcome("")
		p.mu.Lock()
		for _, o := range p.seen {
			if o.InputID != c.id || o.Origin != contract.OriginRecord {
				continue
			}
			switch o.Kind {
			case contract.KindUserInput:
				written = true
			case contract.KindTurnEnded:
				var d contract.TurnEndedData
				_ = o.Decode(&d)
				end = d.Outcome
			}
		}
		p.mu.Unlock()
		got, err := rec.Recover(ctx, c.id)
		switch {
		case err != nil:
			t.Errorf("Recover(%s): %v", c.id, err)
		case written && (end != contract.TurnInterrupted || got.Outcome != contract.RecoveredInterrupted):
			t.Errorf("%s's prompt is in the record: ended %q, Recover %s; want interrupted", c.id, end, got.Outcome)
		case !written && (end != "" || got.Outcome != contract.RecoveredUnknown):
			t.Errorf("%s's prompt is not in the record: ended %q, Recover %s; want no end, unknown", c.id, end, got.Outcome)
		}
		t.Logf("%s: prompt written %v, Recover %s", c.text, written, got.Outcome)
	}
	if t.Failed() {
		cfg, _ := parseOpenConfig(ag.result.OpenConfig)
		if f, err := claudecode.FollowEntries(opened.SessionID, cfg.WorkingDir, cfg.Env, transcript.Checkpoint{}); err == nil {
			data, _ := os.ReadFile(f.Path())
			t.Logf("the transcript:\n%s", data)
		}
	}
}
