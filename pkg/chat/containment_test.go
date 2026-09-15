package chat_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// previousSession is chat.Session as the previous release declares it: what an
// older reader decodes a stored record into.
type previousSession struct {
	ID               string
	Harness          string
	WorkingDir       string
	CreatedAt        time.Time
	HarnessSessionID string
}

func containedRecord(harnessID string) chat.Session {
	return chat.Session{
		ID: "sess-contained", Harness: "claude-code", WorkingDir: "/repo",
		CreatedAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
		Containment: &chat.SessionContainment{
			Version:          chat.ContainmentRecordVersion,
			Required:         &containment.Request{Kind: containment.KindLandlock, MinABI: 9},
			Profile:          "claude-code@2.1.270",
			ProfileVersion:   1,
			StateID:          "abcdefghijkl",
			Resumable:        true,
			HarnessSessionID: harnessID,
		},
	}
}

// TestUncontainedRecordIsUnchanged pins G9 for stored records: an uncontained
// Session serializes exactly as the previous release's Session does.
func TestUncontainedRecordIsUnchanged(t *testing.T) {
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	cur, err := json.Marshal(chat.Session{ID: "s", Harness: "codex", WorkingDir: "/w", CreatedAt: now, HarnessSessionID: "h"})
	if err != nil {
		t.Fatal(err)
	}
	prev, _ := json.Marshal(previousSession{ID: "s", Harness: "codex", WorkingDir: "/w", CreatedAt: now, HarnessSessionID: "h"})
	if string(cur) != string(prev) {
		t.Fatalf("uncontained record changed:\n got %s\nwant %s", cur, prev)
	}
}

