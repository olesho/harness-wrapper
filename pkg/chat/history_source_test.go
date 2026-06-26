package chat

import (
	"context"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// transcriptAdapter is a minimal turns.Adapter that also implements
// turns.TranscriptReader, returning a fixed set of turns.
type transcriptAdapter struct {
	turns.Adapter
	turns []transcript.Turn
}

func (a *transcriptAdapter) ReadTranscript(_, _ string) ([]transcript.Turn, error) {
	return a.turns, nil
}

// TestHistoryWithSource_StoreVsTranscript locks in that HistoryWithSource
// reports the true provenance: the store fallback when no transcript-backed id
// is available, and the transcript when the reader + session id are present.
// This is what lets callers (the run command's debug label) tell a lossy
// screen scrape from the authoritative transcript — len(History) cannot.
func TestHistoryWithSource_StoreFallback(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	sess := Session{ID: "s1"}
	_ = store.CreateSession(ctx, &sess)
	_ = store.AppendTurn(ctx, &Turn{ID: "t1", SessionID: "s1", Role: RoleAssistant, Text: "screen tail"})

	// Adapter implements TranscriptReader, but no harness session id was
	// captured → store fallback.
	c := &Conversation{
		store:   store,
		session: sess,
		adapter: &transcriptAdapter{turns: []transcript.Turn{{Role: "assistant", Text: "transcript"}}},
	}

	out, src, err := c.HistoryWithSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if src != HistorySourceStore {
		t.Fatalf("source = %q, want %q (no harness session id)", src, HistorySourceStore)
	}
	if len(out) != 1 || out[0].Text != "screen tail" {
		t.Fatalf("turns = %+v, want the store turn", out)
	}
}

func TestHistoryWithSource_Transcript(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	sess := Session{ID: "s1", HarnessSessionID: "harness-uuid"}
	_ = store.CreateSession(ctx, &sess)
	_ = store.AppendTurn(ctx, &Turn{ID: "t1", SessionID: "s1", Role: RoleAssistant, Text: "screen tail"})

	c := &Conversation{
		store:   store,
		session: sess,
		adapter: &transcriptAdapter{turns: []transcript.Turn{{Role: "assistant", Text: "full transcript reply"}}},
	}

	out, src, err := c.HistoryWithSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if src != HistorySourceTranscript {
		t.Fatalf("source = %q, want %q", src, HistorySourceTranscript)
	}
	if len(out) != 1 || out[0].Text != "full transcript reply" {
		t.Fatalf("turns = %+v, want the transcript turn", out)
	}
}
