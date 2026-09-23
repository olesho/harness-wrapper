package harness_test

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Claude Code's session-nesting markers: nestingEnvKey is the exact-match one,
// nestingEnvPrefix the family prefix every other marker shares.
const (
	nestingEnvKey    = "CLAUDECODE"
	nestingEnvPrefix = "CLAUDE_CODE_"
)

// nestingExemptEnvKeys are keys matching nestingEnvPrefix that are NOT nesting
// markers, so they must survive the scrub. Exact match only:
// CLAUDE_CODE_OAUTH_TOKEN_FILE and friends still drop.
//
// CLAUDE_CODE_OAUTH_TOKEN is claude's headless credential, not something a
// running claude exports into its children — it just shares the prefix.
// Stripping it starts the spawned harness UNAUTHENTICATED wherever the token is
// the only working auth, and the run dies at the deadline (PUPPET-317).
var nestingExemptEnvKeys = map[string]bool{
	"CLAUDE_CODE_OAUTH_TOKEN": true,
}

// isClaudeNestingEnvKey reports whether key is one of Claude Code's session
// nesting markers. The exemption set is consulted first.
func isClaudeNestingEnvKey(key string) bool {
	if nestingExemptEnvKeys[key] {
		return false
	}
	return key == nestingEnvKey || strings.HasPrefix(key, nestingEnvPrefix)
}

