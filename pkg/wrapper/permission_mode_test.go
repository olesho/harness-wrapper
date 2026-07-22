package wrapper

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestArgsWithHarnessPermissionMode(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		args    []string
		mode    string
		want    []string
	}{
		// --- canonical rungs x claude -------------------------------------
		{
			name:    "claude plan",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			mode:    "plan",
			want:    []string{"--permission-mode", "plan", "-p", "prompt"},
		},
		{
			name:    "claude manual",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			mode:    "manual",
			want:    []string{"--permission-mode", "manual", "-p", "prompt"},
		},
		{
			// The rung is named "ask"; claude's native spelling for it is
			// "acceptEdits". The asymmetry is deliberate and shared with the
			// TypeScript half — do not rename either side.
			name:    "claude ask maps to acceptEdits",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			mode:    "ask",
			want:    []string{"--permission-mode", "acceptEdits", "-p", "prompt"},
		},
		{
			name:    "claude auto",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			mode:    "auto",
			want:    []string{"--permission-mode", "auto", "-p", "prompt"},
		},
		{
			name:    "claude bypass maps to bypassPermissions",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			mode:    "bypass",
			want:    []string{"--permission-mode", "bypassPermissions", "-p", "prompt"},
		},

		// --- canonical rungs x claude-code (adapter-style name) -----------
		{
			name:    "claude-code plan",
			harness: "claude-code",
			args:    []string{"-p"},
			mode:    "plan",
			want:    []string{"--permission-mode", "plan", "-p"},
		},
		{
			name:    "claude-code manual",
			harness: "claude-code",
			args:    []string{"-p"},
			mode:    "manual",
			want:    []string{"--permission-mode", "manual", "-p"},
		},
		{
			name:    "claude-code ask",
			harness: "claude-code",
			args:    []string{"-p"},
			mode:    "ask",
			want:    []string{"--permission-mode", "acceptEdits", "-p"},
		},
		{
			name:    "claude-code auto",
			harness: "claude-code",
			args:    []string{"-p"},
			mode:    "auto",
			want:    []string{"--permission-mode", "auto", "-p"},
		},
		{
			name:    "claude-code bypass",
			harness: "claude-code",
			args:    []string{"-p"},
			mode:    "bypass",
			want:    []string{"--permission-mode", "bypassPermissions", "-p"},
		},

		// --- canonical rungs x codex --------------------------------------
		// "plan" is absent on purpose: it is REJECTED by validateConfig for
		// codex (see TestValidateConfig_PermissionMode).
		{
			name:    "codex manual",
			harness: "codex",
			args:    []string{"exec", "--json"},
			mode:    "manual",
			want:    []string{"-s", "read-only", "-a", "untrusted", "exec", "--json"},
		},
		{
			name:    "codex ask",
			harness: "codex",
			args:    []string{"exec", "--json"},
			mode:    "ask",
			want:    []string{"-s", "workspace-write", "-a", "on-request", "exec", "--json"},
		},
		{
			name:    "codex auto",
			harness: "codex",
			args:    []string{"exec", "--json"},
			mode:    "auto",
			want:    []string{"-s", "workspace-write", "-a", "never", "exec", "--json"},
		},
		{
			name:    "codex bypass",
			harness: "codex",
			args:    []string{"exec", "--json"},
			mode:    "bypass",
			want:    []string{"-s", "danger-full-access", "-a", "never", "exec", "--json"},
		},
		{
			// The live subcommand shape in this repo is `resume`, not `exec`:
			// pkg/turns/harness/codex/codex.go:153-155 ((*Adapter).ResumeArgs
			// returns {"resume", harnessSessionID}) feeds
			// pkg/chat/conversation.go:326-330, which prepends the resume
			// fragment ahead of the caller's args and hands the result to
			// wrapper.Config.Args. Flags ahead of a subcommand are accepted:
			//   codex -s workspace-write -a on-request resume --help -> exit 0
			// (probed on codex-cli 0.144.5, 2026-07-22).
			name:    "codex manual ahead of the resume subcommand",
			harness: "codex",
			args:    []string{"resume", "6f8d2890-4a81-4e44-b75f-2be81a1cb2f5"},
			mode:    "manual",
			want: []string{
				"-s", "read-only", "-a", "untrusted",
				"resume", "6f8d2890-4a81-4e44-b75f-2be81a1cb2f5",
			},
		},

		// --- native-spelling passthrough ----------------------------------
		{
			name:    "claude native acceptEdits passes through verbatim",
			harness: "claude",
			args:    []string{"-p"},
			mode:    "acceptEdits",
			want:    []string{"--permission-mode", "acceptEdits", "-p"},
		},
		{
			name:    "claude native dontAsk passes through verbatim",
			harness: "claude",
			args:    []string{"-p"},
			mode:    "dontAsk",
			want:    []string{"--permission-mode", "dontAsk", "-p"},
		},
		{
			name:    "claude native bypassPermissions passes through verbatim",
			harness: "claude",
			args:    []string{"-p"},
			mode:    "bypassPermissions",
			want:    []string{"--permission-mode", "bypassPermissions", "-p"},
		},
		{
			// A codex-native sandbox value sets the -s axis ONLY; the -a axis
			// is deliberately left untouched.
			name:    "codex native read-only sets the sandbox axis only",
			harness: "codex",
			args:    []string{"exec"},
			mode:    "read-only",
			want:    []string{"-s", "read-only", "exec"},
		},
		{
			name:    "codex native workspace-write sets the sandbox axis only",
			harness: "codex",
			args:    []string{"exec"},
			mode:    "workspace-write",
			want:    []string{"-s", "workspace-write", "exec"},
		},
		{
			name:    "codex native danger-full-access sets the sandbox axis only",
			harness: "codex",
			args:    []string{"exec"},
			mode:    "danger-full-access",
			want:    []string{"-s", "danger-full-access", "exec"},
		},

		// --- explicit flag in args wins: claude ---------------------------
		{
			name:    "claude existing --permission-mode wins",
			harness: "claude",
			args:    []string{"--permission-mode", "plan", "-p"},
			mode:    "bypass",
			want:    []string{"--permission-mode", "plan", "-p"},
		},
		{
			name:    "claude existing --permission-mode=value wins",
			harness: "claude",
			args:    []string{"--permission-mode=plan", "-p"},
			mode:    "bypass",
			want:    []string{"--permission-mode=plan", "-p"},
		},
		{
			// bypass is the ONLY mode class that reaches this arm: every other
			// mode paired with --dangerously-skip-permissions is rejected by
			// validateConfig before Start reaches injection.
			name:    "claude existing skip-permissions wins for bypass",
			harness: "claude",
			args:    []string{SkipPermissionsFlag, "-p"},
			mode:    "bypass",
			want:    []string{SkipPermissionsFlag, "-p"},
		},
		{
			name:    "claude existing skip-permissions=value wins for bypassPermissions",
			harness: "claude",
			args:    []string{SkipPermissionsFlag + "=true", "-p"},
			mode:    "bypassPermissions",
			want:    []string{SkipPermissionsFlag + "=true", "-p"},
		},

		// --- explicit flag in args wins: codex (whole-directive) ----------
		{
			name:    "codex existing -s wins on both axes",
			harness: "codex",
			args:    []string{"-s", "read-only", "exec"},
			mode:    "bypass",
			want:    []string{"-s", "read-only", "exec"},
		},
		{
			name:    "codex existing --sandbox wins",
			harness: "codex",
			args:    []string{"--sandbox", "read-only", "exec"},
			mode:    "bypass",
			want:    []string{"--sandbox", "read-only", "exec"},
		},
		{
			name:    "codex existing --sandbox=value wins",
			harness: "codex",
			args:    []string{"--sandbox=read-only", "exec"},
			mode:    "bypass",
			want:    []string{"--sandbox=read-only", "exec"},
		},
		{
			name:    "codex existing -a wins",
			harness: "codex",
			args:    []string{"-a", "untrusted", "exec"},
			mode:    "auto",
			want:    []string{"-a", "untrusted", "exec"},
		},
		{
			name:    "codex existing --ask-for-approval wins",
			harness: "codex",
			args:    []string{"--ask-for-approval", "untrusted", "exec"},
			mode:    "auto",
			want:    []string{"--ask-for-approval", "untrusted", "exec"},
		},
		{
			name:    "codex existing --ask-for-approval=value wins",
			harness: "codex",
			args:    []string{"--ask-for-approval=untrusted", "exec"},
			mode:    "auto",
			want:    []string{"--ask-for-approval=untrusted", "exec"},
		},
		{
			name:    "codex existing -s=value wins",
			harness: "codex",
			args:    []string{"-s=read-only", "exec"},
			mode:    "bypass",
			want:    []string{"-s=read-only", "exec"},
		},
		{
			name:    "codex existing -a=value wins",
			harness: "codex",
			args:    []string{"-a=never", "exec"},
			mode:    "manual",
			want:    []string{"-a=never", "exec"},
		},
		{
			// clap's attached short form: no separator between flag and value.
			name:    "codex attached short -sread-only wins",
			harness: "codex",
			args:    []string{"-sread-only", "exec"},
			mode:    "bypass",
			want:    []string{"-sread-only", "exec"},
		},
		{
			name:    "codex attached short -auntrusted wins",
			harness: "codex",
			args:    []string{"-auntrusted", "exec"},
			mode:    "auto",
			want:    []string{"-auntrusted", "exec"},
		},
		{
			// Partial specification: the caller pinned only the sandbox axis.
			// Whole-directive wins — we do NOT inject the missing -a, because a
			// half-injected combination is not what either side asked for.
			name:    "codex partial specification suppresses both axes",
			harness: "codex",
			args:    []string{"-s", "workspace-write", "resume", "abc"},
			mode:    "manual",
			want:    []string{"-s", "workspace-write", "resume", "abc"},
		},
		{
			name:    "codex existing bypass flag wins for bypass mode",
			harness: "codex",
			args:    []string{codexBypassFlag, "exec"},
			mode:    "bypass",
			want:    []string{codexBypassFlag, "exec"},
		},
		{
			name:    "codex existing bypass flag wins for danger-full-access",
			harness: "codex",
			args:    []string{codexBypassFlag, "exec"},
			mode:    "danger-full-access",
			want:    []string{codexBypassFlag, "exec"},
		},

		// --- no-ops -------------------------------------------------------
		{
			name:    "empty mode leaves args unchanged",
			harness: "claude",
			args:    []string{"-p", "prompt"},
			mode:    "",
			want:    []string{"-p", "prompt"},
		},
		{
			// Defence in depth: validateConfig rejects a non-empty mode for an
			// unsupported harness before Start ever reaches injection, so this
			// branch is only reachable by calling the function directly. It
			// mirrors argsWithHarnessEffort's default: arm.
			name:    "unsupported harness leaves args unchanged",
			harness: "opencode",
			args:    []string{"-p", "prompt"},
			mode:    "bypass",
			want:    []string{"-p", "prompt"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := argsWithHarnessPermissionMode(tc.harness, tc.args, tc.mode)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argsWithHarnessPermissionMode() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestArgsWithHarnessPermissionMode_PrependOrdering pins the injection position
// with the suppression check deliberately bypassed: it calls prependArgs the
// way argsWithHarnessPermissionMode would if the caller's own flag had not been
// detected. The injected copy must come FIRST, so that claude's and clap's own
// last-wins parsing still lets the caller's later flag win — a second line of
// defence behind the suppression list, not a substitute for it.
func TestArgsWithHarnessPermissionMode_PrependOrdering(t *testing.T) {
	callerClaude := []string{"--permission-mode", "plan", "-p"}
	got := prependArgs(callerClaude, "--permission-mode", claudePermissionMode("bypass"))
	want := []string{"--permission-mode", "bypassPermissions", "--permission-mode", "plan", "-p"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claude ordering = %v, want %v", got, want)
	}

	callerCodex := []string{"-s", "read-only", "exec"}
	sandbox, approval := codexPermissionMode("ask")
	got = prependArgs(callerCodex, "-s", sandbox, "-a", approval)
	want = []string{"-s", "workspace-write", "-a", "on-request", "-s", "read-only", "exec"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codex ordering = %v, want %v", got, want)
	}

	// With suppression in force, neither call above injects anything.
	if got := argsWithHarnessPermissionMode("claude", callerClaude, "bypass"); !reflect.DeepEqual(got, callerClaude) {
		t.Fatalf("claude suppression = %v, want %v", got, callerClaude)
	}
	if got := argsWithHarnessPermissionMode("codex", callerCodex, "ask"); !reflect.DeepEqual(got, callerCodex) {
		t.Fatalf("codex suppression = %v, want %v", got, callerCodex)
	}
}

func TestIsBypassPermissionMode(t *testing.T) {
	for _, mode := range []string{"bypass", "bypassPermissions"} {
		if !IsBypassPermissionMode(mode) {
			t.Errorf("IsBypassPermissionMode(%q) = false, want true", mode)
		}
	}
	// "danger-full-access" is excluded ON PURPOSE. The --sandbox-defaults
	// exclusion check in cmd/harness-wrapper.parseHarnessWrapperArgs runs
	// before the harness name is known, so counting codex's bypass-equivalent
	// as bypass here would let `--sandbox-defaults --permission-mode
	// danger-full-access codex --` slip past that check.
	for _, mode := range []string{"danger-full-access", "manual", "ask", "auto", "plan", ""} {
		if IsBypassPermissionMode(mode) {
			t.Errorf("IsBypassPermissionMode(%q) = true, want false", mode)
		}
	}
}

func TestIsCodexBypassMode(t *testing.T) {
	for _, mode := range []string{"bypass", "danger-full-access"} {
		if !isCodexBypassMode(mode) {
			t.Errorf("isCodexBypassMode(%q) = false, want true", mode)
		}
	}
	for _, mode := range []string{"bypassPermissions", "manual", "ask", "auto", "plan", "read-only", ""} {
		if isCodexBypassMode(mode) {
			t.Errorf("isCodexBypassMode(%q) = true, want false", mode)
		}
	}
}

func TestIsSupportedPermissionMode(t *testing.T) {
	tests := []struct {
		harness string
		mode    string
		want    bool
	}{
		// Canonical rungs are accepted for both harnesses at this layer;
		// plan-on-codex is rejected separately (it is a supported vocabulary
		// word with no codex launch-time flag).
		{"claude", "plan", true},
		{"claude", "manual", true},
		{"claude", "ask", true},
		{"claude", "auto", true},
		{"claude", "bypass", true},
		{"claude-code", "bypass", true},
		{"codex", "manual", true},
		{"codex", "ask", true},
		{"codex", "auto", true},
		{"codex", "bypass", true},
		{"codex", "plan", true},

		// Native spellings, own harness.
		{"claude", "acceptEdits", true},
		{"claude", "dontAsk", true},
		{"claude", "bypassPermissions", true},
		{"claude-code", "acceptEdits", true},
		{"codex", "read-only", true},
		{"codex", "workspace-write", true},
		{"codex", "danger-full-access", true},

		// Native spellings, WRONG harness — rejected, never silently ignored.
		{"codex", "acceptEdits", false},
		{"codex", "dontAsk", false},
		{"codex", "bypassPermissions", false},
		{"claude", "read-only", false},
		{"claude", "workspace-write", false},
		{"claude", "danger-full-access", false},
		{"claude-code", "workspace-write", false},

		// Flags are not values.
		{"claude", SkipPermissionsFlag, false},
		{"codex", codexBypassFlag, false},

		// codex 0.144.5 removed --full-auto; it is not a mode either.
		{"codex", "full-auto", false},

		{"claude", "nonsense", false},
		{"codex", "nonsense", false},
	}

	for _, tc := range tests {
		got := isSupportedPermissionMode(tc.harness, tc.mode)
		if got != tc.want {
			t.Errorf("isSupportedPermissionMode(%q, %q) = %v, want %v", tc.harness, tc.mode, got, tc.want)
		}
	}
}

// TestValidateConfig_PermissionMode asserts every rejection fires BEFORE the
// harness process is launched: Start calls validateConfig first, so a bogus
// BinaryPath is never resolved or spawned.
func TestValidateConfig_PermissionMode(t *testing.T) {
	rejections := []struct {
		name    string
		harness string
		args    []string
		mode    string
	}{
		{name: "unknown value on claude", harness: "claude", mode: "nonsense"},
		{name: "unknown value on codex", harness: "codex", mode: "nonsense"},
		{name: "claude-native spelling on codex", harness: "codex", mode: "acceptEdits"},
		{name: "claude-native bypassPermissions on codex", harness: "codex", mode: "bypassPermissions"},
		{name: "codex-native spelling on claude", harness: "claude", mode: "workspace-write"},
		{name: "codex-native danger-full-access on claude-code", harness: "claude-code", mode: "danger-full-access"},
		{name: "plan on codex", harness: "codex", mode: "plan"},
		{name: "mode on unsupported harness", harness: "opencode", mode: "manual"},
		{name: "mode on empty harness", harness: "", mode: "manual"},
		{
			name:    "claude non-bypass mode contradicts skip-permissions in args",
			harness: "claude",
			args:    []string{SkipPermissionsFlag, "-p"},
			mode:    "manual",
		},
		{
			name:    "claude-code non-bypass mode contradicts skip-permissions=value",
			harness: "claude-code",
			args:    []string{SkipPermissionsFlag + "=true"},
			mode:    "ask",
		},
		{
			name:    "codex non-bypass mode contradicts bypass flag in args",
			harness: "codex",
			args:    []string{codexBypassFlag, "exec"},
			mode:    "manual",
		},
		{
			name:    "codex read-only contradicts bypass flag in args",
			harness: "codex",
			args:    []string{codexBypassFlag, "exec"},
			mode:    "read-only",
		},
	}

	for _, tc := range rejections {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			cfg := Config{
				BinaryPath:     "/no/such/binary/harness-wrapper-test-missing",
				Stdout:         io.Discard,
				Harness:        tc.harness,
				Args:           tc.args,
				PermissionMode: tc.mode,
			}
			if err := validateConfig(&cfg); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("validateConfig() err = %v, want ErrInvalidConfig", err)
			}
			sess, err := Start(context.Background(), cfg)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("Start() err = %v, want ErrInvalidConfig", err)
			}
			if sess != nil {
				t.Fatal("Start() returned a session; no process may be spawned for an invalid config")
			}
		})
	}

	accepted := []struct {
		name    string
		harness string
		args    []string
		mode    string
	}{
		{name: "empty mode on unsupported harness", harness: "opencode", mode: ""},
		{name: "claude plan", harness: "claude", mode: "plan"},
		{name: "claude manual", harness: "claude", mode: "manual"},
		{name: "claude ask", harness: "claude", mode: "ask"},
		{name: "claude auto", harness: "claude", mode: "auto"},
		{name: "claude bypass", harness: "claude", mode: "bypass"},
		{name: "claude acceptEdits", harness: "claude", mode: "acceptEdits"},
		{name: "claude dontAsk", harness: "claude", mode: "dontAsk"},
		{name: "claude bypassPermissions", harness: "claude", mode: "bypassPermissions"},
		{name: "claude-code ask", harness: "claude-code", mode: "ask"},
		{name: "codex manual", harness: "codex", mode: "manual"},
		{name: "codex ask", harness: "codex", mode: "ask"},
		{name: "codex auto", harness: "codex", mode: "auto"},
		{name: "codex bypass", harness: "codex", mode: "bypass"},
		{name: "codex read-only", harness: "codex", mode: "read-only"},
		{name: "codex workspace-write", harness: "codex", mode: "workspace-write"},
		{name: "codex danger-full-access", harness: "codex", mode: "danger-full-access"},
		// A bare same-axis flag is the caller restating the axis, not a
		// contradiction: plain last-wins suppression, not a rejection.
		{
			name:    "claude bare --permission-mode in args is last-wins, not a contradiction",
			harness: "claude",
			args:    []string{"--permission-mode", "plan"},
			mode:    "manual",
		},
		{
			name:    "codex bare -s in args is last-wins, not a contradiction",
			harness: "codex",
			args:    []string{"-s", "read-only", "exec"},
			mode:    "auto",
		},
		{
			name:    "codex bare -a in args is last-wins, not a contradiction",
			harness: "codex",
			args:    []string{"-a", "never", "exec"},
			mode:    "manual",
		},
	}

	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			cfg := Config{
				BinaryPath:     "x",
				Stdout:         io.Discard,
				Harness:        tc.harness,
				Args:           tc.args,
				PermissionMode: tc.mode,
			}
			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("validateConfig() err = %v, want nil", err)
			}
		})
	}
}

