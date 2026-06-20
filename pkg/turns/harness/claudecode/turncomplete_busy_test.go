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
