package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// settledAfterTurn is the live Claude Code 2.1.247 recording of a settled
// post-turn screen: the reply is long enough that the startup banner has
// scrolled out of the viewport, and the end-of-turn summary carries 2.1.247's
// trailing status clause ("✻ Crunched for 2s · done 5:06 AM"). It is the
// artifact that makes this regression un-reintroducible — the pre-existing
// adversarial corpus holds only the bare pre-2.1.24x marker, which is exactly
// why ~30 releases of drift went unnoticed.
const settledAfterTurn = "settled-after-turn"

// replaySettled feeds the recording through the screen emulator the way the
// wrapper does in production — incrementally, snapshotting as bytes arrive —
// and returns the emitted events plus the last snapshot that carried the
// end-of-turn summary.
//
// Incremental replay is not a convenience: Claude Code 2.1.247 runs its TUI on
// the ALTERNATE screen and emits the alt-screen exit (CSI ?1049l) when the
// recorder stops it, which blanks the emulator. A single whole-file Write would
// therefore snapshot the teardown, not the turn. Production never sees the
// final state either — it reads every frame as it lands.
func replaySettled(t *testing.T, a *Adapter) ([]turns.Event, screen.Snapshot) {
	t.Helper()
	raw := corpusBytes(t, settledAfterTurn)

	scr := screen.New(120, 40)
	var evs []turns.Event
	var settled screen.Snapshot
	const chunk = 64
	for i := 0; i < len(raw); i += chunk {
		end := i + chunk
		if end > len(raw) {
			end = len(raw)
		}
		_, _ = scr.Write(raw[i:end])
		snap := scr.Snapshot()
		evs = append(evs, a.OnScreen(snap)...)
		if strings.Contains(snap.Text, "· done ") {
			settled = snap
		}
	}
	return evs, settled
}

// The recorded 2.1.247 settled frame must complete the turn exactly once, and
// the reported marker must be the BARE text — the "· done <clock>" clause stays
// out of capture group 1 because that capture is the de-duplication
// fingerprint and the visible reason string.
func TestClaudeCodeAdapter_SettledAfterTurnCorpus(t *testing.T) {
	a := New()
	evs, settled := replaySettled(t, a)

	var completes []turns.Event
	for _, ev := range evs {
		if ev.Kind == turns.TurnComplete {
			completes = append(completes, ev)
		}
	}
	if len(completes) != 1 {
		t.Fatalf("TurnComplete count = %d, want exactly 1; events: %+v", len(completes), evs)
	}
	if want := reasonPrefix + "✻ Crunched for 2s"; completes[0].Reason != want {
		t.Errorf("reason = %q, want %q (bare marker, no trailing clause)", completes[0].Reason, want)
	}
	if settled.Text == "" {
		t.Fatal("recording never produced a frame carrying the \"· done\" clause")
	}
	if a.Busy(settled) {
		t.Error("settled frame reported Busy; it carries no in-progress footer or spinner")
	}
}

// B's premise, held against the live recording rather than an assumption: on a
// settled 2.1.247 frame the "Claude Code" startup banner has scrolled out of
// the viewport while the composer prompt is still painted. That is why
// pkg/chat.readyForInput may not require the banner — see its claude-code
// branch, and TestReadyForInput_SettledCorpusFrame there.
func TestClaudeCodeAdapter_SettledFrameHasNoBanner(t *testing.T) {
	_, settled := replaySettled(t, New())
	if settled.Text == "" {
		t.Fatal("recording never produced a settled frame")
	}
	if strings.Contains(settled.Text, "Claude Code") {
		t.Error("settled frame still carries the \"Claude Code\" banner; " +
			"pkg/chat.readyForInput could have kept requiring it")
	}
	if !strings.Contains(settled.Text, "❯") {
		t.Error("settled frame is missing the composer prompt")
	}
}

// Fingerprint stability: two frames of the SAME settled turn differ only in the
// "· done <clock>" clause (the clock ticks between redraws). Both must produce
// the same capture group 1, so the adapter de-duplicates them into exactly one
// TurnComplete instead of re-firing on every repaint.
func TestClaudeCodeAdapter_TrailingClauseFingerprintStable(t *testing.T) {
	const footer = "\n❯ \n⏵⏵ auto mode on · ← for agents"
	first := screen.Snapshot{Text: "⏺ done\n✻ Churned for 2m 27s · done 2:26 AM" + footer}
	second := screen.Snapshot{Text: "⏺ done\n✻ Churned for 2m 27s · done 2:27 AM" + footer}

	a := New()
	evs := a.OnScreen(first)
	if len(evs) != 1 || evs[0].Kind != turns.TurnComplete {
		t.Fatalf("first frame: expected exactly 1 TurnComplete, got %+v", evs)
	}
	if want := reasonPrefix + "✻ Churned for 2m 27s"; evs[0].Reason != want {
		t.Fatalf("reason = %q, want %q", evs[0].Reason, want)
	}
	if again := a.OnScreen(second); len(again) != 0 {
		t.Errorf("second frame differing only in the \"· done <clock>\" clause re-fired: %+v", again)
	}
}
