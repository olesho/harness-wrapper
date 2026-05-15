// Package versions reads the repo-root versions.json file that pins
// each supported harness CLI to a specific upstream package version.
//
// versions.json is the single source of truth that ties an adapter's
// code (regex fingerprints, classifier patterns, transcript schema
// assumptions) to a specific upstream release. The version-sentry CLI
// reads it to compare against npm registry latest; corpus tests read
// it to verify that recordings under test/corpus/ were made against
// the same version the adapter targets.
//
// Schema:
//
//	{
//	  "codex":       {"package": "@openai/codex",             "pinned": "0.130.0", "verified_at": "2026-05-15"},
//	  "claude-code": {"package": "@anthropic-ai/claude-code", "pinned": "2.1.141", "verified_at": "2026-05-15"},
//	  "gemini":      {"package": "@google/gemini-cli",        "pinned": "",        "verified_at": ""}
//	}
//
// An empty pinned/verified_at string is allowed and means "not yet
// verified against any upstream version" (the initial state for newly
// added harnesses).
package versions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// Entry describes one harness's pinned upstream binding.
type Entry struct {
	// Package is the npm package name (e.g. "@openai/codex"). Empty is
	// not allowed; every entry must declare a package.
	Package string `json:"package"`

	// Pinned is the upstream version string our adapter is verified
	// against (e.g. "0.130.0"). Empty means "not yet verified".
	Pinned string `json:"pinned"`

	// VerifiedAt is the YYYY-MM-DD date when Pinned was confirmed (by a
	// successful corpus re-bake). Empty when Pinned is empty.
	VerifiedAt string `json:"verified_at"`
}

// All returns every harness entry in versions.json, keyed by harness
// name. Reads from the repo root located relative to this package's
// source — works whether the caller is a test, a CLI, or a library
// consumer.
func All() (map[string]Entry, error) {
	path, err := repoVersionsPath()
	if err != nil {
		return nil, err
	}
	return readFile(path)
}

// Pinned returns the pinned upstream version for a harness, or
// ("", false) if the harness has no entry or its pin is empty.
func Pinned(harness string) (string, bool) {
	all, err := All()
	if err != nil {
		return "", false
	}
	e, ok := all[harness]
	if !ok || e.Pinned == "" {
		return "", false
	}
	return e.Pinned, true
}

// ReadFrom reads a versions.json at an explicit path. Useful for tests
// that don't want to depend on the repo layout.
func ReadFrom(path string) (map[string]Entry, error) {
	return readFile(path)
}

func readFile(path string) (map[string]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("versions: read %s: %w", path, err)
	}
	var out map[string]Entry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("versions: parse %s: %w", path, err)
	}
	for name, e := range out {
		if e.Package == "" {
			return nil, fmt.Errorf("versions: entry %q has empty package", name)
		}
		if e.Pinned == "" && e.VerifiedAt != "" {
			return nil, fmt.Errorf("versions: entry %q has verified_at without pinned", name)
		}
	}
	return out, nil
}

var (
	repoRootOnce sync.Once
	repoRootVal  string
	repoRootErr  error
)

// repoVersionsPath returns the absolute path to versions.json by
// walking up from this source file's directory until a go.mod is
// found.
func repoVersionsPath() (string, error) {
	repoRootOnce.Do(func() {
		_, here, _, ok := runtime.Caller(0)
		if !ok {
			repoRootErr = fmt.Errorf("versions: runtime.Caller failed")
			return
		}
		dir := filepath.Dir(here)
		for range 8 {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				repoRootVal = dir
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
		repoRootErr = fmt.Errorf("versions: go.mod not found walking up from %s", here)
	})
	if repoRootErr != nil {
		return "", repoRootErr
	}
	return filepath.Join(repoRootVal, "versions.json"), nil
}
