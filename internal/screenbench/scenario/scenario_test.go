//go:build screenbench

package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryMetaJSONHasBinaryVersion walks the repo's test/corpus tree
// and fails for any non-synth recording with an empty binary_version.
// Synth scenarios are emulator-bench artifacts and intentionally
// declare binary_version="screenbench-synth" or leave it for the
// generator to populate; this test exempts them.
//
// Recordings against real upstream CLIs (codex, claude-code, gemini, …)
// must always pin the upstream version they were taken against so the
// upgrade playbook has a starting point.
func TestEveryMetaJSONHasBinaryVersion(t *testing.T) {
	root := repoCorpusRoot(t)
	scenarios, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover(%s): %v", root, err)
	}
	if len(scenarios) == 0 {
		t.Fatalf("Discover(%s) returned no scenarios; corpus layout broken?", root)
	}
	for _, s := range scenarios {
		if s.Meta.Harness == "synth" {
			continue
		}
		if strings.TrimSpace(s.Meta.BinaryVersion) == "" {
			t.Errorf("scenario %s (harness=%s) has empty binary_version", s.Path, s.Meta.Harness)
		}
	}
}

// repoCorpusRoot returns the absolute path to test/corpus, walking up
// from this test's CWD to find the repo root.
func repoCorpusRoot(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			return filepath.Join(cwd, "test", "corpus")
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			break
		}
		cwd = parent
	}
	t.Fatalf("could not find repo root from %s", cwd)
	return ""
}
