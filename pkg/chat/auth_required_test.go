package chat

import (
	"context"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// A turn that errors while the terminal screen shows a logged-out / re-auth
// banner is not a task failure — the harness CLI is logged out. handleTurnsEvent
// must relabel such an errored turn with the canonical ReasonAuthRequired (a
// machine-matchable "auth_required:" reason) instead of the generic one, at both
// the ev.Snap and the live-screen fallback sources. It must NOT touch a turn that
// errored for any other reason, and it must never convert a success to a failure.

func newAuthTestConv(t *testing.T, harness string, scr *screen.Screen) (*Conversation, *Turn) {
	t.Helper()
	ctx := context.Background()
	store := newFakeStore()
	sess := &Session{ID: "auth-session"}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	turn := &Turn{
		ID:        "turn-1",
		SessionID: sess.ID,
		Role:      RoleAssistant,
		State:     TurnStateStreaming,
		StartedAt: time.Now(),
	}
	if err := store.AppendTurn(ctx, turn); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	c := &Conversation{
		opts:        Options{Harness: harness},
		store:       store,
		session:     *sess,
		screen:      scr,
		eventCh:     make(chan ConversationEvent, 4),
		closed:      make(chan struct{}),
		currentTurn: turn,
	}
	return c, turn
}

func awaitTurn(t *testing.T, c *Conversation) Turn {
	t.Helper()
	select {
	case ev := <-c.eventCh:
		if ev.Err != nil {
			t.Fatalf("unexpected Err: %v", ev.Err)
		}
		return ev.Turn
	case <-time.After(time.Second):
		t.Fatal("no TurnEvent emitted within 1s")
		return Turn{}
	}
}

// The banner rides on ev.Snap (a screen-derived Errored event stamps it).
func TestHandleTurnsEvent_AuthRequiredFromSnap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		banner  string
	}{
		{"claude", chatClaudeCode, "Not logged in · Please run /login"},
		{"codex", "codex", "ERROR: unexpected status 401 Unauthorized: Missing bearer or basic authentication in header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newAuthTestConv(t, tc.harness, nil)
			c.handleTurnsEvent(turns.Event{
				Kind:   turns.Errored,
				At:     time.Now(),
				Reason: tc.harness + ": harness exited",
				Snap:   &screen.Snapshot{Text: tc.banner},
			})
			got := awaitTurn(t, c)
			if got.State != TurnStateErrored {
				t.Errorf("State = %q, want %q", got.State, TurnStateErrored)
			}
			if got.Reason != ReasonAuthRequired {
				t.Errorf("Reason = %q, want ReasonAuthRequired", got.Reason)
			}
		})
	}
}

// The status-derived Errored event carries no Snap; the reason is recovered from
// the live screen, which still shows the banner after the harness exits.
func TestHandleTurnsEvent_AuthRequiredFromLiveScreen(t *testing.T) {
	scr := screen.New(80, 24)
	if _, err := scr.Write([]byte("Not logged in · Please run /login")); err != nil {
		t.Fatalf("screen.Write: %v", err)
	}
	c, _ := newAuthTestConv(t, chatClaudeCode, scr)

	c.handleTurnsEvent(turns.Event{
		Kind:   turns.Errored,
		At:     time.Now(),
		Reason: "claude-code: harness exited", // no Snap
	})
	got := awaitTurn(t, c)
	if got.Reason != ReasonAuthRequired {
		t.Errorf("Reason = %q, want ReasonAuthRequired (from live screen)", got.Reason)
	}
}

// An ordinary errored turn (no banner) keeps its original reason — the auth relabel
// must not fire, and Blocked (cost/rate-limit) is never relabeled.
func TestHandleTurnsEvent_NonAuthErrorKeepsReason(t *testing.T) {
	c, _ := newAuthTestConv(t, chatClaudeCode, nil)
	const orig = "claude-code: harness exited"
	c.handleTurnsEvent(turns.Event{
		Kind:   turns.Errored,
		At:     time.Now(),
		Reason: orig,
		Snap:   &screen.Snapshot{Text: "⏺ done — all tests pass.\n❯ "},
	})
	got := awaitTurn(t, c)
	if got.Reason != orig {
		t.Errorf("Reason = %q, want unchanged %q", got.Reason, orig)
	}
}
