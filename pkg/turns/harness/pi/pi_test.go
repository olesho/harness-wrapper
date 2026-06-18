package pi

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

func TestPiAdapterName(t *testing.T) {
	if n := New().Name(); n != "pi" {
		t.Errorf("expected Name()=pi, got %q", n)
	}
}

// Until a corpus-derived screen marker lands, OnScreen should produce
// no events — the generic.Adapter delegate does nothing on screen
// changes by design. This locks in that behavior so a future override
// is an intentional change, not a regression.
func TestPiAdapterNoScreenEventsByDefault(t *testing.T) {
	scr := screen.New(120, 40)
	scr.Write([]byte("any old content\r\n"))
	if evs := New().OnScreen(scr.Snapshot()); len(evs) != 0 {
		t.Errorf("expected 0 OnScreen events from generic delegate, got %d: %+v", len(evs), evs)
	}
}

// Verify the generic delegate still fires turn-complete via the
// wrapper.StatusWaitingForInput path. This is the fallback the adapter
// relies on for chat-style flows until a per-harness marker exists.
func TestPiAdapterFiresOnWaitingForInput(t *testing.T) {
	evs := New().OnWrapperStatus(wrapper.StatusWaitingForInput, "prompt detected: (y/n)")
	if len(evs) != 1 || evs[0].Kind != turns.TurnComplete {
		t.Errorf("expected 1 TurnComplete event, got %+v", evs)
	}
}

// pi's on-disk session format is documented and versioned, so the
// adapter implements turns.TranscriptReader (unlike opencode). It does
// NOT yet implement turns.SessionIDExtractor (no confirmed on-screen
// UUID marker). Lock both in so the capability surface is a conscious
// choice.
func TestPiAdapterCapabilities(t *testing.T) {
	var a turns.Adapter = New()
	if _, ok := a.(turns.TranscriptReader); !ok {
		t.Error("pi adapter should implement TranscriptReader (documented JSONL session format)")
	}
	if _, ok := a.(turns.SessionIDExtractor); ok {
		t.Error("pi adapter should not implement SessionIDExtractor until a scrape-able session-ID marker is confirmed")
	}
}
