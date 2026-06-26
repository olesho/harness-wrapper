package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
)

// writeCodexRollout writes a minimal codex rollout under sessionsRoot whose
// session_meta carries the given session id + cwd, so the disk-based
// SessionIDLocator can recover the id. Returns the session id for convenience.
func writeCodexRollout(t *testing.T, sessionsRoot, sessionID, cwd string) {
	t.Helper()
	dir := filepath.Join(sessionsRoot, "2026", "06", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"2026-06-26T05:25:23.303Z","type":"session_meta","payload":{"session_id":"` + sessionID + `","cwd":"` + cwd + `","cli_version":"0.142.0"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-06-26T07-25-23-"+sessionID+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestMaybeExtractSessionID_CodexDiskFallback is the integration-level
// regression for the Codex 0.142 session-id gap: the screen never renders the
// "codex resume <uuid>" hint, so the screen scrape returns nothing — yet the
// conversation must still recover the id from the on-disk rollout's
// session_meta (keyed on WorkingDir) and persist it. Without the fallback,
// History() short-circuits to the truncated screen scrape.
func TestMaybeExtractSessionID_CodexDiskFallback(t *testing.T) {
	cwd := t.TempDir()
	sessionsRoot := t.TempDir()
	const uuid = "019f0263-cdb9-7013-a43a-4eb1f65d94f1"
	writeCodexRollout(t, sessionsRoot, uuid, cwd)

	adapter := codex.New()
	adapter.SessionsRoot = sessionsRoot

	store := newFakeStore()
	sess := Session{ID: "chat-sess-codex", Harness: "codex"}
	if err := store.CreateSession(context.Background(), &sess); err != nil {
		t.Fatal(err)
	}

	// A blank screen — no resume hint, exactly the 0.142 condition.
	scr := screen.New(120, 40)
	c := &Conversation{
		opts:    Options{Harness: "codex", WorkingDir: cwd},
		adapter: adapter,
		screen:  scr,
		store:   store,
		session: sess,
	}

	c.maybeExtractSessionID()

	if c.session.HarnessSessionID != uuid {
		t.Fatalf("in-memory HarnessSessionID = %q, want %q (recovered from disk)", c.session.HarnessSessionID, uuid)
	}
	if got, _ := store.GetSession(context.Background(), sess.ID); got.HarnessSessionID != uuid {
		t.Fatalf("persisted HarnessSessionID = %q, want %q", got.HarnessSessionID, uuid)
	}
}

// TestMaybeIdleComplete_RecoversCodexSessionID is the regression for the
// one-shot-run gap: on Codex 0.142 a turn completes via the idle fallback
// (maybeIdleComplete) rather than a Token-usage TurnComplete event, and the
// screen carries no resume hint. The idle path must still recover the session id
// from the on-disk rollout — otherwise History silently degrades to the lossy
// screen scrape (HistorySourceStore) even though the transcript exists. Before
// the fix this asserted-on id stayed empty because only the TurnComplete-event
// path extracted it.
func TestMaybeIdleComplete_RecoversCodexSessionID(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionsRoot := t.TempDir()
	const uuid = "019f0287-aaaa-7013-a43a-4eb1f65d94f1"
	writeCodexRollout(t, sessionsRoot, uuid, cwd)

	adapter := codex.New()
	adapter.SessionsRoot = sessionsRoot

	store := newFakeStore()
	sess := &Session{ID: "idle-codex"}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	// In-flight turn started well past idleCompletionGap so the age guard passes.
	turn := &Turn{ID: "turn-1", SessionID: sess.ID, Role: RoleAssistant, State: TurnStateStreaming, StartedAt: time.Now().Add(-30 * time.Second)}
	if err := store.AppendTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}

	// A settled 0.142 codex screen: the "›" composer is ready (readyForInput),
	// there is reply text, and crucially NO "codex resume <uuid>" hint — so the
	// screen-scrape extractor finds nothing and only the disk locator can recover
	// the id.
	scr := screen.New(120, 40)
	if _, err := scr.Write([]byte("Codex\n\nDone — committed the change.\n\n› \n")); err != nil {
		t.Fatal(err)
	}

	c := &Conversation{
		opts:        Options{Harness: "codex", WorkingDir: cwd},
		store:       store,
		adapter:     adapter,
		screen:      scr,
		session:     *sess,
		eventCh:     make(chan ConversationEvent, 4),
		closed:      make(chan struct{}),
		markerArmCh: make(chan struct{}, 1),
		currentTurn: turn,
	}

	c.maybeIdleComplete()

	ev, ok := completedEvent(c)
	if !ok {
		t.Fatal("idle completion did not complete the codex turn")
	}
	if ev.Turn.State != TurnStateComplete {
		t.Fatalf("turn state = %q, want %q", ev.Turn.State, TurnStateComplete)
	}
	if c.session.HarnessSessionID != uuid {
		t.Fatalf("idle completion did not recover the session id from disk: got %q, want %q", c.session.HarnessSessionID, uuid)
	}
	if got, _ := store.GetSession(ctx, sess.ID); got.HarnessSessionID != uuid {
		t.Fatalf("persisted HarnessSessionID = %q, want %q", got.HarnessSessionID, uuid)
	}
}
