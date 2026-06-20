package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// quiesceConv builds a Conversation wired with a real screen + claude-code
// adapter and one in-flight assistant turn started far enough in the past that
// the age guard never blocks. The frame is the current rendered screen.
func quiesceConv(t *testing.T, frame string, startedAgo time.Duration) *Conversation {
	t.Helper()
	ctx := context.Background()
	store := newFakeStore()
	sess := &Session{ID: "quiesce-session"}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	turn := &Turn{ID: "turn-1", SessionID: sess.ID, Role: RoleAssistant, State: TurnStateStreaming, StartedAt: time.Now().Add(-startedAgo)}
	if err := store.AppendTurn(ctx, turn); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	sc := screen.New(120, 40)
	_, _ = sc.Write([]byte(frame))
	return &Conversation{
		opts:        Options{Harness: "claude-code"},
		store:       store,
		adapter:     claudecode.New(),
		screen:      sc,
		eventCh:     make(chan ConversationEvent, 4),
		closed:      make(chan struct{}),
		markerArmCh: make(chan struct{}, 1),
		currentTurn: turn,
	}
}

func completedEvent(c *Conversation) (ConversationEvent, bool) {
	select {
	case ev := <-c.eventCh:
		return ev, true
	default:
		return ConversationEvent{}, false
	}
}

// A claude-code end-of-turn marker must NOT complete the turn instantly — it
// defers to quiescence so an intermediate marker (followed by more work) can't
// cut the turn off mid-flight. The turn stays in flight and the watcher is armed.
func TestMarker_defersInsteadOfInstantComplete(t *testing.T) {
	c := quiesceConv(t, "✻ Pondered for 3s\n✶ Cerebrating… (57s · ↓ 4.8k tokens)\n❯ ", 5*time.Second)
	c.handleTurnsEvent(turns.Event{Kind: turns.TurnComplete, At: time.Now(), Reason: "claude-code: ✻ Pondered for 3s"})

	if ev, ok := completedEvent(c); ok {
		t.Fatalf("marker must not complete the turn instantly; got event state=%q", ev.Turn.State)
	}
	c.mu.Lock()
	inFlight := c.currentTurn != nil
	armed := c.endMarkerSeen
	c.mu.Unlock()
	if !inFlight {
		t.Fatal("turn must stay in flight after a marker (deferred to quiescence)")
	}
	if !armed {
		t.Fatal("endMarkerSeen must be set so the idle watcher confirms on the short gap")
	}
	select {
	case <-c.markerArmCh:
	default:
		t.Fatal("idle watcher should have been re-armed via markerArmCh")
	}
}

// Once a marker has been seen, maybeIdleComplete completes the turn ONLY when the
// screen has settled at a non-busy prompt. A still-working frame (spinner up)
// must not complete; a settled frame must — without needing readyForInput.
func TestMarker_completesOnlyWhenSettled(t *testing.T) {
	// Still working: the in-progress spinner is up → Busy → no completion.
	working := quiesceConv(t, "✶ Cerebrating… (57s · ↓ 4.8k tokens)\n  ◯ Explore  verify   24s · ↓ 35.8k tokens\n❯ ", 5*time.Second)
	working.mu.Lock()
	working.endMarkerSeen = true
	working.mu.Unlock()
	working.maybeIdleComplete()
	if ev, ok := completedEvent(working); ok {
		t.Fatalf("must not complete while the spinner is up (Busy); got state=%q", ev.Turn.State)
	}

	// Settled: no spinner, no "esc to interrupt" → not busy → completes (marker
	// path does not require the "Claude Code" header / readyForInput).
	settled := quiesceConv(t, "⏺ Here is the revised plan.\n✻ Synthesized for 2m 3s\n❯ \n⏵⏵ auto mode on · ← for agents", 5*time.Second)
	settled.mu.Lock()
	settled.endMarkerSeen = true
	settled.mu.Unlock()
	settled.maybeIdleComplete()
	ev, ok := completedEvent(settled)
	if !ok {
		t.Fatal("a marker confirmed at a settled, non-busy prompt must complete the turn")
	}
	if ev.Turn.State != TurnStateComplete {
		t.Fatalf("state = %q, want %q", ev.Turn.State, TurnStateComplete)
	}
	if !strings.Contains(ev.Turn.Reason, "marker confirmed") {
		t.Errorf("reason = %q, want marker-confirmed", ev.Turn.Reason)
	}
}

// The fallback path (no marker seen) still requires readyForInput — a settled
// frame that lacks the prompt-readiness anchors must not idle-complete.
func TestFallback_stillRequiresReadyForInput(t *testing.T) {
	// No "Claude Code" header → readyForInput is false for claude-code.
	c := quiesceConv(t, "⏺ some output\n✻ Mused for 4s\n", 10*time.Second)
	c.maybeIdleComplete() // endMarkerSeen is false → fallback path
	if ev, ok := completedEvent(c); ok {
		t.Fatalf("fallback must not complete without prompt-readiness; got state=%q", ev.Turn.State)
	}
}
