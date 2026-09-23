package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	cc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// TestLocateAgreesWithTheAdapter: the transcript package's Locate, where a
// follower starts, finds the one transcript this adapter reads for the same
// launch env and working dir. The rules are written twice — pkg/transcript
// cannot import this package — and must not drift apart.
func TestLocateAgreesWithTheAdapter(t *testing.T) {
	home, wd, abs := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	real, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"reply"}]}}` + "\n")
	for _, tc := range []struct {
		name string
		env  []string
		root string
	}{
		{"unset", []string{"PATH=/usr/bin"}, filepath.Join(home, ".claude")},
		{"blank value", []string{"CLAUDE_CONFIG_DIR=   "}, filepath.Join(home, ".claude")},
		{"absolute", []string{"CLAUDE_CONFIG_DIR=" + abs}, abs},
		{"relative to the child's cwd", []string{"CLAUDE_CONFIG_DIR=.agent-config"}, filepath.Join(wd, ".agent-config")},
		{"duplicate keys, last wins", []string{"CLAUDE_CONFIG_DIR=/first", "CLAUDE_CONFIG_DIR=" + abs}, abs},
		{"prefix collision ignored", []string{"CLAUDE_CONFIG_DIR_EXTRA=" + abs}, filepath.Join(home, ".claude")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(tc.root, "projects", cc.EncodedCWD(real), "parity-session.jsonl")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Remove(path) }()

			a := New()
			a.ConfigureFromEnv(tc.env)
			if _, err := a.ReadTranscript("parity-session", wd); err != nil {
				t.Fatalf("the adapter does not read %s: %v", path, err)
			}
			if got, err := cc.Locate("parity-session", wd, tc.env); err != nil || got != path {
				t.Fatalf("Locate = %q, %v; the adapter reads %q", got, err, path)
			}
		})
	}
}
