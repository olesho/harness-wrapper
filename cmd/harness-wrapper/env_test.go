package main

import (
	"reflect"
	"slices"
	"testing"
)

// TestIsClaudeNestingEnvKey pins the nesting predicate key by key. The rows
// that matter most are the exact-match neighbours of the credential
// (…_TOKEN_FILE, …_TOKENX) and the lowercase form: they are what catches a
// future "simplification" of the exemption into a prefix test.
func TestIsClaudeNestingEnvKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"exact nesting marker", "CLAUDECODE", true},
		{"entrypoint marker", "CLAUDE_CODE_ENTRYPOINT", true},
		{"sse port marker", "CLAUDE_CODE_SSE_PORT", true},
		{"unknown future marker keeps default-deny", "CLAUDE_CODE_SOMETHING_NEW", true},
		{"the credential is exempt", "CLAUDE_CODE_OAUTH_TOKEN", false},
		{"exemption is exact: _FILE suffix", "CLAUDE_CODE_OAUTH_TOKEN_FILE", true},
		{"exemption is exact: X suffix", "CLAUDE_CODE_OAUTH_TOKENX", true},
		// Not exempt, but not a marker either: the whole predicate is
		// case-sensitive, so the lowercase form never matches the prefix and
		// is simply forwarded. Asserting false here records WHY it survives.
		{"lowercase form matches nothing", "claude_code_oauth_token", false},
		{"lowercase nesting marker matches nothing", "claudecode", false},
		{"unrelated claude key", "CLAUDE_API_KEY", false},
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

// TestFilterNestingEnvPreservesOAuthToken is the regression test for
// PUPPET-317: a realistic environment keeps its credential byte-identical while
// the genuine markers go, and the surviving order is unchanged.
func TestFilterNestingEnvPreservesOAuthToken(t *testing.T) {
	t.Parallel()

	in := []string{
		"PATH=/usr/bin:/bin",
		"CLAUDECODE=1",
		"HOME=/home/agent",
		"CLAUDE_CODE_ENTRYPOINT=cli",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test",
		"ANTHROPIC_API_KEY=sk-ant-api-test",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE=/tmp/tok",
	}
	want := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/agent",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test",
		"ANTHROPIC_API_KEY=sk-ant-api-test",
	}

	if got := filterNestingEnv(in); !reflect.DeepEqual(got, want) {
		t.Errorf("filterNestingEnv() = %q, want %q", got, want)
	}
}

// TestFilterNestingEnvEdgeForms covers the shapes os.Environ() will not
// produce but a caller passing an arbitrary slice can.
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

// TestCleanedEnvMaterializesProcessEnv pins the os.Environ() wiring. No
// t.Parallel here: t.Setenv and t.Parallel are mutually exclusive.
func TestCleanedEnvMaterializesProcessEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-test")

	got := cleanedEnv()

	if !slices.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test") {
		t.Error("cleanedEnv() dropped CLAUDE_CODE_OAUTH_TOKEN; the spawned harness would start unauthenticated")
	}
	for _, unwanted := range []string{"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("cleanedEnv() kept nesting marker %q", unwanted)
		}
	}
}
