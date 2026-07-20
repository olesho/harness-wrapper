package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// corpusBytes loads a recorded byte stream from the project's test/corpus.
// Returns the bytes or t.Skip-ed test if the recording isn't present.
func corpusBytes(t *testing.T, scenario string) []byte {
	t.Helper()
	// Walk up from this test's CWD (pkg/turns/harness/codex) to find repo root.
	wd, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		p := filepath.Join(wd, "test/corpus/codex", scenario, "bytes.raw")
		if data, err := os.ReadFile(p); err == nil {
			return data
		}
		wd = filepath.Dir(wd)
	}
	t.Skipf("test/corpus/codex/%s/bytes.raw not found; record it with screenbench-record", scenario)
	return nil
}

// TestCodexAdapter_NoFireOnRealRecording is the post-0.142 contract for the
// canonical corpus (re-baked at 0.142.2). Codex 0.142 removed the on-screen
// "Token usage:" footer, so OnScreen has nothing to fingerprint and must stay
// SILENT on a real recording — completion is the chat layer's idle path, not an
// adapter screen marker (see the package comment). This is the live regression:
// if a future Codex restores an on-screen end-of-turn marker, OR someone loosens
// tokenUsageRE so it false-fires on reply prose, this goes red and prompts a
// re-bake + adapter revisit. The "fires" path itself is covered synthetically by
// TestCodexAdapterRefiresWhenFingerprintChanges.
func TestCodexAdapter_NoFireOnRealRecording(t *testing.T) {
	for _, scenario := range []string{
		"short-reply", "long-markdown", "code-block", "tool-call", "multi-turn",
	} {
		t.Run(scenario, func(t *testing.T) {
			scr := screen.New(120, 40)
			_, _ = scr.Write(corpusBytes(t, scenario))
			for _, ev := range New().OnScreen(scr.Snapshot()) {
				if ev.Kind == turns.TurnComplete {
					t.Errorf("codex 0.142 recording %q unexpectedly fired TurnComplete "+
						"(on-screen footer drift — re-check tokenUsageRE and re-bake): %+v", scenario, ev)
				}
			}
		})
	}
}

func TestCodexAdapterNoFireOnEmptyScreen(t *testing.T) {
	scr := screen.New(80, 24)
	a := New()
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 0 {
		t.Errorf("expected no events on empty screen, got %d", len(evs))
	}
}

func TestCodexAdapterRefiresWhenFingerprintChanges(t *testing.T) {
	scr := screen.New(120, 40)
	a := New()

	_, _ = scr.Write([]byte("\x1b[H\x1b[2JToken usage: total=100 input=80 (+ 50 cached) output=20\r\n"))
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 1 {
		t.Fatalf("first turn: expected 1 event, got %d", len(evs))
	}

	// Same fingerprint → no fire.
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 0 {
		t.Fatalf("repeat: expected 0 events, got %d", len(evs))
	}

	// New fingerprint → fire again.
	_, _ = scr.Write([]byte("\r\nToken usage: total=200 input=150 (+ 100 cached) output=50\r\n"))
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 1 {
		t.Fatalf("second turn: expected 1 event, got %d", len(evs))
	}
}

// TestCodexAdapter_InterstitialTransitionResolvesPrev locks in the fix for a
// dropped resolve: when one interstitial gives way DIRECTLY to a different one
// (no intervening dialog-free frame), the adapter must emit InputResolved for
// the first BEFORE InputRequested for the second. Without it the first
// request's identity/kind is lost — a client subscribed from the start sees
// the replacement's kind on the eventual resolve (observed live as an update
// notice resolving as codex_notice).
func TestCodexAdapter_InterstitialTransitionResolvesPrev(t *testing.T) {
	scr := screen.New(120, 40)
	a := New()

	// Frame 1: the "Update available!" menu → InputRequested(codex_update_notice).
	updateScreen := "\x1b[H\x1b[2J" +
		"  Update available! 0.144.5 -> 0.144.6\r\n" +
		"› 1. Update now (runs `npm install -g @openai/codex`)\r\n" +
		"  2. Skip\r\n" +
		"  3. Skip until next version\r\n" +
		"  Press enter to continue\r\n"
	_, _ = scr.Write([]byte(updateScreen))
	evs := a.OnScreen(scr.Snapshot())
	if len(evs) != 1 || evs[0].Kind != turns.InputRequested || evs[0].Input == nil || evs[0].Input.Kind != KindUpdateNotice {
		t.Fatalf("frame 1: want 1 InputRequested(codex_update_notice), got %+v", evs)
	}

	// Frame 2: the model-migration screen replaces it directly (no clear frame
	// between). Expect InputResolved(update) THEN InputRequested(migration).
	migrationScreen := "\x1b[H\x1b[2J" +
		"  Choose how you'd like Codex to proceed\r\n" +
		"  Press enter to continue\r\n"
	_, _ = scr.Write([]byte(migrationScreen))
	evs = a.OnScreen(scr.Snapshot())
	if len(evs) != 2 {
		t.Fatalf("frame 2: want 2 events (resolve prev, request new), got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != turns.InputResolved || evs[0].Input == nil || evs[0].Input.Kind != KindUpdateNotice {
		t.Errorf("frame 2 event[0] = %+v, want InputResolved(codex_update_notice)", evs[0])
	}
	if evs[1].Kind != turns.InputRequested || evs[1].Input == nil || evs[1].Input.Kind != KindModelMigration {
		t.Errorf("frame 2 event[1] = %+v, want InputRequested(codex_model_migration)", evs[1])
	}
}

func TestCodexAdapterName(t *testing.T) {
	if n := New().Name(); n != "codex" {
		t.Errorf("expected Name()=codex, got %q", n)
	}
}

// TestCodexAdapter_AdversarialNoFire feeds the adapter recordings that
// should NOT fire TurnComplete: an assistant reply that mentions
// "Token usage:" without the full footer pattern, and a truncated
// stream with no footer at all. Locks in that the footer regex stays
// strict — loosening it (e.g. to plain `"Token usage:"`) would break
// these cases.
func TestCodexAdapter_AdversarialNoFire(t *testing.T) {
	for _, scenario := range []string{
		"adversarial/prefix-only-marker",
		"adversarial/partial-stream-no-footer",
	} {
		t.Run(scenario, func(t *testing.T) {
			bytes := corpusBytes(t, scenario)

			scr := screen.New(120, 40)
			_, _ = scr.Write(bytes)
			snap := scr.Snapshot()

			a := New()
			evs := a.OnScreen(snap)
			for _, ev := range evs {
				if ev.Kind == turns.TurnComplete {
					t.Errorf("adversarial scenario %q mis-fired TurnComplete: %+v", scenario, ev)
				}
			}
		})
	}
}
