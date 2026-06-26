package chat

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
)

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

	dir := filepath.Join(sessionsRoot, "2026", "06", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"2026-06-26T05:25:23.303Z","type":"session_meta","payload":{"session_id":"` + uuid + `","cwd":"` + cwd + `","cli_version":"0.142.0"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-06-26T07-25-23-"+uuid+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

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
