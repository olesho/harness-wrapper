package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestApplySandboxDefaults freezes the injection policy: claude gets
// --dangerously-skip-permissions + IS_SANDBOX=1, other harnesses are a
// documented no-op, and both injections are idempotent against
// caller-supplied values (including the --flag=value spelling and exact-key
// env matching at the '=' boundary).
//
// It also freezes the --permission-mode composition matrix. The two halves
// --sandbox-defaults contributes are NOT interchangeable: a bypass rung
// delivers only the ARGS half (pkg/wrapper emits --permission-mode
// bypassPermissions), while IS_SANDBOX=1 — the half that allows root and
// suppresses claude's acceptance screen — comes from --sandbox-defaults alone.
// So the pair COMPOSES: the arg append is skipped, the env append is not, and
// argv ends up with exactly one permission directive. Every other mode is
// rejected earlier by parseHarnessWrapperArgs and never reaches this function.
func TestApplySandboxDefaults(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		// mode is written out as "" on the pre-composition rows rather than
		// left to the zero value: those rows are the cross-repo argv tripwire
		// (crossrepo/meta-harness/HARNESS-WRAPPER-78-sandbox-defaults-argv.md),
		// so if this field ever gains a non-empty default they must fail loudly
		// instead of quietly changing meaning.
		mode     string
		args     []string
		env      []string
		wantArgs []string
		wantEnv  []string
	}{
		{
			name:     "claude injects flag and env",
			harness:  "claude",
			mode:     "",
			args:     []string{"--model", "opus"},
			env:      []string{"PATH=/usr/bin"},
			wantArgs: []string{"--model", "opus", "--dangerously-skip-permissions"},
			wantEnv:  []string{"PATH=/usr/bin", "IS_SANDBOX=1"},
		},
		{
			name:     "codex is a no-op",
			harness:  "codex",
			mode:     "",
			args:     []string{"--model", "o3"},
			env:      []string{"PATH=/usr/bin"},
			wantArgs: []string{"--model", "o3"},
			wantEnv:  []string{"PATH=/usr/bin"},
		},
		{
			name:     "dedup exact flag token",
			harness:  "claude",
			mode:     "",
			args:     []string{"--dangerously-skip-permissions"},
			env:      []string{},
			wantArgs: []string{"--dangerously-skip-permissions"},
			wantEnv:  []string{"IS_SANDBOX=1"},
		},
		{
			name:     "dedup =value flag form",
			harness:  "claude",
			mode:     "",
			args:     []string{"--dangerously-skip-permissions=true"},
			env:      []string{},
			wantArgs: []string{"--dangerously-skip-permissions=true"},
			wantEnv:  []string{"IS_SANDBOX=1"},
		},
		{
			name:     "existing IS_SANDBOX value is left untouched",
			harness:  "claude",
			mode:     "",
			args:     []string{},
			env:      []string{"IS_SANDBOX=0"},
			wantArgs: []string{"--dangerously-skip-permissions"},
			wantEnv:  []string{"IS_SANDBOX=0"},
		},
		{
			name:     "prefix-sharing key does not suppress injection",
			harness:  "claude",
			mode:     "",
			args:     []string{},
			env:      []string{"IS_SANDBOXED=1"},
			wantArgs: []string{"--dangerously-skip-permissions"},
			wantEnv:  []string{"IS_SANDBOXED=1", "IS_SANDBOX=1"},
		},
		{
			name:     "empty inputs still inject for claude",
			harness:  "claude",
			mode:     "",
			args:     nil,
			env:      nil,
			wantArgs: []string{"--dangerously-skip-permissions"},
			wantEnv:  []string{"IS_SANDBOX=1"},
		},
		// --- composition matrix -------------------------------------------
		{
			// bypass + --sandbox-defaults: the ENV HALF ONLY. Args come back
			// untouched, so the single permission directive in argv is the
			// --permission-mode bypassPermissions that pkg/wrapper prepends —
			// no --dangerously-skip-permissions double-spelling beside it.
			name:     "bypass composes: env half only",
			harness:  "claude",
			mode:     "bypass",
			args:     []string{"--model", "opus"},
			env:      []string{"PATH=/usr/bin"},
			wantArgs: []string{"--model", "opus"},
			wantEnv:  []string{"PATH=/usr/bin", "IS_SANDBOX=1"},
		},
		{
			// bypassPermissions is claude's native spelling of the same rung;
			// wrapper.IsBypassPermissionMode accepts both, so it composes
			// identically. The env contributed is byte-identical to the row
			// above — composition changes argv only, never the env half.
			name:     "bypassPermissions composes identically",
			harness:  "claude",
			mode:     "bypassPermissions",
			args:     []string{"--model", "opus"},
			env:      []string{"PATH=/usr/bin"},
			wantArgs: []string{"--model", "opus"},
			wantEnv:  []string{"PATH=/usr/bin", "IS_SANDBOX=1"},
		},
		{
			// The env half stays subject to the pre-existing hasEnvKey
			// idempotence: composing never overwrites a caller-set IS_SANDBOX.
			name:     "bypass composes but respects existing IS_SANDBOX",
			harness:  "claude",
			mode:     "bypass",
			args:     []string{},
			env:      []string{"IS_SANDBOX=0"},
			wantArgs: []string{},
			wantEnv:  []string{"IS_SANDBOX=0"},
		},
		{
			// The harnessName guard is an EXACT match with no normHarness
			// normalization, so "claude-code" is a full no-op even in a
			// compose-shaped invocation. Load-bearing: normalizing it would
			// widen which invocations receive the root-enabling env half.
			name:     "non-claude name is a no-op even with a bypass mode",
			harness:  "claude-code",
			mode:     "bypass",
			args:     []string{"--model", "opus"},
			env:      []string{"PATH=/usr/bin"},
			wantArgs: []string{"--model", "opus"},
			wantEnv:  []string{"PATH=/usr/bin"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotArgs, gotEnv := applySandboxDefaults(tt.harness, tt.mode, tt.args, tt.env)
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
			if !reflect.DeepEqual(gotEnv, tt.wantEnv) {
				t.Errorf("env = %v, want %v", gotEnv, tt.wantEnv)
			}
			// No row may ever leave TWO spellings of the same permission
			// intent in argv: --sandbox-defaults contributes at most one
			// --dangerously-skip-permissions, and contributes none at all when
			// pkg/wrapper is about to prepend --permission-mode.
			if n := countSkipPermissionsFlags(gotArgs); n > 1 {
				t.Errorf("args carry %d permission directives, want at most 1: %v", n, gotArgs)
			}
		})
	}
}

