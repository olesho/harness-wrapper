package claude

import (
	"path/filepath"
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
		{"set", []string{"CLAUDE_CONFIG_DIR=/agents/w3/claude"}, "/agents/w3/claude"},
		{"unset", []string{"PATH=/usr/bin"}, ""},
		{"empty value", []string{"CLAUDE_CONFIG_DIR="}, ""},
		{"blank value", []string{"CLAUDE_CONFIG_DIR=  "}, ""},
		{"nil env", nil, ""},
		{"duplicate keys, last wins", []string{"CLAUDE_CONFIG_DIR=/first", "CLAUDE_CONFIG_DIR=/second"}, "/second"},
		{"prefix collision ignored", []string{"CLAUDE_CONFIG_DIR_EXTRA=/x"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Profile{}).HarnessConfigDir(tc.env); got != tc.want {
				t.Fatalf("HarnessConfigDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestValidateTranscriptPath_ConfigDir is the consumer end of the fix: once
// HW_HARNESS_CONFIG_DIR reaches the hook subprocess, a payload naming the
// PROFILED agent's transcript is accepted — while the root and session-id
// checks stay exactly as strict as before.
func TestValidateTranscriptPath_ConfigDir(t *testing.T) {
	const sessID = "11111111-2222-3333-4444-555555555555"
	const home = "/Users/operator"
	const cfgDir = "/agents/w3/claude"

	profiled := filepath.Join(cfgDir, "projects", "-wt", sessID+".jsonl")
	operator := filepath.Join(home, ".claude", "projects", "-wt", sessID+".jsonl")

	// With the config dir set, the agent's own path is accepted...
	withCfg := harness.HookContext{Home: home, ConfigDir: cfgDir}
	if err := validateTranscriptPath(withCfg, sessID, profiled); err != nil {
		t.Fatalf("profiled path rejected with ConfigDir set: %v", err)
	}
	// ...and a path under a DIFFERENT root is still rejected.
	if err := validateTranscriptPath(withCfg, sessID, operator); err == nil {
		t.Fatal("operator-rooted path accepted with ConfigDir set, want rejection")
	}
	// The basename check is not weakened by widening the root.
	other := filepath.Join(cfgDir, "projects", "-wt", "99999999-0000-0000-0000-000000000000.jsonl")
	if err := validateTranscriptPath(withCfg, sessID, other); err == nil {
		t.Fatal("foreign session id accepted, want rejection")
	}
	// Unset ConfigDir keeps the <Home>/.claude fallback — the unprofiled case.
	noCfg := harness.HookContext{Home: home}
	if err := validateTranscriptPath(noCfg, sessID, operator); err != nil {
		t.Fatalf("home-rooted path rejected with ConfigDir unset: %v", err)
	}
	if err := validateTranscriptPath(noCfg, sessID, profiled); err == nil {
		t.Fatal("profiled path accepted with ConfigDir unset, want rejection")
	}
}
