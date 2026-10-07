package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// The subagent-tool recording (2.1.283): claude backgrounds an Agent call,
// answers the tool call, then holds the turn on "✻ Waiting for 1 background
// agent to finish" for the agent's 30 s — no spinner ellipsis on the status
// line, no "esc to interrupt" in the footer, the agent's row painted below the
// composer box. The turn is still in flight throughout, so Busy must say so on
// every such frame; only the repaints of the agent's elapsed-time counter kept
// the idle-completion fallback from ending it early before. Exactly one
// TurnComplete fires, after the wait.
func TestSubagentCorpus_BusyWhileWaitingOnAgents(t *testing.T) {
	raw := corpusBytes(t, "subagent-tool")
	scr := screen.New(120, 40)
	a := New()
	waiting, completes := 0, 0
	waitedAfterComplete := false
	for i := 0; i < len(raw); i += trustReplayChunk {
		_, _ = scr.Write(raw[i:min(i+trustReplayChunk, len(raw))])
		snap := scr.Snapshot()
		status, _, ok := statusRegion(snap.Text)
		isWaiting := ok && strings.Contains(status, "Waiting for")
		if isWaiting {
			waiting++
			if !a.Busy(snap) {
				t.Fatalf("frame @%d: Busy=false while the status line reads %q:\n%s", i, strings.TrimSpace(status), snap.Text)
			}
			if completes > 0 {
				waitedAfterComplete = true
			}
		}
		for _, ev := range a.OnScreen(snap) {
			if ev.Kind == turns.TurnComplete {
				completes++
			}
		}
	}
	if waiting == 0 {
		t.Fatal("no frame showed the background-agent wait: the recording no longer covers it")
	}
	if completes != 1 || waitedAfterComplete {
		t.Fatalf("TurnComplete fired %d time(s) (waitedAfterComplete=%v), want exactly once, after the wait", completes, waitedAfterComplete)
	}
}

// The 2.1.283 layout with a composer box: a running subagent's rows sit in the
// footer below the box, so the status line above it is still the spinner — or,
// once claude has answered the Agent call, the background-agent wait line.
func TestBusy_subagentLayoutWithComposer(t *testing.T) {
	rule := strings.Repeat("─", 120)
	footer := []string{
		"  ⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents · ↓ to manage",
		"  ⏺ main",
		"  ◯ general-purpose  mock                                   12s · ↓ 21 tokens",
	}
	frame := func(status string) screen.Snapshot {
		lines := append([]string{"❯ AGENT TOOL sleep 30", "⏺ Agent(mock)", "  ⎿  Backgrounded agent", status, rule, "❯ ", rule}, footer...)
		return screen.Snapshot{Text: strings.Join(lines, "\n")}
	}
	a := New()
	for _, status := range []string{
		"✻ Waiting for 1 background agent to finish",
		"✳ Waiting for 2 background agents to finish",
		"✶ Boogieing… (3s · ↓ 13 tokens)",
	} {
		if !a.Busy(frame(status)) {
			t.Errorf("Busy=false with status %q and a subagent running", status)
		}
	}
	if a.Busy(frame("✻ Worked for 35s · done 12:30 PM")) {
		t.Error("Busy=true on the settled end-of-turn summary")
	}
	// A reply that merely mentions the phrase is not the status line.
	if a.Busy(frame("⏺ Claude says \"Waiting for 1 background agent to finish\" while it waits.")) {
		t.Error("Busy=true on reply text quoting the wait line")
	}
}
