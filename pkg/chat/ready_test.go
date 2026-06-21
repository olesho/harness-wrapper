package chat

import (
	"bytes"
	"testing"
)

// TestSubmitKeyForHarness pins the per-harness Enter key. codex 0.141.0 and
// claude-code's enhanced TUI both run the kitty keyboard protocol, where a plain
// CR/LF from a synthetic PTY writer does not submit the composer — CSI 13 u
// (unmodified Enter) does. A regression here silently wedges session-mode codex:
// the prompt is typed but never submitted, so the turn never runs.
func TestSubmitKeyForHarness(t *testing.T) {
	const csi13u = "\x1b[13u"
	tests := []struct {
		name    string
		harness string
		screen  string
		want    string
	}{
		// codex 0.141.0: enhanced keyboard mode is unconditional, so always CSI 13u.
		{"codex composer", "codex", "›Find and fix a bug in @filename", csi13u},
		{"codex any screen", "codex", "whatever is on screen", csi13u},
		// claude-code (≥2.1.x): enhanced keyboard mode is unconditional — the
		// auto-mode composer shows neither "bypass permissions" nor the Vim hint,
		// yet a plain CR/LF still won't submit. Always CSI 13u, like codex.
		{"claude bypass", "claude-code", "... bypass permissions ...", csi13u},
		{"claude vim hint", "claude-code", "ctrl+g to edit in Vim", csi13u},
		{"claude auto mode", "claude-code", "Claude Code ❯ ... auto mode on", csi13u},
		// Unknown harnesses keep the plain newline.
		{"unknown", "gemini", "anything", "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := submitKeyForHarness(tt.harness, tt.screen)
			if !bytes.Equal(got, []byte(tt.want)) {
				t.Errorf("submitKeyForHarness(%q, …) = %q, want %q", tt.harness, got, tt.want)
			}
		})
	}
}