// filterNestingEnv returns src minus the Claude Code nesting markers, keeping
// the relative order of the survivors — a later duplicate key wins in exec, so
// reordering would be observable. Pure, so the policy is testable without
// mutating the process environment.
func filterNestingEnv(src []string) []string {
	out := make([]string, 0, len(src))
	for _, kv := range src {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if isClaudeNestingEnvKey(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// scrubbedRealClaudeEnv returns os.Environ() minus Claude Code's session
// nesting markers, so a live dogfood spawns claude as a TOP-LEVEL session.
//
// This MIRRORS cmd/harness-wrapper/run.go's filterNestingEnv (the production
// policy) rather than importing it: that function is unexported in package
// main. Keep the two in step — cmd/harness-wrapper/env_test.go covers the
// original, the tests below cover this copy, and the two case tables are
// deliberately the same shape so a divergence shows up as one of them failing.
//
// Why the live tests need it: an inherited CLAUDE_CODE_CHILD_SESSION makes
// claude paint "⚠ Transcript saving is off" and write no rollout.
// pkg/chat/swallowed.go then has nothing to overturn a screen-only swallow
// verdict with, so a turn that may really have run is reported as a prompt the
// harness never accepted. The live dogfood is the fleet's only canary for a new
// claude release; it must fail on the adapter, not on who its parent was.
//
// Call it AFTER any t.Setenv the test depends on (CLAUDE_CONFIG_DIR in
// run_turn_untrusted_test.go): it materializes the environment as it stands.
// CLAUDE_CONFIG_DIR itself is not a nesting marker and survives.
func scrubbedRealClaudeEnv(t *testing.T) []string {
	t.Helper()
	return filterNestingEnv(os.Environ())
}

// TestIsClaudeNestingEnvKey pins the nesting predicate key by key. The rows that
// matter most are the exact-match neighbours of the credential (…_TOKEN_FILE,
// …_TOKENX) and the lowercase form: they are what catches a future
// "simplification" of the exemption into a prefix test. Mirrors the table in
// cmd/harness-wrapper/env_test.go.
func TestIsClaudeNestingEnvKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"exact nesting marker", "CLAUDECODE", true},
		{"child session marker", "CLAUDE_CODE_CHILD_SESSION", true},
		{"entrypoint marker", "CLAUDE_CODE_ENTRYPOINT", true},
		{"unknown future marker keeps default-deny", "CLAUDE_CODE_SOMETHING_NEW", true},
		{"bare family prefix", "CLAUDE_CODE_", true},
		{"the credential is exempt", "CLAUDE_CODE_OAUTH_TOKEN", false},
		{"exemption is exact: _FILE suffix", "CLAUDE_CODE_OAUTH_TOKEN_FILE", true},
		{"exemption is exact: X suffix", "CLAUDE_CODE_OAUTH_TOKENX", true},
		{"lowercase form matches nothing", "claude_code_oauth_token", false},
		{"the config dir is not a nesting marker", "CLAUDE_CONFIG_DIR", false},
		{"anthropic key", "ANTHROPIC_API_KEY", false},
		{"prefix without trailing underscore", "CLAUDE_CODEX", false},
		{"ordinary key", "PATH", false},
		{"empty key", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isClaudeNestingEnvKey(tt.key); got != tt.want {
				t.Errorf("isClaudeNestingEnvKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

// TestFilterNestingEnvKeepsCredentialAndOrder is the shape the live tests
// depend on: the markers go, the credential and the config dir stay byte
// identical, and the surviving order is unchanged.
func TestFilterNestingEnvKeepsCredentialAndOrder(t *testing.T) {
	t.Parallel()

	in := []string{
		"PATH=/usr/bin:/bin",
		"CLAUDECODE=1",
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CONFIG_DIR=/tmp/profile",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test",
		"CLAUDE_CODE_",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE=/tmp/tok",
	}
	want := []string{
		"PATH=/usr/bin:/bin",
		"CLAUDE_CONFIG_DIR=/tmp/profile",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test",
	}

	if got := filterNestingEnv(in); !reflect.DeepEqual(got, want) {
		t.Errorf("filterNestingEnv() = %q, want %q", got, want)
	}
}

// TestFilterNestingEnvEdgeForms covers the shapes os.Environ() will not produce
// but a caller passing an arbitrary slice can.
func TestFilterNestingEnvEdgeForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "bare credential key with no = survives verbatim",
			in:   []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDECODE"},
			want: []string{"CLAUDE_CODE_OAUTH_TOKEN"},
		},
		{
			name: "empty credential value survives verbatim",
			in:   []string{"CLAUDE_CODE_OAUTH_TOKEN=", "CLAUDE_CODE_ENTRYPOINT="},
			want: []string{"CLAUDE_CODE_OAUTH_TOKEN="},
		},
		{
			name: "empty key is kept",
			in:   []string{"=weird"},
			want: []string{"=weird"},
		},
		{
			name: "duplicate keys keep their order",
			in:   []string{"CLAUDE_CODE_OAUTH_TOKEN=first", "PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=second"},
			want: []string{"CLAUDE_CODE_OAUTH_TOKEN=first", "PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=second"},
		},
		{
			name: "value containing = is not re-split",
			in:   []string{"CLAUDE_CODE_OAUTH_TOKEN=a=b=c"},
			want: []string{"CLAUDE_CODE_OAUTH_TOKEN=a=b=c"},
		},
		{
			name: "empty input yields an empty non-nil slice",
			in:   []string{},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filterNestingEnv(tt.in)
			if got == nil {
				t.Fatal("filterNestingEnv() returned nil, want non-nil slice")
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("filterNestingEnv(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestScrubbedRealClaudeEnvMaterializesProcessEnv pins the os.Environ() wiring
// the live tests actually call. No t.Parallel: t.Setenv and t.Parallel are
// mutually exclusive.
func TestScrubbedRealClaudeEnvMaterializesProcessEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-test")
	t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/profile")

	got := scrubbedRealClaudeEnv(t)

	for _, wanted := range []string{"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test", "CLAUDE_CONFIG_DIR=/tmp/profile"} {
		if !slices.Contains(got, wanted) {
			t.Errorf("scrubbedRealClaudeEnv() dropped %q", wanted)
		}
	}
	for _, unwanted := range []string{"CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("scrubbedRealClaudeEnv() kept nesting marker %q; claude would run as a nested session with transcript saving off", unwanted)
		}
	}
}