// TestDowngradeGuard: an older reader decoding a contained record — before or
// after the harness id was discovered — finds no harness session id, so its
// Reopen refuses (ErrNoHarnessSession) instead of resuming unrestricted.
func TestDowngradeGuard(t *testing.T) {
	for _, id := range []string{"", "11111111-2222-3333-4444-555555555555"} {
		rec := containedRecord(id)
		if rec.HarnessID() != id || rec.HarnessSessionID != "" {
			t.Fatalf("accessor/legacy mismatch: %q / %q", rec.HarnessID(), rec.HarnessSessionID)
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		var old previousSession
		if err := json.Unmarshal(b, &old); err != nil {
			t.Fatal(err)
		}
		if old.HarnessSessionID != "" {
			t.Fatalf("an older reader sees harness id %q on a contained record", old.HarnessSessionID)
		}
		if id != "" && !strings.Contains(string(b), id) {
			t.Fatal("the harness id was not persisted in the containment record")
		}
	}
}

func TestHarnessIDAccessor(t *testing.T) {
	plain := chat.Session{HarnessSessionID: "legacy"}
	if plain.HarnessID() != "legacy" {
		t.Fatal("uncontained sessions must keep reading the legacy field")
	}
	if got := containedRecord("rec").HarnessID(); got != "rec" {
		t.Fatalf("contained HarnessID = %q", got)
	}
}

// TestReopenRefusesContainmentOnUncontained: containment is chosen when a
// conversation is created; asking for it on reopen of an uncontained one is
// refused before any launch, and the record is untouched.
func TestReopenRefusesContainmentOnUncontained(t *testing.T) {
	store := memstore.New()
	rec := chat.Session{ID: "plain", Harness: "claude-code", WorkingDir: t.TempDir(), HarnessSessionID: "h"}
	if err := store.CreateSession(context.Background(), &rec); err != nil {
		t.Fatal(err)
	}
	_, err := chat.Reopen(context.Background(), chat.ReopenOptions{
		SessionID:   "plain",
		Store:       store,
		BinaryPath:  "/nonexistent/harness",
		Containment: &wrapper.Containment{Kind: wrapper.ContainmentLandlock},
	})
	if !errors.Is(err, chat.ErrInvalidOptions) || !strings.Contains(err.Error(), "uncontained") {
		t.Fatalf("err = %v", err)
	}
	after, _ := store.GetSession(context.Background(), "plain")
	if after.Containment != nil || after.HarnessSessionID != "h" {
		t.Fatalf("record changed: %+v", after)
	}
}

// TestReopenRejectsInvalidContainedRecords: an unknown record version and a
// contained record carrying a legacy harness id are refused before launch.
func TestReopenRejectsInvalidContainedRecords(t *testing.T) {
	store := memstore.New()
	bad := containedRecord("h")
	bad.ID = "future"
	bad.Containment.Version = chat.ContainmentRecordVersion + 1
	legacy := containedRecord("h")
	legacy.ID = "legacy"
	legacy.HarnessSessionID = "h"
	for _, r := range []chat.Session{bad, legacy} {
		r := r
		if err := store.CreateSession(context.Background(), &r); err != nil {
			t.Fatal(err)
		}
		_, err := chat.Reopen(context.Background(), chat.ReopenOptions{SessionID: r.ID, Store: store, BinaryPath: "/nonexistent/harness"})
		if !errors.Is(err, chat.ErrInvalidOptions) {
			t.Fatalf("%s: err = %v", r.ID, err)
		}
	}
}

// plainStore implements chat.Store without the ContainmentStore extension.
type plainStore struct{ inner *memstore.Store }

func (s plainStore) CreateSession(ctx context.Context, x *chat.Session) error {
	return s.inner.CreateSession(ctx, x)
}

func (s plainStore) GetSession(ctx context.Context, id string) (*chat.Session, error) {
	return s.inner.GetSession(ctx, id)
}

func (s plainStore) UpdateSession(ctx context.Context, x *chat.Session) error {
	return s.inner.UpdateSession(ctx, x)
}

func (s plainStore) AppendTurn(ctx context.Context, x *chat.Turn) error {
	return s.inner.AppendTurn(ctx, x)
}

func (s plainStore) UpdateTurn(ctx context.Context, x *chat.Turn) error {
	return s.inner.UpdateTurn(ctx, x)
}

func (s plainStore) ListTurns(ctx context.Context, id string) ([]chat.Turn, error) {
	return s.inner.ListTurns(ctx, id)
}

// TestOpenRefusesContainmentOnStoreWithoutExtension: a store that has not
// declared it persists containment records cannot hold a contained
// conversation.
func TestOpenRefusesContainmentOnStoreWithoutExtension(t *testing.T) {
	_, err := chat.Open(context.Background(), chat.Options{
		Harness:     "claude-code",
		BinaryPath:  "/nonexistent/harness",
		Store:       plainStore{inner: memstore.New()},
		Containment: &wrapper.Containment{Kind: wrapper.ContainmentLandlock},
	})
	if !errors.Is(err, chat.ErrContainmentUnavailable) {
		t.Fatalf("err = %v, want ErrContainmentUnavailable", err)
	}
}

// TestMemstoreKeepsContainmentRecordsPrivate: the store deep-copies the record
// in and out, so a caller mutating its copy cannot change the stored one.
func TestMemstoreKeepsContainmentRecordsPrivate(t *testing.T) {
	store := memstore.New()
	rec := containedRecord("h")
	if err := store.CreateSession(context.Background(), &rec); err != nil {
		t.Fatal(err)
	}
	rec.Containment.HarnessSessionID = "mutated"
	got, _ := store.GetSession(context.Background(), rec.ID)
	if got.Containment.HarnessSessionID != "h" {
		t.Fatal("store aliases the caller's record")
	}
	got.Containment.Required.ReadOnly = append(got.Containment.Required.ReadOnly, "/x")
	again, _ := store.GetSession(context.Background(), rec.ID)
	if len(again.Containment.Required.ReadOnly) != 0 {
		t.Fatal("store hands out an aliased record")
	}
	var _ chat.ContainmentStore = store
}

func TestDeleteContainmentStateRefusesUncontained(t *testing.T) {
	store := memstore.New()
	rec := chat.Session{ID: "plain", Harness: "codex"}
	_ = store.CreateSession(context.Background(), &rec)
	if err := chat.DeleteContainmentState(context.Background(), store, "plain"); !errors.Is(err, chat.ErrInvalidOptions) {
		t.Fatalf("err = %v", err)
	}
}