// TestValidateConfig_PermissionModeMessages pins the two message texts the
// CLI, pkg/env and the TypeScript meta-harness half quote verbatim. Reword
// them here only in lockstep with those consumers.
func TestValidateConfig_PermissionModeMessages(t *testing.T) {
	planOnCodex := validateConfig(&Config{
		BinaryPath: "x", Stdout: io.Discard, Harness: "codex", PermissionMode: "plan",
	})
	const wantPlan = `permission mode "plan" is not supported by the codex harness (no launch-time flag; use /plan after launch)`
	if planOnCodex == nil || !strings.Contains(planOnCodex.Error(), wantPlan) {
		t.Fatalf("plan-on-codex err = %v, want it to contain %q", planOnCodex, wantPlan)
	}

	unsupported := validateConfig(&Config{
		BinaryPath: "x", Stdout: io.Discard, Harness: "opencode", PermissionMode: "manual",
	})
	const wantUnsupported = "PermissionMode is only supported for claude and codex harnesses"
	if unsupported == nil || !strings.Contains(unsupported.Error(), wantUnsupported) {
		t.Fatalf("unsupported-harness err = %v, want it to contain %q", unsupported, wantUnsupported)
	}

	contradiction := validateConfig(&Config{
		BinaryPath: "x", Stdout: io.Discard, Harness: "claude",
		Args: []string{SkipPermissionsFlag}, PermissionMode: "manual",
	})
	const wantContradiction = `PermissionMode "manual" contradicts --dangerously-skip-permissions in Args`
	if contradiction == nil || !strings.Contains(contradiction.Error(), wantContradiction) {
		t.Fatalf("claude contradiction err = %v, want it to contain %q", contradiction, wantContradiction)
	}

	codexContradiction := validateConfig(&Config{
		BinaryPath: "x", Stdout: io.Discard, Harness: "codex",
		Args: []string{codexBypassFlag}, PermissionMode: "manual",
	})
	const wantCodexContradiction = `PermissionMode "manual" contradicts --dangerously-bypass-approvals-and-sandbox in Args`
	if codexContradiction == nil || !strings.Contains(codexContradiction.Error(), wantCodexContradiction) {
		t.Fatalf("codex contradiction err = %v, want it to contain %q", codexContradiction, wantCodexContradiction)
	}
}

