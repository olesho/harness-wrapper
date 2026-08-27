package claudecode

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

func hasTurnComplete(evs []turns.Event) bool {
	for _, e := range evs {
		if e.Kind == turns.TurnComplete {
			return true
		}
	}
	return false
}

// An intermediate "✻ <verb> for Ns" summary printed while Claude is still
// working (footer shows "esc to interrupt") must NOT complete the turn; the
// genuine end-of-turn marker (claude idle) on a later frame must.
func TestTurnComplete_gatedOnBusy(t *testing.T) {
	a := New()
	busy := screen.Snapshot{Text: "✻ Pondered for 3s\n⏵⏵ bypass permissions on · esc to interrupt"}
	if hasTurnComplete(a.OnScreen(busy)) {
		t.Fatal("intermediate marker while busy must NOT emit TurnComplete")
	}
	idle := screen.Snapshot{Text: "✻ Baked for 12s\n❯ \n⏵⏵ auto mode on · ← for agents"}
	if !hasTurnComplete(a.OnScreen(idle)) {
		t.Fatal("end-of-turn marker while idle must emit TurnComplete")
	}
}

// Sub-agent variant of the gate: an intermediate "✻ <verb> for Ns" summary
// landing on a frame where the "esc to interrupt" footer has flickered out but
// the in-progress spinner is still up (Claude waiting on Task/Explore agents)
// must NOT complete the turn. The genuine end-of-turn marker, once the spinner
// is gone and the prompt is idle, still completes it. Regression for the
// plan-reviewer-stalls-on-a-sub-agent-plan bug.
func TestTurnComplete_gatedOnSubAgentSpinner(t *testing.T) {
	a := New()
	working := screen.Snapshot{Text: "⏺ I'll verify the facts first.\n✻ Pondered for 12s\n" +
		"✶ Cerebrating… (57s · ↓ 4.8k tokens)\n  ◯ Explore  Verify types   24s · ↓ 35.8k tokens\n❯ "}
	if hasTurnComplete(a.OnScreen(working)) {
		t.Fatal("intermediate marker while sub-agents run (spinner up, no footer) must NOT complete")
	}
	done := screen.Snapshot{Text: "⏺ Here is the revised plan…\n✻ Synthesized for 2m 3s\n❯ \n⏵⏵ auto mode on · ← for agents"}
	if !hasTurnComplete(a.OnScreen(done)) {
		t.Fatal("genuine end-of-turn marker (spinner gone, idle) must complete")
	}
}

// countTurnComplete reports how many TurnComplete events a batch carries, so a
// case can distinguish "did not fire" from "fired twice".
func countTurnComplete(evs []turns.Event) int {
	n := 0
	for _, e := range evs {
		if e.Kind == turns.TurnComplete {
			n++
		}
	}
	return n
}

// idleFooter is the settled Claude Code footer: a composer prompt and the
// permission-mode line, with no "esc to interrupt" and no working spinner, so
// Busy() is false and OnScreen is free to act on a marker.
const idleFooter = "\n\u276f \n\u23f5\u23f5 auto mode on \u00b7 \u2190 for agents"

// Claude Code 2.1.247 renders the settled end-of-turn summary with a trailing
// status clause ("✻ Churned for 2m 27s \u00b7 done 2:26 AM") and sometimes more
// than one ("\u00b7 done 10:47 AM \u00b7 2 shells still running"). The end-anchored
// thinkingRE rejected every one of them, so TurnComplete never fired and RunTurn
// hung until an external watchdog killed the run. Each shape must produce
// EXACTLY ONE TurnComplete on a settled frame.
func TestTurnComplete_trailingStatusClause(t *testing.T) {
	for _, marker := range []string{
		"✻ Churned for 2m 27s \u00b7 done 2:26 AM",
		"✻ Baked for 0s \u00b7 done 10:35 AM",
		"✻ Saut\u00e9ed for 11m 51s \u00b7 done 10:47 AM \u00b7 2 shells still running",
		"✻ Cooked for 1m 22s", // the bare pre-2.1.24x shape still works
	} {
		t.Run(marker, func(t *testing.T) {
			a := New()
			snap := screen.Snapshot{Text: "\u23fa done\n" + marker + idleFooter}
			if n := countTurnComplete(a.OnScreen(snap)); n != 1 {
				t.Fatalf("TurnComplete count = %d, want exactly 1 for %q", n, marker)
			}
		})
	}
}

// The widened trailing-clause tail must not swallow the in-progress indicator,
// the same shape quoted inside prose, or any settled-looking marker on a frame
// where Busy() is true.
func TestTurnComplete_trailingStatusClauseNegatives(t *testing.T) {
	for name, text := range map[string]string{
		// Contains " \u00b7 " but has no " for <dur>" outside the parenthetical, so
		// the marker prefix never matches.
		"in-progress indicator": "✻ Cooking\u2026 (1m 22s \u00b7 esc to interrupt)" + idleFooter,
		// Line anchors: the marker shape echoed inside explanatory prose.
		"marker quoted in prose": "\u23fa you'd see '✻ Baked for 5s \u00b7 done 1:00 PM' here" + idleFooter,
		// Busy(): the footer hint is up, so the frame is mid-turn.
		"busy via footer": "✻ Churned for 2m 27s \u00b7 done 2:26 AM\n\u23f5\u23f5 auto mode on \u00b7 esc to interrupt",
		// Busy(): the working spinner is up even though the footer flickered out.
		"busy via spinner": "✻ Baked for 0s \u00b7 done 10:35 AM\n\u2736 Cerebrating\u2026 (57s \u00b7 \u2193 4.8k tokens)\n\u276f ",
	} {
		t.Run(name, func(t *testing.T) {
			a := New()
			if n := countTurnComplete(a.OnScreen(screen.Snapshot{Text: text})); n != 0 {
				t.Fatalf("TurnComplete count = %d, want 0 for %s", n, name)
			}
		})
	}
}
