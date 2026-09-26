package chat

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/delivery"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

const (
	capturedID = "74ca2184-c064-492c-88dc-c79c128de13e"
	staleID    = "00000000-0000-0000-0000-00000000dead"
)

// bothPathsAdapter recovers the harness session id on both capture paths: from
// a raw output line ("session <id>"), and by scraping the screen. The scrape
// signals scraping when it starts and returns what the test sends on scraped,
// which holds it open for as long as the test needs.
type bothPathsAdapter struct {
	*generic.Adapter
	scraping chan struct{}
	scraped  chan string
}

func (a *bothPathsAdapter) ExtractSessionID(screen.Snapshot) (string, bool) {
	close(a.scraping)
	return <-a.scraped, true
}

func (a *bothPathsAdapter) ExtractSessionIDFromLine(line string) (string, bool) {
	return strings.CutPrefix(line, "session ")
}

// A screen scrape that was still running when the line tap captured the id
// used to write its own, stale result over it — in memory and in the Store —
// because it checked for an id before it looked and never after. History then
// read another session's transcript.
func TestLateScrapeDoesNotReplaceCapturedSessionID(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	sess := Session{ID: "chat-race"}
	if err := store.CreateSession(ctx, &sess); err != nil {
		t.Fatal(err)
	}
	adapter := &bothPathsAdapter{Adapter: generic.New(), scraping: make(chan struct{}), scraped: make(chan string)}
	c := &Conversation{adapter: adapter, screen: screen.New(80, 24), store: store, session: sess}

	scrapeDone := make(chan struct{})
	go func() {
		defer close(scrapeDone)
		c.maybeExtractSessionID()
	}()
	<-adapter.scraping // the scrape found no id recorded and is looking
	c.captureRawSessionID("session " + capturedID)
	adapter.scraped <- staleID
	<-scrapeDone

	if got := c.State().HarnessSessionID; got != capturedID {
		t.Errorf("in-memory harness session id = %q, want the captured %q", got, capturedID)
	}
	if got, _ := store.GetSession(ctx, sess.ID); got.HarnessID() != capturedID {
		t.Errorf("stored harness session id = %q, want the captured %q", got.HarnessID(), capturedID)
	}
}

var errStoreDown = errors.New("store down")

// flakyStore fails UpdateSession while down is set, and counts the calls.
type flakyStore struct {
	*fakeStore
	fmu     sync.Mutex
	down    bool
	updates int
}

func (f *flakyStore) setDown(down bool) {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.down = down
}

func (f *flakyStore) calls() int {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return f.updates
}

func (f *flakyStore) UpdateSession(ctx context.Context, s *Session) error {
	f.fmu.Lock()
	f.updates++
	down := f.down
	f.fmu.Unlock()
	if down {
		return errStoreDown
	}
	return f.fakeStore.UpdateSession(ctx, s)
}

func newFlakyConversation(t *testing.T) (*Conversation, *flakyStore) {
	t.Helper()
	store := &flakyStore{fakeStore: newFakeStore()}
	sess := Session{ID: "chat-flaky"}
	if err := store.CreateSession(context.Background(), &sess); err != nil {
		t.Fatal(err)
	}
	adapter := &bothPathsAdapter{Adapter: generic.New(), scraping: make(chan struct{}), scraped: make(chan string)}
	return &Conversation{adapter: adapter, screen: screen.New(80, 24), store: store, session: sess}, store
}

func storedHarnessID(t *testing.T, store Store, id string) string {
	t.Helper()
	s, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.HarnessID()
}

