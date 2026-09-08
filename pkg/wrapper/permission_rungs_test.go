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
	slices.Reverse(first)

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

	// Unknown / empty / native spellings fail closed on either side. dontAsk
	// stays here even though claudeRung now maps it onto the manual rung:
	// MorePermissive takes CANONICAL rungs only, so rungIndex("dontAsk") must
	// keep returning -1, exactly as it does for acceptEdits/bypassPermissions.
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
		// dontAsk is a second claude-native spelling of the manual rung, exactly
		// as acceptEdits is of ask: claude's own permissiveness rank table
		// ({plan:0, bubble:1, default:1, dontAsk:1, acceptEdits:2, auto:3,
		// bypassPermissions:4}) ties dontAsk with default, and PermissionRungs()
		// is a strict total order that cannot express a tie.
		{"claude knob dontAsk is manual", "claude", nil, "dontAsk", "manual"},
		{"claude argv separated native dontAsk", "claude", []string{"--permission-mode", "dontAsk"}, "", "manual"},
		{"claude argv attached native dontAsk", "claude", []string{"--permission-mode=dontAsk"}, "", "manual"},
		{"claude knob empty", "claude", nil, "", ""},
		{"claude argv wins over knob", "claude", []string{"--permission-mode", "plan"}, "bypass", "plan"},
		{"claude argv canonical rung passthrough", "claude", []string{"--permission-mode=manual"}, "", "manual"},
		{"claude-code adapter name", "claude-code", []string{"--permission-mode=acceptEdits"}, "", "ask"},
		{"claude argv attached-equals acceptEdits", "claude", []string{"--permission-mode=acceptEdits"}, "", "ask"},
		// Never under-report permissiveness: the blanket flag is checked first, so
		// an argv carrying BOTH a restrictive --permission-mode and the blanket
		// flag reports the more permissive rung. validatePermissionMode rejects
		// this combination as a LAUNCH, but EffectiveLaunchRung is called at cmd/
		// independently of validateConfig, so it is reachable as a RESOLUTION —
		// which is exactly what the structured-run startup_error guard relies on.
		{"claude blanket bypass flag beats restrictive mode flag", "claude", []string{"--permission-mode", "plan", SkipPermissionsFlag}, "", "bypass"},
		{"claude blanket bypass flag beats restrictive knob", "claude", []string{SkipPermissionsFlag}, "plan", "bypass"},
		// Duplicated flags: claude's parser is last-wins, so the resolver must be
		// too — reporting "plan" for a launch that lands on bypass is the one
		// direction a safety field must never fail in.
		{"claude argv duplicate last wins", "claude", []string{"--permission-mode", "plan", "--permission-mode", "bypassPermissions"}, "", "bypass"},
		{"claude argv duplicate last wins attached", "claude", []string{"--permission-mode=bypassPermissions", "--permission-mode=plan"}, "", "plan"},

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
		// The safety-critical spelling: clap's attached short form WITH an equals.
		// The "flag=" arm must precede the attached-short arm, or the value reads
		// back as "=danger-full-access" and the most dangerous codex launch
		// reports nothing.
		{"codex argv short attached-equals danger-full-access", "codex", []string{"-s=danger-full-access"}, "", "bypass"},
		// The sandbox axis wins over -a REGARDLESS of what -a says: a codex-native
		// danger-full-access knob emits `-s danger-full-access` with NO -a at all,
		// so a "bare -s means unknown" reading would silently drop it.
		{"codex argv sandbox wins over approval axis", "codex", []string{"-sworkspace-write", "-aon-request"}, "", "ask"},
		{"codex argv attached-short approval only", "codex", []string{"-aon-request"}, "manual", ""},
		{"codex argv duplicate sandbox last wins", "codex", []string{"-s", "read-only", "-s", "danger-full-access"}, "", "bypass"},

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
		{"claude", nil, "dontAsk"},
		{"claude", []string{"--permission-mode", "dontAsk"}, ""},
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

// EffectiveLaunchRung is idempotent over injection: feeding it already-injected
// argv resolves to the same rung as feeding it the caller's raw argv. This is
// what removes the footgun from the exported API — a caller that does not know
// whether it holds pre- or post-injection args cannot get a different answer.
func TestEffectiveLaunchRungIdempotentOverInjection(t *testing.T) {
	tests := []struct {
		harness string
		args    []string
		mode    string
	}{
		{"claude", nil, "bypass"},
		{"claude", nil, "plan"},
		{"claude", nil, "acceptEdits"},
		{"claude", nil, "dontAsk"},
		{"claude", []string{"--permission-mode", "dontAsk"}, ""},
		{"claude", []string{"--permission-mode=dontAsk"}, "bypass"},
		{"claude", []string{"--permission-mode", "plan"}, "bypass"},
		{"claude", []string{SkipPermissionsFlag}, "plan"},
		{"claude-code", nil, "manual"},
		{"codex", nil, "manual"},
		{"codex", nil, "danger-full-access"},
		{"codex", []string{"-s", "read-only"}, "bypass"},
		{"codex", []string{codexBypassFlag}, "manual"},
		{"pi", nil, "bypass"},
	}
	for _, tt := range tests {
		raw := EffectiveLaunchRung(tt.harness, tt.args, tt.mode)
		injected := EffectiveLaunchRung(tt.harness, argsWithHarnessPermissionMode(tt.harness, tt.args, tt.mode), tt.mode)
		if raw != injected {
			t.Errorf("EffectiveLaunchRung(%q, %v, %q) = %q, but over injected argv = %q (not idempotent)",
				tt.harness, tt.args, tt.mode, raw, injected)
		}
	}
}

// Whenever argv already carries a suppression-set flag, the resolution comes
// from ARGV, not from the knob — the mirror of TestArgsWithHarnessPermissionMode's
// suppression cases, stated as a property rather than row by row.
func TestEffectiveLaunchRungResolvesFromArgvWhenSuppressed(t *testing.T) {
	tests := []struct {
		harness string
		args    []string
		want    string
	}{
		{"claude", []string{"--permission-mode", "plan"}, "plan"},
		{"claude", []string{"--permission-mode=acceptEdits"}, "ask"},
		{"claude", []string{SkipPermissionsFlag}, "bypass"},
		{"codex", []string{"-s", "read-only"}, "manual"},
		{"codex", []string{"-sworkspace-write", "-aon-request"}, "ask"},
		{"codex", []string{"-s=danger-full-access"}, "bypass"},
		{"codex", []string{codexBypassFlag}, "bypass"},
	}
	// Every canonical rung as a knob must be ignored in favour of argv.
	for _, tt := range tests {
		suppressors := suppressionFlagsFor(tt.harness)
		if !argsContainAnyFlag(tt.args, suppressors...) {
			t.Fatalf("test setup: %v does not trip suppression for %q", tt.args, tt.harness)
		}
		for _, mode := range append(PermissionRungs(), "") {
			if got := EffectiveLaunchRung(tt.harness, tt.args, mode); got != tt.want {
				t.Errorf("EffectiveLaunchRung(%q, %v, %q) = %q, want %q from argv",
					tt.harness, tt.args, mode, got, tt.want)
			}
		}
	}
}

// suppressionFlagsFor is the test-local mirror of the flag set
// argsWithHarnessPermissionMode consults before injecting.
func suppressionFlagsFor(harness string) []string {
	switch normHarness(harness) {
	case "claude", harnessClaudeCode:
		return []string{"--permission-mode", SkipPermissionsFlag}
	case "codex":
		return []string{"-s", "--sandbox", "-a", "--ask-for-approval", codexBypassFlag}
	default:
		return nil
	}
}