// countSkipPermissionsFlags counts --dangerously-skip-permissions tokens
// (bare and =value forms) in args.
func countSkipPermissionsFlags(args []string) int {
	n := 0
	for _, a := range args {
		if a == skipPermissionsFlag || strings.HasPrefix(a, skipPermissionsFlag+"=") {
			n++
		}
	}
	return n
}

// TestPermissionModeBypassAloneSetsNoSandboxEnv covers the matrix row that
// applySandboxDefaults cannot: --permission-mode bypass WITHOUT
// --sandbox-defaults. The CLI gates the whole injection on parsed
// SandboxDefaults (run.go / structured_run.go), so a lone bypass never reaches
// applySandboxDefaults at all — claude gets --permission-mode bypassPermissions
// from pkg/wrapper and NO IS_SANDBOX=1, meaning the acceptance screen still
// appears and root is still disallowed. That is the deliberate difference
// between the two flags; asserting the gate here keeps a future refactor from
// quietly granting the env half to every bypass run.
func TestPermissionModeBypassAloneSetsNoSandboxEnv(t *testing.T) {
	parsed, err := parseHarnessWrapperArgs([]string{"--permission-mode", "bypass", "claude", "--"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.SandboxDefaults {
		t.Fatal("SandboxDefaults should be false when only --permission-mode is set")
	}
	if parsed.PermissionMode != "bypass" {
		t.Fatalf("PermissionMode = %q, want bypass", parsed.PermissionMode)
	}
	// The env the harness would receive is the untouched cleaned env: no
	// injection call happens, so no IS_SANDBOX key is contributed.
	env := []string{"PATH=/usr/bin"}
	if hasEnvKey(env, sandboxEnvKey) {
		t.Fatalf("test fixture already defines %s", sandboxEnvKey)
	}
}
