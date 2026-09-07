package claudecode

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// trustScreen mimics Claude Code's folder-trust dialog as the vt100 emulator
// renders it: a bordered box (right-edge "│" after column padding) with a
// "❯" selector on the default option.
const trustScreen = `╭─────────────────────────────────────────────────╮
│ Do you trust the files in this folder?            │
│                                                   │
│ /Users/oleh/Work/aether/harness-wrapper           │
│                                                   │
│ ❯ 1. Yes, proceed                                 │
│   2. No, exit                                     │
│                                                   │
│ Enter to confirm · Esc to exit                    │
╰─────────────────────────────────────────────────╯`

const bypassScreen = `WARNING: Claude Code running in Bypass Permissions mode

By proceeding, you accept all risks.

❯ 1. Yes, I accept
  2. No, exit
`

func TestDetectInput_TrustDialog(t *testing.T) {
	req, ok := DetectInput(trustScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the trust dialog")
	}
	if req.Kind != "trust_prompt" {
		t.Errorf("Kind = %q, want trust_prompt", req.Kind)
	}
	if req.Prompt != trustAnchor {
		t.Errorf("Prompt = %q, want %q", req.Prompt, trustAnchor)
	}
	want := []turns.InputOption{
		{ID: "1", Alias: "proceed", Label: "Yes, proceed", Keys: []byte("1\r")},
		{ID: "2", Alias: "deny", Label: "No, exit", Keys: []byte("2\r")},
	}
	if len(req.Options) != len(want) {
		t.Fatalf("len(Options) = %d, want %d (%+v)", len(req.Options), len(want), req.Options)
	}
	for i, w := range want {
		o := req.Options[i]
		if o.ID != w.ID || o.Alias != w.Alias || o.Label != w.Label || string(o.Keys) != string(w.Keys) {
			t.Errorf("Options[%d] = {id:%q alias:%q label:%q keys:%q}, want {id:%q alias:%q label:%q keys:%q}",
				i, o.ID, o.Alias, o.Label, o.Keys, w.ID, w.Alias, w.Label, w.Keys)
		}
	}
	if req.ID == "" {
		t.Error("request ID is empty")
	}
}

func TestDetectInput_BypassAcceptance(t *testing.T) {
	req, ok := DetectInput(bypassScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the bypass-permissions screen")
	}
	if len(req.Options) != 2 {
		t.Fatalf("len(Options) = %d, want 2 (%+v)", len(req.Options), req.Options)
	}
	if req.Options[0].Alias != "proceed" { // "Yes, I accept"
		t.Errorf("Options[0].Alias = %q, want proceed", req.Options[0].Alias)
	}
	if req.Options[1].Alias != "deny" {
		t.Errorf("Options[1].Alias = %q, want deny", req.Options[1].Alias)
	}
}

func TestDetectInput_StableIDAcrossRedraw(t *testing.T) {
	a, _ := DetectInput(trustScreen)
	b, _ := DetectInput(trustScreen + "\n") // a redraw with extra trailing padding
	if a.ID != b.ID {
		t.Errorf("ID not stable across redraws: %q vs %q", a.ID, b.ID)
	}
}

func TestDetectInput_NoDialog(t *testing.T) {
	if _, ok := DetectInput("a normal Claude Code session\n❯ \n"); ok {
		t.Error("DetectInput false-positive on a normal screen")
	}
	// Anchor present but no menu rendered yet → not actionable.
	if _, ok := DetectInput(trustAnchor + "\n"); ok {
		t.Error("DetectInput fired on an anchor with no options")
	}
}

