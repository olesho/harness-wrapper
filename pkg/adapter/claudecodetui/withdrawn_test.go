package claudecodetui

import (
	"context"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Against the pinned claude on a terminal and the mock: a prompt interrupted
// before its first token — the request answered with message_start alone
// (STALL), or not at all (HUSH) — is withdrawn by claude, which writes no end
// for its turn. Once the next prompt is written, the record ends the turn
// interrupted, and Recover says so.
func TestClaudeTUIWithdrawnPrompt(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	run := tuiRunAt(t, mock, contract.PostureBypass)
	s, p := run.s, run.p
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
	ended := func(id string, origin contract.Origin) contract.TurnEndedData {
		t.Helper()
		var d contract.TurnEndedData
		_ = p.kind(t, contract.KindTurnEnded, origin, id).Decode(&d)
		return d
	}

	// The session's first prompt is not withdrawn so: the next follows it.
	send("in-ping", "PING 0")
	ended("in-ping", contract.OriginLive)
	withdrawn := []string{"in-stall", "in-hush"}
	for i, text := range []string{"STALL 30", "HUSH 30"} {
		id := withdrawn[i]
		send(id, text)
		p.kind(t, contract.KindTurnStarted, contract.OriginLive, id)
		time.Sleep(300 * time.Millisecond)
		out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: id, DeadlineMS: 15000})
		if err != nil || out != contract.InterruptStopped {
			t.Fatalf("interrupting %s: %v %v, want stopped", text, out, err)
		}
		next := id + "-next"
		send(next, "PING "+next)
		if d := ended(next, contract.OriginRecord); d.Outcome != contract.TurnCompleted {
			t.Errorf("%s: record turn_ended %+v", next, d)
		}
		if d := ended(id, contract.OriginRecord); d.Outcome != contract.TurnInterrupted {
			t.Errorf("%s: record turn_ended %+v, want interrupted", text, d)
		}
	}

	rec, err := adapter.New(Profile{}).OpenRecord(ctx, contract.RecordRequest{SessionID: run.sessionID, OpenConfig: run.openConfig, Layout: run.layout})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rec.Close() }()
	for _, id := range withdrawn {
		if got, err := rec.Recover(ctx, id); err != nil || got.Outcome != contract.RecoveredInterrupted {
			t.Errorf("Recover(%s) = %+v %v, want interrupted", id, got, err)
		}
	}
}