// TestValidateConfig_BypassModeWithBypassFlagSuppressesInjection covers the
// negative of the contradictory-argv rejection: a bypass-class mode paired with
// the matching bypass flag is ACCEPTED, and injection is suppressed so argv
// carries exactly one permission directive.
func TestValidateConfig_BypassModeWithBypassFlagSuppressesInjection(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		args    []string
		mode    string
	}{
		{"claude bypass", "claude", []string{SkipPermissionsFlag, "-p"}, "bypass"},
		{"claude bypassPermissions", "claude", []string{SkipPermissionsFlag}, "bypassPermissions"},
		{"claude-code bypass", "claude-code", []string{SkipPermissionsFlag + "=true"}, "bypass"},
		{"codex bypass", "codex", []string{codexBypassFlag, "exec"}, "bypass"},
		{"codex danger-full-access", "codex", []string{codexBypassFlag, "exec"}, "danger-full-access"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				BinaryPath:     "x",
				Stdout:         io.Discard,
				Harness:        tc.harness,
				Args:           tc.args,
				PermissionMode: tc.mode,
			}
			if err := validateConfig(&cfg); err != nil {
				t.Fatalf("validateConfig() err = %v, want nil", err)
			}
			got := argsWithHarnessPermissionMode(tc.harness, tc.args, tc.mode)
			if !reflect.DeepEqual(got, tc.args) {
				t.Fatalf("argsWithHarnessPermissionMode() = %v, want args unchanged %v", got, tc.args)
			}
		})
	}
}

