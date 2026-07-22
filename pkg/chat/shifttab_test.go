package chat

import (
	"bytes"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// TestShiftTabForHarness pins the per-harness Shift+Tab key — the keystroke
// claude-code and codex bind to "cycle permission mode". Both run the kitty /
// enhanced keyboard protocol (the same fact that forces CSI 13u for Enter, see
// TestSubmitKeyForHarness), where Shift+Tab is CSI 9 ; 2 u. That encoding was
// verified live against claude-code 2.1.217 and codex 0.144.5 (both visibly
// cycled their mode indicator); see shiftTabForHarness's doc comment for the
// measurements, including the finding that the legacy CSI Z still works too.
// A regression here is silent in the worst way: the write succeeds, the harness
// ignores it, and a cycle loop spins through its entire bound without ever
// changing mode.
func TestShiftTabForHarness(t *testing.T) {
	const csi92u = "\x1b[9;2u"
	tests := []struct {
		name    string
		harness string
		screen  string
		want    []byte
	}{
		{"claude alias", "claude", "Claude Code ❯ ", []byte(csi92u)},
		{"claude-code", chatClaudeCode, "... accept edits on ...", []byte(csi92u)},
		{"claude-code any screen", chatClaudeCode, "whatever is on screen", []byte(csi92u)},
		{"codex", "codex", "›Find and fix a bug in @filename", []byte(csi92u)},
		{"codex any screen", "codex", "whatever is on screen", []byte(csi92u)},
		// pi enables no enhanced keyboard mode and exposes no permission-mode
		// cycle (verified live on 0.76.0: Shift+Tab lands on a thinking toggle
		// instead), so it gets the same nil as any unknown harness — callers
		// must fail loudly rather than fire a keystroke that means something
		// else entirely.
		{"pi unsupported", "pi", "0.0%/131k (auto)", nil},
		{"unknown unsupported", "someharness", "anything", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shiftTabForHarness(tt.harness, tt.screen)
			if !bytes.Equal(got, tt.want) {
				t.Errorf("shiftTabForHarness(%q, …) = %q, want %q", tt.harness, got, tt.want)
			}
			if tt.want == nil && got != nil {
				t.Errorf("shiftTabForHarness(%q, …) = %q, want nil (not an empty slice)", tt.harness, got)
			}
		})
	}
}

// TestShiftTabRejectsLegacyForm guards the single riskiest assumption in the
// permission-mode work. Live measurement showed CSI Z *does* currently cycle
// both harnesses, so this is not a "the legacy form is broken" test — it is a
// deliberate pin on emitting the protocol-native encoding the TUIs' enhanced
// keyboard mode defines, because the legacy path is the compatibility shim of
// the two and the one that can disappear. Spelled out separately from the table
// above so the intent survives a careless table edit.
func TestShiftTabRejectsLegacyForm(t *testing.T) {
	const legacy = "\x1b[Z"
	for _, harness := range []string{"claude", chatClaudeCode, "codex"} {
		if got := shiftTabForHarness(harness, ""); bytes.Equal(got, []byte(legacy)) {
			t.Errorf("shiftTabForHarness(%q, …) returned the legacy CSI Z form; "+
				"enhanced-keyboard harnesses need CSI 9;2u", harness)
		}
	}
}

// TestShiftTabMatchesFakeharness pins the production writer and the hermetic
// fake to the same bytes. internal/fakeharness.Builder.AwaitShiftTab matches on
// ShiftTabCSI9_2u, so if this drifts the scenarios in the driver subtask would
// keep passing against bytes the wrapper no longer sends. Mirrors the way
// AwaitSubmit ties fakeharness.SubmitCSI13u to submitKeyForHarness.
func TestShiftTabMatchesFakeharness(t *testing.T) {
	for _, harness := range []string{"claude", chatClaudeCode, "codex"} {
		got := shiftTabForHarness(harness, "")
		if !bytes.Equal(got, []byte(fakeharness.ShiftTabCSI9_2u)) {
			t.Errorf("shiftTabForHarness(%q, …) = %q, but fakeharness.ShiftTabCSI9_2u = %q; "+
				"the fake and the production writer must stay byte-equal",
				harness, got, fakeharness.ShiftTabCSI9_2u)
		}
	}
}

// TestSubmitKeyMatchesFakeharness is the same pin for the pre-existing submit
// key, made explicit here alongside its Shift+Tab twin — until now the tie was
// only implicit in the PTY scenarios that use AwaitSubmit.
func TestSubmitKeyMatchesFakeharness(t *testing.T) {
	for _, harness := range []string{chatClaudeCode, "codex"} {
		if got := submitKeyForHarness(harness, ""); !bytes.Equal(got, []byte(fakeharness.SubmitCSI13u)) {
			t.Errorf("submitKeyForHarness(%q, …) = %q, want fakeharness.SubmitCSI13u %q",
				harness, got, fakeharness.SubmitCSI13u)
		}
	}
	if got := submitKeyForHarness("pi", ""); !bytes.Equal(got, []byte(fakeharness.SubmitCR)) {
		t.Errorf("submitKeyForHarness(\"pi\", …) = %q, want fakeharness.SubmitCR %q",
			got, fakeharness.SubmitCR)
	}
}
