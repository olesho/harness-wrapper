package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns/generic"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// Quit writes the claude-code adapter's "/quit" sequence (slash command +
// enhanced Enter) through the held writer.
func TestQuit_SendsClaudeQuitSequence(t *testing.T) {
	rec := &keyRecorder{}
	c := &Conversation{
		opts:       Options{Harness: "claude-code"},
		adapter:    claudecode.New(),
		queue:      newControlQueue(),
		closed:     make(chan struct{}),
		writeStdin: rec.write,
	}
	if err := c.Quit(context.Background()); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	if got, want := string(rec.data), "/quit\x1b[13u"; got != want {
		t.Fatalf("Quit wrote %q, want %q", got, want)
	}
}

// Quit on an adapter with no QuitSequence (generic) reports ErrQuitUnsupported
// and writes nothing — the caller falls back to a signal via Close.
func TestQuit_UnsupportedAdapter(t *testing.T) {
	rec := &keyRecorder{}
	c := &Conversation{
		opts:       Options{Harness: "generic"},
		adapter:    generic.New(),
		queue:      newControlQueue(),
		closed:     make(chan struct{}),
		writeStdin: rec.write,
	}
	if err := c.Quit(context.Background()); !errors.Is(err, ErrQuitUnsupported) {
		t.Fatalf("Quit err = %v, want ErrQuitUnsupported", err)
	}
	if len(rec.data) != 0 {
		t.Fatalf("Quit wrote %q, want nothing", rec.data)
	}
}

// Quit after Close reports ErrClosed.
func TestQuit_AfterClose(t *testing.T) {
	c := &Conversation{
		opts:       Options{Harness: "claude-code"},
		adapter:    claudecode.New(),
		queue:      newControlQueue(),
		closed:     make(chan struct{}),
		writeStdin: (&keyRecorder{}).write,
	}
	close(c.closed)
	if err := c.Quit(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Quit err = %v, want ErrClosed", err)
	}
}

// captureRawSessionID records the harness session id the first time the
// "claude --resume <uuid>" exit hint appears in the raw line stream — tolerating
// trailing ANSI + CR — and persists it; non-matching lines are no-ops and the
// first capture wins.
func TestCaptureRawSessionID(t *testing.T) {
	const id = "74ca2184-c064-492c-88dc-c79c128de13e"
	store := newFakeStore()
	sess := Session{ID: "chat-sess-1", Harness: "claude-code"}
	if err := store.CreateSession(context.Background(), &sess); err != nil {
		t.Fatal(err)
	}
	c := &Conversation{
		opts:    Options{Harness: "claude-code"},
		adapter: claudecode.New(),
		store:   store,
		session: sess,
	}

	// A non-matching line is a no-op.
	c.captureRawSessionID("✻ Baked for 5s")
	if c.session.HarnessSessionID != "" {
		t.Fatalf("HarnessSessionID = %q after non-matching line, want empty", c.session.HarnessSessionID)
	}

	// The exit hint (with ANSI styling + CR riding along) is captured + persisted.
	c.captureRawSessionID("claude --resume " + id + "\x1b[22m\r")
	if c.session.HarnessSessionID != id {
		t.Fatalf("in-memory HarnessSessionID = %q, want %q", c.session.HarnessSessionID, id)
	}
	if got, _ := store.GetSession(context.Background(), sess.ID); got.HarnessSessionID != id {
		t.Fatalf("persisted HarnessSessionID = %q, want %q", got.HarnessSessionID, id)
	}

	// First capture wins: a later, different id does not overwrite.
	c.captureRawSessionID("claude --resume 00000000-0000-0000-0000-000000000000")
	if c.session.HarnessSessionID != id {
		t.Fatalf("HarnessSessionID changed to %q, want first %q", c.session.HarnessSessionID, id)
	}
}
