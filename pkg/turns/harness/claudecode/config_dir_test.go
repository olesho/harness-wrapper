package claudecode

import (
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// TestAdapterImplementsEnvConfigurable pins the capability so a refactor that
// drops the method fails here rather than silently reverting every profiled
// agent to the operator's ~/.claude.
func TestAdapterImplementsEnvConfigurable(t *testing.T) {
	var _ turns.EnvConfigurable = New()
	var _ turns.TranscriptReader = New()
}

func TestConfigureFromEnv(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"set", []string{"CLAUDE_CONFIG_DIR=/p/agent/claude"}, filepath.Join("/p/agent/claude", "projects")},
		{"unset", []string{"PATH=/usr/bin", "HOME=/h"}, ""},
		{"empty value", []string{"CLAUDE_CONFIG_DIR="}, ""},
		{"blank value", []string{"CLAUDE_CONFIG_DIR=   "}, ""},
		{"nil env", nil, ""},
		// exec semantics: a caller appending an override to an inherited env
		// produces duplicate keys, and the LAST one is what the child sees.
		{"duplicate keys, last wins", []string{"CLAUDE_CONFIG_DIR=/first", "CLAUDE_CONFIG_DIR=/second"}, filepath.Join("/second", "projects")},
		// Left verbatim: claude resolves a relative root against the harness
		// child's cwd, not the wrapper's.
		{"relative stays relative", []string{"CLAUDE_CONFIG_DIR=.claude"}, filepath.Join(".claude", "projects")},
		// A same-prefixed variable must not be mistaken for ours.
		{"prefix collision ignored", []string{"CLAUDE_CONFIG_DIR_EXTRA=/x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := New()
			a.ConfigureFromEnv(tc.env)
			if a.ProjectsRoot != tc.want {
				t.Fatalf("ProjectsRoot = %q, want %q", a.ProjectsRoot, tc.want)
			}
		})
	}
}
