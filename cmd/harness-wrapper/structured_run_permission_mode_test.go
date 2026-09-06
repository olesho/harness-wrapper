package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
)

// Acceptance for turnproto.StructuredTurnResult.permission_mode: the structured
// result reports the CANONICAL RUNG THE TURN WAS LAUNCHED AT, resolved from the
// final launch inputs by wrapper.EffectiveLaunchRung.
//
// These assert the EMITTED PAYLOAD, not the helper — the helper's own table
// lives in pkg/wrapper/permission_rungs_test.go. What is under test here is the
// WIRING: that structured_run.go feeds the resolver the post-applySandboxDefaults
// argv and the parsed knob, and that the status guard holds.
//
// The value's promise, restated so a reader of these rows does not over-read
// them: presence of "bypass" is trustworthy for a turn that reached the harness;
// ABSENCE NEVER MEANS SAFE; and a RESTRICTIVE rung here is a launch argument,
// not a gate (claude's per-tool dialog is undetected on this path and codex
// approvals are auto-answered by oneshot.AutoAcceptAnswer). See the field's doc
// comment.

// writeClaudeReplyScript writes the standard "one completed claude turn"
// fakeharness script and returns its path.
func writeClaudeReplyScript(t *testing.T, dir string) string {
	t.Helper()
	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
		StayAliveUntilStopped().
		Build()
	return writeFakeScript(t, dir, script)
}

// writeCodexReplyScript is the codex twin: readiness is the "›" composer and the
// turn completes on a fresh Token-usage footer.
func writeCodexReplyScript(t *testing.T, dir string) string {
	t.Helper()
	script := fakeharness.New("codex").
		Idle().
		AwaitSubmit().
		CodexWorking(30, "Thinking").
		CodexReply(40, "codex reply: "+fakeharness.PromptRef()).
		StayAliveUntilStopped().
		Build()
	return writeFakeScript(t, dir, script)
}

func writeFakeScript(t *testing.T, dir string, script fakeharness.Script) string {
	t.Helper()
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	path := filepath.Join(dir, "script.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// TestStructuredRun_PermissionModeClaude drives real claude-shaped turns through
// structured-run and asserts the reported rung for each way a permission axis
// can reach the launch: the knob, a native spelling, --sandbox-defaults' injected
// blanket flag, the composed pair, a raw flag after --, and nothing at all.
func TestStructuredRun_PermissionModeClaude(t *testing.T) {
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
		// why documents WHICH resolution arm the row pins, so a row that starts
		// passing for the wrong reason is visible in review.
		why string
	}{
		{
			name: "explicit canonical rung",
			args: []string{"--permission-mode", "plan", "claude", "--"},
			want: "plan",
			why:  "the knob, carried through to claudeRung's canonical passthrough",
		},
		{
			name: "native spelling reports the canonical rung",
			args: []string{"--permission-mode", "acceptEdits", "claude", "--"},
			want: "ask",
			why:  "acceptEdits -> ask; the wire NEVER carries a native spelling",
		},
		{
			name: "native dontAsk reports the manual rung",
			args: []string{"--permission-mode", "dontAsk", "claude", "--"},
			want: "manual",
			why:  "dontAsk -> manual; claude's own rank table ties dontAsk with its default (this ladder's manual), and the wire NEVER carries a native spelling",
		},
		{
			name: "sandbox-defaults alone reports bypass",
			args: []string{"--sandbox-defaults", "claude", "--"},
			want: "bypass",
			why:  "no --permission-mode at all; resolved from the INJECTED --dangerously-skip-permissions",
		},
		{
			name: "composed sandbox-defaults plus bypass rung",
			args: []string{"--sandbox-defaults", "--permission-mode", "bypass", "claude", "--"},
			want: "bypass",
			why:  "applySandboxDefaults skips the arg half for a bypass-class mode, so this resolves via claudeRung(\"bypass\") — the two halves do not double-count",
		},
		{
			name: "raw skip-permissions flag after --",
			args: []string{"claude", "--", "--dangerously-skip-permissions"},
			want: "bypass",
			why:  "the literal example in this subcommand's doc comment; a naive echo-the-knob design would emit nothing here",
		},
		{
			name: "no mode and no permission flag omits the field",
			args: []string{"claude", "--"},
			want: "",
			why:  "harness default: no canonical rung can be named. ABSENT, not an empty-string default",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HARNESS_BINARY_CLAUDE", fakeBin)
			t.Setenv(fakeharness.EnvVar, writeClaudeReplyScript(t, dir))
			t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "30s")
			t.Chdir(t.TempDir())

			out, code := captureStructuredRun(t, "ship the turn API", tc.args)
			res, ok := turnproto.ParseLastJSONLine([]byte(out))
			if !ok {
				t.Fatalf("no JSON result line in stdout:\n%s", out)
			}
			if res.Status != turnproto.StatusCompleted {
				t.Fatalf("status = %q, want %q (exit %d); stdout:\n%s",
					res.Status, turnproto.StatusCompleted, code, out)
			}
			if res.PermissionMode != tc.want {
				t.Errorf("permission_mode = %q, want %q (%s)", res.PermissionMode, tc.want, tc.why)
			}
			// Absence must be ABSENCE on the wire, not `"permission_mode":""`.
			assertPermissionModeKeyPresence(t, out, tc.want != "")
		})
	}
}

