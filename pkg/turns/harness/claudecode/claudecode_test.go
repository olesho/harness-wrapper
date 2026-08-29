package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

func corpusBytes(t *testing.T, scenario string) []byte {
	t.Helper()
	wd, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		p := filepath.Join(wd, "test/corpus/claude-code", scenario, "bytes.raw")
		if data, err := os.ReadFile(p); err == nil {
			return data
		}
		wd = filepath.Dir(wd)
	}
	t.Skipf("test/corpus/claude-code/%s/bytes.raw not found", scenario)
	return nil
}

// footerAnchor is the substring every live Claude Code TUI frame carries in its
// permission footer ("⏵⏵ auto mode on …", "⏸ manual mode on …"). It marks a
// frame as belonging to the harness's own screen rather than to what the
// terminal shows once the TUI is gone.
const footerAnchor = " mode on"

// lastLiveFrame replays a recording the way production reads a PTY —
// incrementally, snapshotting as bytes land — and returns the last frame that
// was still the harness's TUI.
//
// A whole-file Write is not equivalent, and stopped being equivalent at
// 2.1.24x: Claude Code now runs its TUI on the ALTERNATE screen and emits the
// alt-screen exit (CSI ?1049l) when the recorder stops it, after which it
// prints its "Resume this session with: claude --resume …" epilogue on the
// normal screen. The final snapshot of a freshly baked recording is therefore
// that epilogue, not the turn — a whole-file assertion reads a screen the
// adapter never sees in production. Recordings made before the alt-screen move
// have no teardown at all and their last live frame IS their last frame, which
// is why one helper serves both vintages of the corpus.
func lastLiveFrame(t *testing.T, scenario string) screen.Snapshot {
	t.Helper()
	raw := corpusBytes(t, scenario)

	scr := screen.New(120, 40)
	var live screen.Snapshot
	const chunk = 64
	for i := 0; i < len(raw); i += chunk {
		end := i + chunk
		if end > len(raw) {
			end = len(raw)
		}
		_, _ = scr.Write(raw[i:end])
		if snap := scr.Snapshot(); strings.Contains(snap.Text, footerAnchor) {
			live = snap
		}
	}
	if live.Text == "" {
		t.Fatalf("%s: no frame in the recording carried the harness footer (%q); "+
			"the recording is truncated or the footer was renamed", scenario, footerAnchor)
	}
	return live
}

