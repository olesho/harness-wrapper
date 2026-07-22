package wrapper

import (
	"slices"
	"testing"
)

func TestPermissionRungsOrder(t *testing.T) {
	want := []string{"plan", "manual", "ask", "auto", "bypass"}
	if got := PermissionRungs(); !slices.Equal(got, want) {
		t.Fatalf("PermissionRungs() = %v, want %v", got, want)
	}
}

func TestPermissionRungsFreshSlice(t *testing.T) {
	first := PermissionRungs()
	for i := range first {
		first[i] = "clobbered"
	}
	first = append(first, "extra") //nolint:staticcheck // exercising caller mutation
	_ = first

	want := []string{"plan", "manual", "ask", "auto", "bypass"}
	if got := PermissionRungs(); !slices.Equal(got, want) {
		t.Fatalf("PermissionRungs() after caller mutation = %v, want %v", got, want)
	}
}

func TestMorePermissive(t *testing.T) {
	rungs := PermissionRungs()

	// Every ordered pair of canonical rungs: strictly-greater index only.
	for i, a := range rungs {
		for j, b := range rungs {
			want := i > j
			if got := MorePermissive(a, b); got != want {
				t.Errorf("MorePermissive(%q, %q) = %v, want %v", a, b, got, want)
			}
		}
	}

	// Unknown / empty / native spellings fail closed on either side.
	unknowns := []string{"", "acceptEdits", "dontAsk", "bypassPermissions", "danger-full-access", "read-only", "workspace-write", "totallyUnknown"}
	for _, u := range unknowns {
		for _, r := range rungs {
			if MorePermissive(u, r) {
				t.Errorf("MorePermissive(%q, %q) = true, want false (unknown a fails closed)", u, r)
			}
			if MorePermissive(r, u) {
				t.Errorf("MorePermissive(%q, %q) = true, want false (unknown b fails closed)", r, u)
			}
		}
		if MorePermissive(u, u) {
			t.Errorf("MorePermissive(%q, %q) = true, want false", u, u)
		}
	}
}

func TestBypassEnablingFlags(t *testing.T) {
	tests := []struct {
		harness string
		want    []string
	}{
		{"claude", []string{SkipPermissionsFlag}},
		{"claude-code", []string{SkipPermissionsFlag}},
		{"Claude-Code", []string{SkipPermissionsFlag}},
		{"codex", []string{codexBypassFlag}},
		{"pi", nil},
		{"opencode", nil},
		{"generic", nil},
		{"", nil},
	}
	for _, tt := range tests {
		t.Run(tt.harness, func(t *testing.T) {
			if got := BypassEnablingFlags(tt.harness); !slices.Equal(got, tt.want) {
				t.Fatalf("BypassEnablingFlags(%q) = %v, want %v", tt.harness, got, tt.want)
			}
		})
	}
}

func TestBypassEnablingFlagsNeverIncludesNonexistentFlag(t *testing.T) {
	// --allow-dangerously-skip-permissions does not exist in this repo.
	for _, harness := range []string{"claude", "claude-code", "codex", "pi", ""} {
		for _, flag := range BypassEnablingFlags(harness) {
			if flag != SkipPermissionsFlag && flag != codexBypassFlag {
				t.Fatalf("BypassEnablingFlags(%q) returned unknown flag %q", harness, flag)
			}
		}
	}
}

func TestEffectiveLaunchRung(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		args    []string
		mode    string
		want    string
	}{
		{"claude knob bypass", "claude", nil, "bypass", "bypass"},
		{"claude argv separated native bypass", "claude", []string{"--permission-mode", "bypassPermissions"}, "", "bypass"},
		{"claude argv attached native bypass", "claude", []string{"--permission-mode=bypassPermissions"}, "", "bypass"},
		{"claude argv skip-permissions flag", "claude", []string{SkipPermissionsFlag}, "", "bypass"},
		{"claude argv trailing flag no operand", "claude", []string{"--permission-mode"}, "", ""},
		{"claude argv unknown value", "claude", []string{"--permission-mode", "totallyUnknown"}, "", ""},
		{"claude knob plan", "claude", nil, "plan", "plan"},
		{"claude knob acceptEdits", "claude", nil, "acceptEdits", "ask"},
		{"claude knob dontAsk is unknown", "claude", nil, "dontAsk", ""},
		{"claude knob empty", "claude", nil, "", ""},
		{"claude argv wins over knob", "claude", []string{"--permission-mode", "plan"}, "bypass", "plan"},
		{"claude argv canonical rung passthrough", "claude", []string{"--permission-mode=manual"}, "", "manual"},
		{"claude-code adapter name", "claude-code", []string{"--permission-mode=acceptEdits"}, "", "ask"},

		{"codex argv separated danger-full-access", "codex", []string{"-s", "danger-full-access"}, "", "bypass"},
		{"codex argv attached long read-only", "codex", []string{"--sandbox=read-only"}, "", "manual"},
		{"codex argv attached short workspace-write", "codex", []string{"-sworkspace-write"}, "", "ask"},
		{"codex argv bypass flag", "codex", []string{codexBypassFlag}, "", "bypass"},
		{"codex argv trailing -s no operand", "codex", []string{"-s"}, "", ""},
		{"codex argv unknown sandbox value", "codex", []string{"-s", "totallyUnknown"}, "", ""},
		{"codex argv approval axis only", "codex", []string{"-a", "never"}, "bypass", ""},
		{"codex knob bypass", "codex", nil, "bypass", "bypass"},
		{"codex knob manual", "codex", nil, "manual", "manual"},
		{"codex knob native workspace-write", "codex", nil, "workspace-write", "ask"},
		{"codex knob empty", "codex", nil, "", ""},
		{"codex argv wins over knob", "codex", []string{"-s", "read-only"}, "bypass", "manual"},

		{"unsupported harness", "pi", []string{"--permission-mode", "bypassPermissions"}, "bypass", ""},
		{"empty harness", "", nil, "bypass", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EffectiveLaunchRung(tt.harness, tt.args, tt.mode); got != tt.want {
				t.Fatalf("EffectiveLaunchRung(%q, %v, %q) = %q, want %q", tt.harness, tt.args, tt.mode, got, tt.want)
			}
		})
	}
}

// EffectiveLaunchRung must agree with the injection path it replays: when
// argsWithHarnessPermissionMode suppresses injection, the argv value is what
// the harness launched with.
func TestEffectiveLaunchRungMatchesInjection(t *testing.T) {
	tests := []struct {
		harness string
		args    []string
		mode    string
	}{
		{"claude", nil, "bypass"},
		{"claude", nil, "plan"},
		{"claude", []string{"--permission-mode", "plan"}, "bypass"},
		{"codex", nil, "manual"},
		{"codex", []string{"-s", "read-only"}, "bypass"},
	}
	for _, tt := range tests {
		got := EffectiveLaunchRung(tt.harness, tt.args, tt.mode)
		out := argsWithHarnessPermissionMode(tt.harness, tt.args, tt.mode)
		var want string
		switch normHarness(tt.harness) {
		case "claude", harnessClaudeCode:
			v, _ := flagValue(out, "--permission-mode")
			want = claudeRung(v)
		case "codex":
			v, _ := flagValue(out, "-s", "--sandbox")
			want = codexSandboxRung(v)
		}
		if got != want {
			t.Errorf("EffectiveLaunchRung(%q, %v, %q) = %q, but injected argv %v resolves to %q",
				tt.harness, tt.args, tt.mode, got, out, want)
		}
	}
}