// TestStructuredRun_PermissionModeCodex is the codex half: the two-axis inverse
// is where a rung resolver can go wrong, and --sandbox-defaults / acceptEdits are
// claude-only.
func TestStructuredRun_PermissionModeCodex(t *testing.T) {
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
		why  string
	}{
		{
			name: "manual inverts the read-only sandbox pair",
			args: []string{"--permission-mode", "manual", "codex", "--"},
			want: "manual",
			why:  "the (read-only, untrusted) pair inverts through codexSandboxRung",
		},
		{
			name: "codex-native danger-full-access reports bypass",
			args: []string{"--permission-mode", "danger-full-access", "codex", "--"},
			want: "bypass",
			why:  "this knob emits `-s danger-full-access` with NO -a at all; a bare-`-s`-means-unknown reading would report nothing for the most dangerous codex launch",
		},
		{
			name: "raw sandbox flag after --",
			args: []string{"codex", "--", "-s", "danger-full-access"},
			want: "bypass",
			why:  "caller argv suppresses injection; the resolver reads the argv the harness actually got",
		},
		{
			name: "no mode and no permission flag omits the field",
			args: []string{"codex", "--"},
			want: "",
			why:  "harness default",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HARNESS_BINARY_CODEX", fakeBin)
			t.Setenv(fakeharness.EnvVar, writeCodexReplyScript(t, dir))
			t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "30s")
			t.Chdir(t.TempDir())

			out, code := captureStructuredRun(t, "ship the turn API", tc.args)
			res, ok := turnproto.ParseLastJSONLine([]byte(out))
			if !ok {
				t.Fatalf("no JSON result line in stdout:\n%s", out)
			}
			if res.Status != turnproto.StatusCompleted {
				t.Fatalf("status = %q, want %q (exit %d); stdout:\n%s",
					res.Status, turnproto.StatusCompleted, code, out)
			}
			if res.PermissionMode != tc.want {
				t.Errorf("permission_mode = %q, want %q (%s)", res.PermissionMode, tc.want, tc.why)
			}
			assertPermissionModeKeyPresence(t, out, tc.want != "")
		})
	}
}