// A harness session id whose write to the Store failed used to be lost for
// good: the setter recorded it in memory first, dropped the Store's error, and
// every later attempt returned at once because the in-memory id was set. The
// conversation kept reading its transcript; a Reopen, or History after it
// ended, could not.
func TestUnsavedSessionIDIsRetried(t *testing.T) {
	c, store := newFlakyConversation(t)
	store.setDown(true)

	c.captureRawSessionID("session " + capturedID)
	if got := storedHarnessID(t, store, c.session.ID); got != "" {
		t.Fatalf("stored harness session id = %q while the store is down", got)
	}
	st := c.State()
	if st.HarnessSessionID != capturedID {
		t.Fatalf("in-memory harness session id = %q, want %q", st.HarnessSessionID, capturedID)
	}
	if !errors.Is(st.HarnessSessionIDErr, ErrHarnessSessionIDNotSaved) || !errors.Is(st.HarnessSessionIDErr, errStoreDown) {
		t.Fatalf("State().HarnessSessionIDErr = %v, want ErrHarnessSessionIDNotSaved wrapping the store's error", st.HarnessSessionIDErr)
	}

	// Another capture neither replaces the id nor hammers the store: the line
	// tap runs once per output line.
	before := store.calls()
	c.captureRawSessionID("session " + staleID)
	if store.calls() != before {
		t.Errorf("the line tap wrote to the store again (%d calls, was %d)", store.calls(), before)
	}

	// A turn's end retries the write, which now succeeds.
	store.setDown(false)
	c.maybeExtractSessionID()
	if got := storedHarnessID(t, store, c.session.ID); got != capturedID {
		t.Fatalf("stored harness session id = %q after the retry, want %q", got, capturedID)
	}
	if err := c.State().HarnessSessionIDErr; err != nil {
		t.Errorf("State().HarnessSessionIDErr = %v after the id was saved, want nil", err)
	}
	// Saved: no more writes.
	before = store.calls()
	c.maybeExtractSessionID()
	if store.calls() != before {
		t.Errorf("maybeExtractSessionID wrote a saved id again")
	}
}

// exitConversation readies c for exitWith and returns the events it delivers.
func exitConversation(c *Conversation) <-chan ConversationEvent {
	got := make(chan ConversationEvent, 4)
	c.done = make(chan struct{})
	c.delivery = delivery.New(delivery.Limits(wrapper.QueueLimits{}), eventSize, func(ev ConversationEvent) { got <- ev })
	return got
}

func exitedEvent(t *testing.T, got <-chan ConversationEvent) ConversationEvent {
	t.Helper()
	select {
	case ev := <-got:
		if ev.Type != EventExited {
			t.Fatalf("event %q, want %q", ev.Type, EventExited)
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("EventExited never delivered")
		return ConversationEvent{}
	}
}

// The harness's exit is the last chance to save an id the Store refused: it is
// retried there, and if the Store still refuses it EventExited says so, so the
// caller learns that the stored record cannot be resumed.
func TestUnsavedSessionIDAtExit(t *testing.T) {
	t.Run("still refused", func(t *testing.T) {
		c, store := newFlakyConversation(t)
		store.setDown(true)
		c.captureRawSessionID("session " + capturedID)
		got := exitConversation(c)
		before := store.calls()

		c.exitWith(ExitInfo{Status: wrapper.StatusIdle})
		ev := exitedEvent(t, got)
		if store.calls() == before {
			t.Error("exit did not retry the write")
		}
		if !errors.Is(ev.Err, ErrHarnessSessionIDNotSaved) || !errors.Is(ev.Err, errStoreDown) {
			t.Fatalf("EventExited.Err = %v, want ErrHarnessSessionIDNotSaved wrapping the store's error", ev.Err)
		}
		if !strings.Contains(ev.Err.Error(), capturedID) {
			t.Errorf("EventExited.Err = %q, want it to name the id", ev.Err)
		}
	})
	t.Run("saved at exit", func(t *testing.T) {
		c, store := newFlakyConversation(t)
		store.setDown(true)
		c.captureRawSessionID("session " + capturedID)
		got := exitConversation(c)
		store.setDown(false)

		c.exitWith(ExitInfo{Status: wrapper.StatusIdle})
		if ev := exitedEvent(t, got); ev.Err != nil {
			t.Fatalf("EventExited.Err = %v, want nil once the id is saved", ev.Err)
		}
		if got := storedHarnessID(t, store, c.session.ID); got != capturedID {
			t.Fatalf("stored harness session id = %q, want %q", got, capturedID)
		}
	})
}

// recordLaunch and the id's save both write the whole session record. Neither
// may drop the other's change, whichever runs last.
func TestSessionWritesKeepEachOthersChanges(t *testing.T) {
	c, store := newFlakyConversation(t)
	c.captureRawSessionID("session " + capturedID)

	c.mu.Lock()
	c.session.WorkingDir = "/changed/by/another/writer"
	c.mu.Unlock()
	if err := c.saveSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := store.GetSession(context.Background(), c.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.HarnessID() != capturedID || s.WorkingDir != "/changed/by/another/writer" {
		t.Fatalf("stored record = %+v, want both the id and the other writer's change", s)
	}
}
