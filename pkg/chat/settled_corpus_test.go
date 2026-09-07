package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/screen"
)

// settledCorpusFrame returns the settled post-turn screen from the live Claude
// Code 2.1.247 recording at test/corpus/claude-code/settled-after-turn. The
// frame carries 2.1.247's end-of-turn summary WITH its trailing status clause
// ("✻ Crunched for 2s \u00b7 done 5:06 AM") on a reply long enough that the
// "Claude Code" startup banner has scrolled out of the viewport — the exact
// screen on which end-of-turn detection used to fail twice over: thinkingRE
// rejected the clause, and readyForInput demanded the banner, so the turn had
// no way to complete and RunTurn hung until a watchdog killed the run.
//
// The recording is replayed INCREMENTALLY, keeping the last frame that carried
// the clause, because 2.1.247 runs its TUI on the alternate screen: the
// recorder's SIGTERM makes claude emit the alt-screen exit, which blanks the
// emulator. Production reads frames as they land and never sees that teardown.
func settledCorpusFrame(t *testing.T) string {
	t.Helper()
	path := filepath.Join(corpusRoot(t), "claude-code", "settled-after-turn", "bytes.raw")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("settled-after-turn corpus unavailable: %v", err)
	}
	scr := screen.New(120, 40)
	var settled string
	const chunk = 64
	for i := 0; i < len(raw); i += chunk {
		end := i + chunk
		if end > len(raw) {
			end = len(raw)
		}
		_, _ = scr.Write(raw[i:end])
		if text := scr.Snapshot().Text; strings.Contains(text, "\u00b7 done ") {
			settled = text
		}
	}
	if settled == "" {
		t.Fatal("settled-after-turn recording never produced a frame carrying the \"\u00b7 done\" clause")
	}
	return settled
}

// corpusRoot walks up from the package directory to the repo's test/corpus.
func corpusRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		p := filepath.Join(wd, "test", "corpus")
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
		wd = filepath.Dir(wd)
	}
	t.Skip("test/corpus not found")
	return ""
}

// repaintable strips the emulator's right-edge column padding so the text can be
// painted back into a 120-column PTY without every line wrapping onto the next.
func repaintable(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	return strings.Join(lines, "\n") + "\n"
}

// TestReadyForInput_SettledCorpusFrame is B's premise, checked against the live
// recording rather than an assumption: the settled 2.1.247 frame has NO "Claude
// Code" banner (it scrolled off) but does paint the composer prompt, so
// readyForInput must call it ready. Requiring the banner is what left
// maybeIdleComplete's fallback inert on exactly this screen.
func TestReadyForInput_SettledCorpusFrame(t *testing.T) {
	settled := settledCorpusFrame(t)
	if strings.Contains(settled, "Claude Code") {
		t.Fatalf("precondition failed: the recorded settled frame still carries the banner, " +
			"so it cannot demonstrate the scroll-away case")
	}
	if !readyForInput(chatClaudeCode, settled) {
		t.Error("readyForInput(claude-code, settled 2.1.247 frame) = false; " +
			"a settled composer must be ready even with the banner scrolled off")
	}
}

// Dropping the banner requirement must NOT make the blocking first-run and
// dialog screens look ready — they each paint a "\u276f" selector of their own, and
// typing a prompt into one would send it to a menu.
func TestReadyForInput_ClaudeNotReadyScreens(t *testing.T) {
	for name, text := range map[string]string{
		"folder-trust dialog": "Do you trust the files in this folder?\n" +
			"\u276f 1. Yes, I trust this folder\n  2. No, exit\n",
		"folder-trust dialog (alt phrasing)": "Is this a project you created or one you trust?\n" +
			"\u276f 1. Yes, I trust this folder\n  2. No, exit\n",
		// The UNNUMBERED shape claude 2.1.251 actually renders (captured live,
		// 2026-08-29). The numbered entries above pass only because their digits
		// parse; this one FAILED before the selector-menu parser landed —
		// readyForInput fell through to its bare "❯"-contains and called the
		// dialog ready, so Send typed the prompt into it and submitted onto the
		// highlighted "No, exit", quitting claude at startup. The cheapest
		// possible regression pin for that.
		"folder-trust dialog (unnumbered, 2.1.251)": "Accessing workspace:\n" +
			"/private/tmp/trustrepo\n" +
			"Quick safety check: Is this a project you created or one you trust? \u2026\n" +
			"Claude Code'll be able to read, edit, and execute files here.\n" +
			"Security guide\n" +
			" \u276f No, exit\n" +
			"   Yes, I trust this folder\n" +
			"Enter to confirm \u00b7 Esc to cancel\n",
		"bypass acceptance": "WARNING: Bypass Permissions mode\n" +
			"\u276f 1. No, exit\n  2. Yes, I accept\n",
		"theme picker": loadCorpusScreen(t, "claude-code/theme-picker"),
		"select login method": "Select login method:\n" +
			"\u276f 1. Claude account with subscription\n  2. Anthropic Console account\n",
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(text, "\u276f") {
				t.Fatalf("precondition: %s must paint a \u276f selector, or it proves nothing", name)
			}
			if readyForInput(chatClaudeCode, text) {
				t.Errorf("readyForInput(claude-code, %s) = true; a blocking screen must stay not-ready", name)
			}
		})
	}
}

// TestIntegration_SettledCorpusFrameCompletesViaMarker drives the RECORDED
// 2.1.247 settled frame through a real PTY and the whole conversation loop, and
// pins that the turn completes VIA THE END-OF-TURN ✻ PATH — not via the
// idle fallback, and not by hanging until the test deadline.
//
// This is the artifact that makes the regression un-reintroducible. The
// pre-existing adversarial corpus holds only the bare pre-2.1.24x marker, which
// is exactly why ~30 releases of drift went unnoticed.
func TestIntegration_SettledCorpusFrameCompletesViaMarker(t *testing.T) {
	settled := settledCorpusFrame(t)

	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Crunching").
		Build()
	// Repaint the recorded settled screen verbatim as the final frame. Going
	// through the Script struct rather than a Builder helper keeps the recording
	// — not a hand-written approximation of it — as what the test asserts on.
	script.Steps = append(script.Steps, fakeharness.Step{
		Frame: &fakeharness.Frame{DelayMs: 40, Screen: repaintable(settled)},
	})

	conv := openFake(t, script)
	sendOneTurn(t, conv, "List the integers from 1 to 60, one per line, nothing else.")

	turn := waitForTerminalTurn(t, conv, 15*time.Second)
	if turn.State != TurnStateComplete {
		t.Fatalf("state = %q (reason %q), want %q", turn.State, turn.Reason, TurnStateComplete)
	}
	if !strings.Contains(turn.Reason, "end-of-turn marker confirmed") {
		t.Errorf("reason = %q, want the marker path (\"end-of-turn marker confirmed\"), not the idle fallback", turn.Reason)
	}
	// The recorded reply body must survive: completing on the marker captures
	// this frame's text, so the tail of the list has to be in it.
	if !strings.Contains(turn.Text, "59") || !strings.Contains(turn.Text, "60") {
		t.Errorf("captured text lost the recorded reply body\n--- captured ---\n%s", turn.Text)
	}
}
