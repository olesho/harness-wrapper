package chat

import (
	"bytes"
	"os"
	"path/filepath"
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
		{"unknown", "someharness", "anything", "\n"},
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

// TestAuthRequired pins the per-harness logged-out / re-auth banner detection.
// Anchors are grounded in real observed CLI output (see ready.go).
func TestAuthRequired(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness string
		text    string
		want    bool
	}{
		// claude-code — real: `claude -p` on a logged-out box prints this verbatim.
		{"claude not-logged-in", chatClaudeCode, "Not logged in · Please run /login", true},
		{"claude login-expiry banner", chatClaudeCode, "  ⚠ Your login expires in 1 day · run /login to renew\n❯ ", true},
		// codex — real: `codex exec` turn failure / `codex login status` / remediation.
		{"codex 401", "codex", "ERROR: unexpected status 401 Unauthorized: Missing bearer or basic authentication in header", true},
		{"codex not-logged-in", "codex", "Not logged in", true},
		{"codex login remediation", "codex", "ChatGPT account ID not available, please re-run `codex login`", true},
		// No false positives on ordinary screen text.
		{"claude ordinary reply", chatClaudeCode, "⏺ I refactored the auth module.", false},
		{"codex ordinary reply", "codex", "› ready\nthinking about the task", false},
		// Anchors do not cross harnesses.
		{"claude does not fire on codex 401", chatClaudeCode, "HTTP 401 Unauthorized from the API", false},
		{"codex does not fire on claude /login", "codex", "please run /login", false},
		// Unknown harness never fires.
		{"unknown harness", "some-other-harness", "Not logged in", false},
		// claude-code 2.1.263 OAuth browser sign-in (PUPPET-315): the wall the
		// login-method menu advances into. "Select login method" is GONE from this
		// screen, so before the fix nothing matched it and Send hung to the
		// deadline. Both prose lines are anchors; either alone suffices.
		{"claude oauth sign-in url line", chatClaudeCode, " Browser didn't open? Use the url below to sign in (c to copy)", true},
		{"claude oauth paste-code line", chatClaudeCode, " Paste code here if prompted >", true},
		// The anchors are deliberately the full UI phrasings: an assistant reply
		// merely saying "paste code" must not be gated.
		{"claude reply mentioning paste code", chatClaudeCode, "⏺ Copy the snippet and paste code into main.go.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := authRequired(tc.harness, tc.text); got != tc.want {
				t.Errorf("authRequired(%q, %q) = %v, want %v", tc.harness, tc.text, got, tc.want)
			}
		})
	}
}

// TestOnboardingWallClaude2_1_263 pins the claude 2.1.263 pre-reply screens as
// measured live on 2026-09-06 (PUPPET-315), straight off the corpus fixtures. The
// OAuth browser sign-in screen is the one that used to match nothing: it is a
// WALL (onboardingWall true, so the auth gate fires IMMEDIATELY rather than after
// authGateStabilizeGap — the CLI can replace this frame within one paint), and it
// is never ready for input. The logged-out composer is the deliberate contrast:
// authRequired true but readyForInput ALSO true, because a real composer carrying
// a stale banner is usable.
func TestOnboardingWallClaude2_1_263(t *testing.T) {
	for _, tc := range []struct {
		fixture                       string
		wantWall, wantReady, wantAuth bool
	}{
		{"oauth-browser-signin", true, false, true},
		{"login-method-2.1.263", true, false, true},
		{"not-logged-in-2.1.263", false, true, true},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(authCorpusRoot, "claude-code", tc.fixture, "screen.txt"))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			screen := string(b)
			if got := onboardingWall(chatClaudeCode, screen); got != tc.wantWall {
				t.Errorf("onboardingWall = %v, want %v", got, tc.wantWall)
			}
			if got := readyForInput(chatClaudeCode, screen); got != tc.wantReady {
				t.Errorf("readyForInput = %v, want %v", got, tc.wantReady)
			}
			if got := authRequired(chatClaudeCode, screen); got != tc.wantAuth {
				t.Errorf("authRequired = %v, want %v", got, tc.wantAuth)
			}
		})
	}
}

// TestReadyForInputClaudeTrustDialog2_1_263 is the chat-layer cross-check for the
// PUPPET-452 class of regression: 2.1.261 dropped the folder-trust dialog's
// numbered options, which broke the turns-layer menu matcher. The chat layer
// matches by literal substring, not menu shape, so the dialog is still not-ready
// on 2.1.263 — and it is not an auth screen. Frame body captured live 2026-09-06.
func TestReadyForInputClaudeTrustDialog2_1_263(t *testing.T) {
	const trustDialog = " Accessing workspace:\n\n /tmp/probe/wd4\n\n" +
		" Quick safety check: Is this a project you created or one you trust? (Like your own code, a well-known open source\n" +
		" project, or work from your team). If not, take a moment to review what's in this folder first.\n\n" +
		" Claude Code'll be able to read, edit, and execute files here.\n\n Security guide\n\n" +
		" \u276f No, exit\n   Yes, I trust this folder\n\n Enter to confirm \u00b7 Esc to cancel\n"
	if readyForInput(chatClaudeCode, trustDialog) {
		t.Error("readyForInput(trust dialog) = true, want false")
	}
	if onboardingWall(chatClaudeCode, trustDialog) {
		t.Error("onboardingWall(trust dialog) = true, want false: it is a blocking dialog, not an auth wall")
	}
	if authRequired(chatClaudeCode, trustDialog) {
		t.Error("authRequired(trust dialog) = true, want false")
	}
}
