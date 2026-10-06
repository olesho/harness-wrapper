package inputtrack

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

func kinds(evs []turns.Event) []turns.Kind {
	out := make([]turns.Kind, len(evs))
	for i, e := range evs {
		out[i] = e.Kind
	}
	return out
}

func TestObserve(t *testing.T) {
	var tr Tracker
	a := &turns.InputRequest{ID: "a", Kind: "trust_prompt", Prompt: "A?"}
	b := &turns.InputRequest{ID: "b", Kind: "bypass_acceptance", Prompt: "B?"}

	if evs := tr.Observe(a, "x: "); len(evs) != 1 || evs[0].Kind != turns.InputRequested {
		t.Fatalf("first dialog: %v", kinds(evs))
	}
	if evs := tr.Observe(a, "x: "); len(evs) != 0 {
		t.Fatalf("redraw of the same dialog emitted %v", kinds(evs))
	}
	// A replaces B with no dialog-free frame between: resolve A, then request B.
	evs := tr.Observe(b, "x: ")
	if len(evs) != 2 || evs[0].Kind != turns.InputResolved || evs[1].Kind != turns.InputRequested {
		t.Fatalf("replacement: %v, want [resolved requested]", kinds(evs))
	}
	if evs[0].Input.ID != "a" || evs[0].Input.Kind != "trust_prompt" {
		t.Errorf("resolved %+v, want the replaced dialog a", evs[0].Input)
	}
	evs = tr.Observe(nil, "x: ")
	if len(evs) != 1 || evs[0].Kind != turns.InputResolved || evs[0].Input.ID != "b" {
		t.Fatalf("clear: %v", kinds(evs))
	}
	if tr.LastID() != "" {
		t.Errorf("LastID = %q after clear", tr.LastID())
	}
	if evs := tr.Observe(nil, "x: "); len(evs) != 0 {
		t.Fatalf("idle frame emitted %v", kinds(evs))
	}
}
