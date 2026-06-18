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