func TestOnScreen_InputRequestedThenResolved(t *testing.T) {
	a := New()

	req := findKind(a.OnScreen(screen.Snapshot{Text: trustScreen}), turns.InputRequested)
	if req == nil {
		t.Fatal("no InputRequested emitted for the trust dialog")
	}
	if req.Input == nil || len(req.Input.Options) != 2 {
		t.Fatalf("InputRequested missing structured input: %+v", req)
	}

	// Same dialog re-renders → no duplicate request.
	if dup := findKind(a.OnScreen(screen.Snapshot{Text: trustScreen}), turns.InputRequested); dup != nil {
		t.Error("duplicate InputRequested emitted on redraw")
	}

	// Dialog clears → InputResolved carrying the same ID.
	res := findKind(a.OnScreen(screen.Snapshot{Text: "Claude Code\n❯ \n"}), turns.InputResolved)
	if res == nil {
		t.Fatal("no InputResolved emitted when the dialog cleared")
	}
	if res.Input == nil || res.Input.ID != req.Input.ID {
		t.Errorf("InputResolved ID = %+v, want %q", res.Input, req.Input.ID)
	}
}

func findKind(evs []turns.Event, k turns.Kind) *turns.Event {
	for i := range evs {
		if evs[i].Kind == k {
			return &evs[i]
		}
	}
	return nil
}

// TestDetectInput_SelectorTrustDialog is the selector-shape twin of
// TestDetectInput_TrustDialog above. The numbered assertions there are the
// regression guard proving this branch did not steal the numbered path, so they
// stay exactly as they are; this one pins the shape claude 2.1.251 actually
// renders — unnumbered, highlight on "No, exit" — through the SAME public entry
// point a caller uses.
func TestDetectInput_SelectorTrustDialog(t *testing.T) {
	req, ok := DetectInput(selectorTrustFrame)
	if !ok {
		t.Fatal("DetectInput did not recognize the unnumbered 2.1.251 folder-trust dialog")
	}
	if req.Kind != "trust_prompt" {
		t.Errorf("Kind = %q, want trust_prompt", req.Kind)
	}
	if req.Prompt != trustAnchorAlt {
		t.Errorf("Prompt = %q, want %q", req.Prompt, trustAnchorAlt)
	}
	if req.MultiSelect {
		t.Error("MultiSelect = true; selector keys are select-and-submit, i.e. single-select")
	}
	want := []turns.InputOption{
		{ID: "0", Alias: "deny", Label: "No, exit", Keys: []byte("\r"), Highlighted: true},
		{ID: "1", Alias: "proceed", Label: "Yes, I trust this folder", Keys: []byte("\x1b[B\r")},
	}
	if len(req.Options) != len(want) {
		t.Fatalf("len(Options) = %d, want %d (%+v)", len(req.Options), len(want), req.Options)
	}
	for i, w := range want {
		o := req.Options[i]
		if o.ID != w.ID || o.Alias != w.Alias || o.Label != w.Label ||
			string(o.Keys) != string(w.Keys) || o.Highlighted != w.Highlighted {
			t.Errorf("Options[%d] = {id:%q alias:%q label:%q keys:%q highlighted:%v}, "+
				"want {id:%q alias:%q label:%q keys:%q highlighted:%v}",
				i, o.ID, o.Alias, o.Label, o.Keys, o.Highlighted,
				w.ID, w.Alias, w.Label, w.Keys, w.Highlighted)
		}
	}
	if req.ID == "" {
		t.Error("request ID is empty")
	}
}

// TestDetectInput_NumberedMenuWinsWhenBothShapesPresent pins the precedence: a
// numbered menu also carries a "❯" highlight, and its digit keys are absolute
// (immune to a stale highlight), so it must keep winning byte-for-byte.
func TestDetectInput_NumberedMenuWinsWhenBothShapesPresent(t *testing.T) {
	req, ok := DetectInput(trustScreen)
	if !ok {
		t.Fatal("DetectInput did not recognize the numbered trust dialog")
	}
	for _, o := range req.Options {
		if string(o.Keys) != o.ID+"\r" {
			t.Errorf("option %q keys = %q, want the absolute digit form %q — the selector "+
				"branch stole the numbered path", o.Label, o.Keys, o.ID+"\r")
		}
	}
}
