package versions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRootPackageJSONBootstrap pins the repo-root package.json invariant.
//
// The dev→release promotion gate runs `npm install` with the repository root
// as cwd. That step requires a root package.json to exist; without one npm
// exits 254 (ENOENT) and the release gate goes RED (HARNESS-WRAPPER-40).
//
// The file is a deliberate no-op bootstrap anchor (see .gitignore's
// "Root package.json exists only as a no-op npm bootstrap target"), NOT part
// of the deleted in-repo TypeScript port. This test guards against a future
// cleanup silently re-removing it and re-breaking the gate.
func TestRootPackageJSONBootstrap(t *testing.T) {
	// Resolve the repo root relative to this test file so the test runs
	// hermetically under `go test -race ./...` regardless of cwd.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	path := filepath.Join(repoRoot, "package.json")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("repo-root package.json must exist as an npm bootstrap target for the release gate: %v", err)
	}

	var pkg map[string]any
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("repo-root package.json must be valid JSON: %v", err)
	}

	if priv, _ := pkg["private"].(bool); !priv {
		t.Errorf(`repo-root package.json must set "private": true (got %v)`, pkg["private"])
	}
}
