package codex

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

func TestProfileImplementsConfigDirResolver(t *testing.T) {
	var _ harness.ConfigDirResolver = Profile{}
}

func TestHarnessConfigDir(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"set", []string{"CODEX_HOME=/agents/w3/codex"}, "/agents/w3/codex"},
		{"unset", []string{"PATH=/usr/bin"}, ""},
		{"empty value", []string{"CODEX_HOME="}, ""},
		{"nil env", nil, ""},
		{"duplicate keys, last wins", []string{"CODEX_HOME=/first", "CODEX_HOME=/second"}, "/second"},
		{"prefix collision ignored", []string{"CODEX_HOME_EXTRA=/x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Profile{}).HarnessConfigDir(tc.env); got != tc.want {
				t.Fatalf("HarnessConfigDir = %q, want %q", got, tc.want)
			}
		})
	}
}