func TestArgsContainAnyFlag(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		flags []string
		want  bool
	}{
		{name: "bare short token", args: []string{"-s", "read-only"}, flags: []string{"-s"}, want: true},
		{name: "bare long token", args: []string{"--sandbox", "read-only"}, flags: []string{"--sandbox"}, want: true},
		{name: "attached long form", args: []string{"--sandbox=read-only"}, flags: []string{"--sandbox"}, want: true},
		{name: "attached short form", args: []string{"-sread-only"}, flags: []string{"-s"}, want: true},
		{name: "attached short form for -a", args: []string{"-auntrusted"}, flags: []string{"-a"}, want: true},
		{name: "matches any of several flags", args: []string{"exec", "-a", "never"}, flags: []string{"-s", "--sandbox", "-a"}, want: true},
		{name: "no match", args: []string{"exec", "--json"}, flags: []string{"-s", "-a"}, want: false},
		{name: "empty args", args: nil, flags: []string{"-s"}, want: false},
		{
			// A long flag has no attached-short form: "--sandboxes" must not
			// match "--sandbox" (only "--sandbox=" does).
			name:  "long flag prefix is not a match without =",
			args:  []string{"--sandboxes"},
			flags: []string{"--sandbox"},
			want:  false,
		},
		{
			// Documented one-sided false positive: the attached-short rule is a
			// prefix match, so a hypothetical single-dash token beginning with
			// "-a" also matches. codex exposes no such flag today, and the
			// consequence is suppression (argv left as written), never a
			// duplicated -a.
			name:  "prefix match also catches a hypothetical single-dash token",
			args:  []string{"-auto-something"},
			flags: []string{"-a"},
			want:  true,
		},
		{
			// The short rule applies only to ^-[a-z]$ shapes.
			name:  "double-dash token does not trip the short rule",
			args:  []string{"--sandbox-defaults"},
			flags: []string{"-s"},
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := argsContainAnyFlag(tc.args, tc.flags...); got != tc.want {
				t.Fatalf("argsContainAnyFlag(%v, %v) = %v, want %v", tc.args, tc.flags, got, tc.want)
			}
		})
	}
}

// TestPermissionModeNeverEmitsFullAuto pins the codex 0.144.5 removal: nothing
// this package emits may contain --full-auto, which now hard-errors.
func TestPermissionModeNeverEmitsFullAuto(t *testing.T) {
	for _, mode := range []string{"manual", "ask", "auto", "bypass", "read-only", "workspace-write", "danger-full-access"} {
		for _, arg := range argsWithHarnessPermissionMode("codex", []string{"exec"}, mode) {
			if strings.Contains(arg, "full-auto") {
				t.Fatalf("mode %q emitted --full-auto, which codex 0.144.5 removed", mode)
			}
		}
	}
}
