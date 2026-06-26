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
		// pi submits on a plain carriage return (no kitty protocol); a "\n" leaves
		// the prompt unsent. Regression guard for the live-verified 0.76.0 fix.
		{"pi composer", "pi", "0.0%/131k (auto)  gpt-oss-120b • medium", "\r"},
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

// TestReadyForInputPi pins the chat-layer wiring of pi's send-readiness gate:
// pi requires readiness (so Send waits past its noisy startup) and delegates the
// decision to pi.PromptReady — ready only at the idle status line, not while busy
// or still starting up.
func TestReadyForInputPi(t *testing.T) {
	if !requiresPromptReadiness("pi") {
		t.Fatal("pi must require prompt readiness so Send waits for its composer")
	}
	const (
		idle    = "────\n~/proj (main)\n↑1.2k ↓32 $0.000 0.9%/131k (auto)   gpt-oss-120b • medium\n"
		busy    = " ⠧ Working...\n0.0%/131k (auto)   gpt-oss-120b • medium\n"
		startup = " pi v0.76.0\n Press ctrl+o to show full startup help\n ripgrep not found. Downloading...\n"
	)
	for _, tc := range []struct {
		name string
		text string
		want bool
	}{
		{"idle composer ready", idle, true},
		{"busy not ready", busy, false},
		{"startup not ready", startup, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readyForInput("pi", tc.text); got != tc.want {
				t.Errorf("readyForInput(pi, %q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
