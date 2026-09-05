package harness_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
)

// TestRunTurn_RealClaudeUntrustedDirSurfacesTrustDialog is the live regression
// for the folder-trust dialog Claude Code 2.1.261 re-rendered.
//
// It exists because the other real-Claude tests CANNOT catch this class. They
// run against a config dir whose trust is pre-seeded (see the caller's setup,
// e.g. scripts/claude-release-check.sh) precisely so the run can reach a turn —
// so a trust-dialog regression leaves them all green. That is how 2.1.261
// shipped a dialog this package could not parse without any test failing: the
// numbered-menu parser found zero options, DetectInput reported "not
// actionable", no InputRequested was emitted, and the harness died on the
// dialog's default "No, exit".
//
// The assertion is deliberately about DETECTION, not about answering: with a
// complete config that is missing only trust, the turn must fail as a surfaced,
// answerable input request (chat.ErrInputPending) rather than as an opaque
// errored turn. Measured across the fix:
//
//	v0.8.1: "harness: turn errored"                     — dialog invisible
//	v0.8.2: "chat: blocked on interactive input request" — dialog detected
func TestRunTurn_RealClaudeUntrustedDirSurfacesTrustDialog(t *testing.T) {
	if os.Getenv("HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN") != "1" {
		t.Skip("set HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN=1 to run against real Claude Code")
	}
	claudePath := requireRealClaude(t)

	// Auth is taken from whatever the caller already has working. A file copy of
	// .credentials.json is NOT reliable on its own: Claude Code migrates that
	// file into the OS keychain on first use and removes it, so a config dir
	// that authenticated a moment ago may no longer contain one. An ambient
	// CLAUDE_CODE_OAUTH_TOKEN (how loom spawns its agents) is sufficient by
	// itself, since the fresh config dir below inherits it from the environment.
	var creds []byte
	if src := os.Getenv("CLAUDE_CONFIG_DIR"); src != "" {
		creds, _ = os.ReadFile(filepath.Join(src, ".credentials.json"))
	}
	if len(creds) == 0 && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		t.Skip("no usable auth: set CLAUDE_CODE_OAUTH_TOKEN, or CLAUDE_CONFIG_DIR to a profile that still has .credentials.json")
	}

	// A config dir that is complete EXCEPT for trust: onboarding done and the
	// permission dialogs pre-accepted, so the ONLY thing that can block the turn
	// is the folder-trust prompt. Without these the test could pass for the
	// wrong reason (blocked on the bypass-permissions screen instead).
	cfg := t.TempDir()
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cfg, name), []byte(body), mode); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	if len(creds) > 0 {
		write(".credentials.json", string(creds), 0o600)
	}
	write(".claude.json", `{"hasCompletedOnboarding":true}`, 0o600)
	write("settings.json", `{"permissions":{"defaultMode":"auto"},
"skipDangerousModePermissionPrompt":true,"skipAutoPermissionPrompt":true,
"bypassPermissionsModeAccepted":true,"disableBundledSkills":true,
"disableClaudeAiConnectors":true,"disableRemoteControl":true,
"disableWorkflows":true,"disableArtifacts":true}`, 0o600)
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)

	// A directory that has never been trusted by this config.
	workDir := t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var out bytes.Buffer
	_, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    claudePath,
		Args:          []string{"--dangerously-skip-permissions"},
		WorkingDir:    workDir,
		Prompt:        "Reply with exactly: HARNESS_WRAPPER_RUNTURN_OK",
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err == nil {
		t.Fatalf("turn completed in an untrusted directory; the dialog did not appear, so this test proves nothing\noutput:\n%s", out.String())
	}
	if !errors.Is(err, chat.ErrInputPending) {
		t.Fatalf("folder-trust dialog was not detected as an answerable input request.\n"+
			"got: %v\nwant an error satisfying chat.ErrInputPending\noutput:\n%s", err, out.String())
	}
}
