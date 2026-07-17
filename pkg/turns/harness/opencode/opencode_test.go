package opencode

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

func TestOpenCodeAdapterName(t *testing.T) {
	if n := New().Name(); n != "opencode" {
		t.Errorf("expected Name()=opencode, got %q", n)
	}
}

// Until a corpus-derived screen marker lands, OnScreen should produce
// no events — the generic.Adapter delegate does nothing on screen
// changes by design. This locks in that behavior so a future override
// is an intentional change, not a regression.
func TestOpenCodeAdapterNoScreenEventsByDefault(t *testing.T) {
	scr := screen.New(120, 40)
	_, _ = scr.Write([]byte("any old content\r\n"))
	if evs := New().OnScreen(scr.Snapshot()); len(evs) != 0 {
		t.Errorf("expected 0 OnScreen events from generic delegate, got %d: %+v", len(evs), evs)
	}
}

// Verify the generic delegate still fires turn-complete via the
// wrapper.StatusWaitingForInput path. This is the fallback the adapter
// relies on for chat-style flows until a per-harness marker exists.
func TestOpenCodeAdapterFiresOnWaitingForInput(t *testing.T) {
	evs := New().OnWrapperStatus(wrapper.StatusWaitingForInput, "prompt detected: (y/n)")
	if len(evs) != 1 || evs[0].Kind != turns.TurnComplete {
		t.Errorf("expected 1 TurnComplete event, got %+v", evs)
	}
}

// The adapter deliberately does not implement turns.TranscriptReader or
// turns.SessionIDExtractor yet (OpenCode's on-disk store is in flux:
// JSON files in early versions, SQLite later). Lock that in so adding
// either capability is a conscious, corpus-backed change rather than an
// accidental one against an unverified format.
func TestOpenCodeAdapterCapabilitiesNotYetImplemented(t *testing.T) {
	var a turns.Adapter = New()
	if _, ok := a.(turns.TranscriptReader); ok {
		t.Error("opencode adapter should not implement TranscriptReader until the on-disk format is pinned by a corpus")
	}
	if _, ok := a.(turns.SessionIDExtractor); ok {
		t.Error("opencode adapter should not implement SessionIDExtractor until a scrape-able session-ID marker is confirmed")
	}
}
