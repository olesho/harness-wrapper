package codex

import (
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// TestAdapterImplementsEnvConfigurable keeps codex in step with claude-code:
// both resolve their on-disk root from the harness launch env, so neither can
// regress to $HOME alone without a test failing.
func TestAdapterImplementsEnvConfigurable(t *testing.T) {
	var _ turns.EnvConfigurable = New()
}

func TestConfigureFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"set", []string{"CODEX_HOME=/p/agent/codex"}, filepath.Join("/p/agent/codex", "sessions")},
		{"unset", []string{"PATH=/usr/bin"}, ""},
		{"empty value", []string{"CODEX_HOME="}, ""},
		{"blank value", []string{"CODEX_HOME= \t"}, ""},
		{"nil env", nil, ""},
		{"duplicate keys, last wins", []string{"CODEX_HOME=/first", "CODEX_HOME=/second"}, filepath.Join("/second", "sessions")},
		{"prefix collision ignored", []string{"CODEX_HOME_EXTRA=/x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := New()
			a.ConfigureFromEnv(tc.env)
			if a.SessionsRoot != tc.want {
				t.Fatalf("SessionsRoot = %q, want %q", a.SessionsRoot, tc.want)
			}
		})
	}
}
