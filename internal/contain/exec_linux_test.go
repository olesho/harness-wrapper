//go:build linux

package contain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLookPathInRefusesWorkingDirectoryEntries: /usr/bin/env searches an
// empty or relative PATH entry in the working directory, which the harness
// can write, so the node the codex shim runs would not be the one checked and
// granted. Such a PATH is refused rather than skipped.
func TestLookPathInRefusesWorkingDirectoryEntries(t *testing.T) {
	bin := t.TempDir()
	node := filepath.Join(bin, "node")
	if err := os.WriteFile(node, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := lookPathIn("node", []string{"PATH=/nonexistent:" + bin})
	if err != nil || got != node {
		t.Fatalf("absolute PATH: got %q, %v; want %q", got, err, node)
	}
	for _, path := range []string{
		":" + bin,
		bin + ":",
		bin + "::/usr/bin",
		"bin:" + bin,
		bin + ":./bin",
		bin + ":.",
	} {
		got, err := lookPathIn("node", []string{"PATH=" + path})
		if err == nil || !strings.Contains(err.Error(), "empty or relative entry") {
			t.Errorf("PATH=%q: got %q, %v; want a refusal", path, got, err)
		}
	}
	if _, err := lookPathIn("node", []string{"PATH="}); err == nil {
		t.Error("empty PATH: found node")
	}
}
