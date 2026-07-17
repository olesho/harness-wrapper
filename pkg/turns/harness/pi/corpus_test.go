package pi

import (
	"os"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// TestCorpusReplay_TUI replays a REAL pi 0.76.0 interactive-turn capture
// (test/corpus/pi/tui-turn-raw.bin, recorded live over a PTY) through the screen
// emulator and asserts the adapter's drift-sensitive markers still hold on
// genuine rendered frames: the busy spinner is detected mid-turn (BusyDetector)
// and the idle status line is recognized once the turn settles (PromptReady). If
// a pi upgrade renames "Working..."/"Thinking..." or reformats its status line,
// this fails — the early-warning the version pin relies on.
func TestCorpusReplay_TUI(t *testing.T) {
	const path = "../../../../test/corpus/pi/tui-turn-raw.bin"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("corpus not present: %v", err)
	}
	a := New()
	// A wide screen so the harness's absolute cursor positioning never wraps a
	// status-line token; width only affects wrapping, not content.
	scr := screen.New(200, 50)

	// Feed cumulatively so a snapshot is taken during the busy phase (the spinner
	// frames) and at the settled end — the emulator state is incremental.
	everBusy := false
	const chunks = 40
	step := len(raw)/chunks + 1
	for off := 0; off < len(raw); off += step {
		end := off + step
		if end > len(raw) {
			end = len(raw)
		}
		_, _ = scr.Write(raw[off:end])
		if a.Busy(scr.Snapshot()) {
			everBusy = true
		}
	}

	final := scr.Snapshot()
	if !everBusy {
		t.Error("BusyDetector never fired across the real turn — the Working.../Thinking... spinner marker may have drifted")
	}
	if a.Busy(final) {
		t.Errorf("final frame still reads as busy — the turn should have settled\n--- final ---\n%s", final.Text)
	}
	if !PromptReady(final.Text) {
		t.Errorf("final frame not recognized as ready — pi's status-line format may have drifted\n--- final ---\n%s", final.Text)
	}
	if !strings.Contains(final.Text, "PINEAPPLE") {
		t.Errorf("reply text missing from final frame\n--- final ---\n%s", final.Text)
	}
}