// TestStructuredRun_PermissionModeAbsentOnStartupError pins the §4 rule for BOTH
// mechanisms that produce a startup_error: no harness was launched, so there is
// no rung to report.
//
// The CLASSIFIED row is the one that matters, and it must use a
// REJECTED-BUT-RESOLVABLE config. `--permission-mode auto claude --
// --dangerously-skip-permissions` is rejected by validatePermissionMode's
// contradiction arm, yet EffectiveLaunchRung resolves it to "bypass" via claude
// rule 1 — so without structured_run.go's status guard the wire would report a
// startup_error claiming it launched at bypass, for a harness that was never
// spawned. TestStructuredRun_InvalidPermissionModeIsStartupError's "not-a-rung"
// config resolves to "" and therefore pins nothing here.
func TestStructuredRun_PermissionModeAbsentOnStartupError(t *testing.T) {
	// An unexecutable placeholder on purpose: validation must reject the config
	// before anything is spawned.
	placeholder := filepath.Join(t.TempDir(), "never-executed-claude")
	if err := os.WriteFile(placeholder, []byte("#!/bin/sh\nexit 99\n"), 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}

	for _, tc := range []struct {
		name   string
		prompt string
		args   []string
		// resolvesTo is what EffectiveLaunchRung WOULD say for this config —
		// documented so the row's teeth are visible.
		resolvesTo string
	}{
		{
			// emitStartupError path: fails before parsed/harnessArgs even exist.
			name:       "pre-parse failure omits the field",
			prompt:     "   ",
			args:       []string{"--permission-mode", "bypass", "claude", "--"},
			resolvesTo: "bypass",
		},
		{
			// Classified path: travels the ordinary emitStructured literal, so
			// only the explicit status guard keeps the field off.
			name:       "classified rejected-but-resolvable config omits the field",
			prompt:     "a prompt",
			args:       []string{"--permission-mode", "auto", "claude", "--", "--dangerously-skip-permissions"},
			resolvesTo: "bypass",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HARNESS_BINARY_CLAUDE", placeholder)
			t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "30s")
			t.Chdir(t.TempDir())

			out, code := captureStructuredRun(t, tc.prompt, tc.args)
			res, ok := turnproto.ParseLastJSONLine([]byte(out))
			if !ok {
				t.Fatalf("no JSON result line in stdout:\n%s", out)
			}
			if res.Status != turnproto.StatusStartupError {
				t.Fatalf("status = %q, want %q; stdout:\n%s", res.Status, turnproto.StatusStartupError, out)
			}
			if code != turnproto.ExitError {
				t.Errorf("exit code = %d, want %d", code, turnproto.ExitError)
			}
			if res.PermissionMode != "" {
				t.Errorf("permission_mode = %q, want empty: no harness was launched (this config resolves to %q, which is exactly what the status guard must suppress)",
					res.PermissionMode, tc.resolvesTo)
			}
			assertPermissionModeKeyPresence(t, out, false)
		})
	}
}

// TestStructuredRun_PermissionModePresentOnDeadline pins the deliberate other
// half of the §3 decision: ONLY startup_error is excluded. A turn that timed out
// at "bypass" DID reach the harness, and that is precisely the record an
// orchestrator needs.
func TestStructuredRun_PermissionModePresentOnDeadline(t *testing.T) {
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}

	// A script that goes ready and then never replies: the run timeout fires.
	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		StayAliveUntilStopped().
		Build()

	dir := t.TempDir()
	t.Setenv("HARNESS_BINARY_CLAUDE", fakeBin)
	t.Setenv(fakeharness.EnvVar, writeFakeScript(t, dir, script))
	t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "3s")
	t.Chdir(t.TempDir())

	out, code := captureStructuredRun(t, "ship the turn API",
		[]string{"--permission-mode", "bypass", "claude", "--"})

	res, ok := turnproto.ParseLastJSONLine([]byte(out))
	if !ok {
		t.Fatalf("no JSON result line in stdout:\n%s", out)
	}
	if res.Status != turnproto.StatusDeadline {
		t.Skipf("status = %q, want %q — the timing-dependent deadline path did not trigger; stdout:\n%s",
			res.Status, turnproto.StatusDeadline, out)
	}
	if code != turnproto.ExitDeadline {
		t.Errorf("exit code = %d, want %d", code, turnproto.ExitDeadline)
	}
	if res.PermissionMode != "bypass" {
		t.Errorf("permission_mode = %q, want %q: a deadline turn REACHED the harness, so it carries the rung",
			res.PermissionMode, "bypass")
	}
}

// assertPermissionModeKeyPresence checks the raw emitted line, not just the
// parsed struct: "absent" must mean the key is missing (omitempty), never
// `"permission_mode":""`, which a consumer could misread as a value.
func assertPermissionModeKeyPresence(t *testing.T, out string, want bool) {
	t.Helper()
	if got := strings.Contains(out, `"permission_mode"`); got != want {
		if want {
			t.Errorf("permission_mode key absent from the emitted line, want present; stdout:\n%s", out)
		} else {
			t.Errorf("permission_mode key present in the emitted line, want ABSENT (omitempty); stdout:\n%s", out)
		}
	}
}