func TestClaudeCodeAdapterFiresOnMultiTurn(t *testing.T) {
	snap := lastLiveFrame(t, "multi-turn")

	a := New()
	evs := a.OnScreen(snap)
	if len(evs) != 1 {
		t.Fatalf("expected 1 event from final snapshot, got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != turns.TurnComplete {
		t.Errorf("expected TurnComplete, got %s", evs[0].Kind)
	}
}

func TestClaudeCodeAdapterDetectsInterrupt(t *testing.T) {
	snap := lastLiveFrame(t, "interrupted-mid-reply")

	a := New()
	evs := a.OnScreen(snap)

	var sawErrored bool
	for _, ev := range evs {
		if ev.Kind == turns.Errored {
			sawErrored = true
		}
	}
	if !sawErrored {
		t.Errorf("expected Errored event for interrupted recording, got events: %+v", evs)
	}
}

func TestClaudeCodeAdapterRefiresAcrossTurns(t *testing.T) {
	scr := screen.New(120, 40)
	a := New()

	_, _ = scr.Write([]byte("⏺ first reply\r\n✻ Baked for 5s\r\n"))
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 1 {
		t.Fatalf("turn 1: expected 1 event, got %d", len(evs))
	}

	// Same fingerprint → no fire.
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 0 {
		t.Fatalf("repeat: expected 0 events, got %d", len(evs))
	}

	// New thinking summary → fire again.
	_, _ = scr.Write([]byte("⏺ second reply\r\n✻ Brewed for 8s\r\n"))
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 1 {
		t.Fatalf("turn 2: expected 1 event, got %d", len(evs))
	}

	// Claude Code can use accented verbs in the thinking summary.
	_, _ = scr.Write([]byte("⏺ third reply\r\n✻ Sautéed for 4s\r\n"))
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 1 {
		t.Fatalf("turn 3: expected 1 event for accented verb, got %d", len(evs))
	}
}

// TestClaudeCodeAdapterFiresOnMinuteDurations is the regression guard for the
// >=60s detection-miss: Claude Code renders a turn's duration as "1m 22s" (and
// "1h 2m 3s") once it crosses a minute, but the old `for \d+s` pattern matched
// only sub-minute "5s" summaries — so every turn of a minute or more never
// emitted TurnComplete and RunTurn hung until a caller-side guard stepped in.
// Reproduced live on Claude Code 2.1.178 ("✻ Cooked for 1m 22s").
func TestClaudeCodeAdapterFiresOnMinuteDurations(t *testing.T) {
	cases := []struct{ name, summary string }{
		{"seconds", "✻ Baked for 5s"},
		{"minutes", "✻ Cooked for 1m 22s"},
		{"minutes-only", "✻ Brewed for 2m"},
		{"hours", "✻ Pondered for 1h 2m 3s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scr := screen.New(120, 40)
			a := New()
			_, _ = scr.Write([]byte("⏺ reply\r\n" + tc.summary + "\r\n"))
			evs := a.OnScreen(scr.Snapshot())
			if len(evs) != 1 || evs[0].Kind != turns.TurnComplete {
				t.Fatalf("%q: expected exactly 1 TurnComplete, got %+v", tc.summary, evs)
			}
		})
	}
}

// TestClaudeCodeAdapter_TrailingContentNoFire locks in that a duration line
// carrying the in-progress trailing decoration ("· ↑ tokens · esc to
// interrupt") does NOT mis-fire TurnComplete, so completion fires only on a
// settled frame. This is what keeps the broadened minute/hour duration pattern
// from firing mid-turn.
//
// Since the 2.1.247 fix, thinkingRE deliberately admits an arbitrary "· <tail>"
// suffix (the settled summary now carries "· done <clock>"), so the rejection
// here is made by the Busy() gate in OnScreen — which keys off exactly the "esc
// to interrupt" footer on this line — rather than by the regex's end anchor.
func TestClaudeCodeAdapter_TrailingContentNoFire(t *testing.T) {
	scr := screen.New(120, 40)
	a := New()
	_, _ = scr.Write([]byte("⏺ working\r\n✻ Cooked for 1m 22s · ↑ 3.1k tokens · esc to interrupt\r\n"))
	for _, ev := range a.OnScreen(scr.Snapshot()) {
		if ev.Kind == turns.TurnComplete {
			t.Errorf("trailing-content duration line mis-fired TurnComplete: %+v", ev)
		}
	}
}

func TestClaudeCodeAdapterName(t *testing.T) {
	if n := New().Name(); n != "claude-code" {
		t.Errorf("expected Name()=claude-code, got %q", n)
	}
}

// TestClaudeCodeAdapter_AdversarialNoFire feeds the adapter a recording
// where the assistant echoes the "✻ <Verb> for Ns" marker shape inside
// explanatory prose without actually completing the turn. Before the
// thinkingRE was anchored to a line of its own, this scenario
// mis-fired TurnComplete; the test locks the anchored behavior in.
func TestClaudeCodeAdapter_AdversarialNoFire(t *testing.T) {
	bytes := corpusBytes(t, "adversarial/thinking-line-mid-reply")

	scr := screen.New(120, 40)
	_, _ = scr.Write(bytes)
	snap := scr.Snapshot()

	a := New()
	evs := a.OnScreen(snap)
	for _, ev := range evs {
		if ev.Kind == turns.TurnComplete {
			t.Errorf("adversarial thinking-line-mid-reply mis-fired TurnComplete: %+v", ev)
		}
	}
}
