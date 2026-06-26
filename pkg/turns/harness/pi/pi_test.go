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

// pi's on-disk session format is documented and versioned, so the adapter
// implements turns.TranscriptReader (unlike opencode), and pi's "/quit" slash
// command lets it implement turns.Quitter. It does NOT implement
// turns.SessionIDExtractor on the interactive path (no confirmed scrape-able
// on-screen UUID — the headless pkg/harness/pi profile covers session-id
// instead). Lock the surface in so each capability is a conscious choice.
func TestPiAdapterCapabilities(t *testing.T) {
	var a turns.Adapter = New()
	if _, ok := a.(turns.TranscriptReader); !ok {
		t.Error("pi adapter should implement TranscriptReader (documented JSONL session format)")
	}
	if _, ok := a.(turns.Quitter); !ok {
		t.Error("pi adapter should implement Quitter (pi has a /quit slash command)")
	}
	if _, ok := a.(turns.BusyDetector); !ok {
		t.Error("pi adapter should implement BusyDetector (the Working.../Thinking... spinner)")
	}
	if _, ok := a.(turns.SessionIDExtractor); ok {
		t.Error("pi adapter should not implement SessionIDExtractor until a scrape-able session-ID marker is confirmed")
	}
}

// frames captured live from pi 0.76.0 (cerebras/gpt-oss-120b).
const (
	piBusyFrame  = " ⠧ Working...\n────────────\n~/proj (main)\n0.0%/131k (auto)        gpt-oss-120b • medium\n"
	piThinkFrame = " ⠇ Thinking...\n────────────\n0.0%/131k (auto)        gpt-oss-120b • medium\n"
	piIdleFrame  = "────────────\n~/proj (main)\n↑1.2k ↓32 $0.000 0.9%/131k (auto)        gpt-oss-120b • medium\n"
	// Early startup: composer not yet listening, no status-line context indicator.
	piStartupFrame = " pi v0.76.0\n Press ctrl+o to show full startup help and loaded resources.\n ripgrep not found. Downloading...\n"
	// The thinking-level menu label must NOT read as busy (no trailing ellipsis).
	piMenuFrame = " Thinking Level\n 1. off  2. low  3. medium\n"
)

func snap(t *testing.T, text string) screen.Snapshot {
	t.Helper()
	scr := screen.New(120, 40)
	scr.Write([]byte(text))
	return scr.Snapshot()
}

func TestPiBusy(t *testing.T) {
	a := New()
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"working spinner", piBusyFrame, true},
		{"thinking spinner", piThinkFrame, true},
		{"idle status line", piIdleFrame, false},
		{"thinking-level menu is not busy", piMenuFrame, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.Busy(snap(t, tc.text)); got != tc.want {
				t.Errorf("Busy = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPiPromptReady(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"idle composer is ready", piIdleFrame, true},
		{"busy is not ready", piBusyFrame, false},
		{"thinking is not ready", piThinkFrame, false},
		{"startup (no status line) is not ready", piStartupFrame, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PromptReady(tc.text); got != tc.want {
				t.Errorf("PromptReady = %v, want %v", got, tc.want)
			}
		})
	}
}

// QuitSequence must carry pi's "/quit" command and a submit key so
// Conversation.Quit can drive a graceful exit instead of a SIGTERM.
func TestPiAdapterQuitSequence(t *testing.T) {
	keys := New().QuitSequence()
	if got, want := string(keys), "/quit\r"; got != want {
		t.Fatalf("QuitSequence = %q, want %q", got, want)
	}
}
