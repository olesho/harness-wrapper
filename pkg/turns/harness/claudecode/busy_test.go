package claudecode

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// Busy is the guard the chat layer's idle-completion fallback consults so it
// never completes a turn while Claude is still working. It keys off the
// "esc to interrupt" footer Claude shows only mid-turn.
func TestBusy_synthetic(t *testing.T) {
	a := New()
	if !a.Busy(screen.Snapshot{Text: "⏵⏵ bypass permissions on · esc to interrupt\n✢ Schlepping… (3s · ↓2 tokens)"}) {
		t.Fatal("expected Busy=true while the 'esc to interrupt' footer is shown")
	}
	if a.Busy(screen.Snapshot{Text: "✻ Baked for 3s\n❯ \n⏵⏵ auto mode on · ← for agents"}) {
		t.Fatal("expected Busy=false at an idle prompt (no 'esc to interrupt')")
	}
}

// While Claude waits on sub-agents (Task/Explore), the "esc to interrupt" footer
// can flicker out for a redraw frame even though the work continues. The
// in-progress spinner line stays, so Busy must still report true off it alone —
// otherwise an intermediate "✻ <verb> for Ns" summary on that frame cuts the turn
// off mid-sub-agent (the plan-reviewer-stalls-on-ORCHE-37 bug).
func TestBusy_subAgentSpinnerWithoutFooter(t *testing.T) {
	a := New()
	// A real sub-agent frame, WITHOUT "esc to interrupt" present.
	frame := "✶ Cerebrating… (57s · ↓ 4.8k tokens)\n" +
		"  ◯ Explore  Verify queue, screen events, fleet-db types   24s · ↓ 35.8k tokens\n" +
		"❯ \n⏵⏵ bypass permissions on (shift+tab to cycle) · ↓ to manage"
	if !a.Busy(screen.Snapshot{Text: frame}) {
		t.Fatal("expected Busy=true off the in-progress spinner while sub-agents run (footer flickered out)")
	}
	// The compact form seen in the unit corpus must also read busy.
	if !a.Busy(screen.Snapshot{Text: "✢ Schlepping… (3s · ↓2 tokens)\n❯ "}) {
		t.Fatal("expected Busy=true off the spinner line alone")
	}
}

// The settled past-tense summary must NOT look like the in-progress spinner —
// guards against workingRE over-matching and hanging a finished turn.
func TestBusy_settledSummaryIsNotWorking(t *testing.T) {
	a := New()
	for _, idle := range []string{
		"✻ Baked for 3s\n❯ \n⏵⏵ auto mode on · ← for agents",
		"✻ Cooked for 1m 22s\n❯ \n⏵⏵ auto mode on · ← for agents",
		"⏺ Done. See lib.ts.\n✻ Pondered for 9s\n❯ ",
	} {
		if a.Busy(screen.Snapshot{Text: idle}) {
			t.Fatalf("expected Busy=false on a settled frame: %q", idle)
		}
	}
}

// Corpus guard: every recording ends at a settled/idle frame, so Busy must be
// false there. If a Claude Code release renames the footer, this fails — the
// early-warning the corpus exists to provide.
func TestBusy_corpusFinalFramesAreIdle(t *testing.T) {
	a := New()
	for _, name := range []string{"tool-call", "multi-turn", "interrupted-mid-reply"} {
		if a.Busy(lastLiveFrame(t, name)) {
			t.Errorf("[%s] final frame should be idle (Busy=false), but 'esc to interrupt' was present", name)
		}
	}
}
