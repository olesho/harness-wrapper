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
//	  "codex":       {"package": "@openai/codex",             "binary": "codex",    "pinned": "0.130.0", "verified_at": "2026-05-15"},
//	  "claude-code": {"package": "@anthropic-ai/claude-code", "binary": "claude",   "pinned": "2.1.141", "verified_at": "2026-05-15"},
//	  "opencode":    {"package": "opencode-ai",               "binary": "opencode", "pinned": "",        "verified_at": ""},
//	  "pi":          {"package": "@earendil-works/pi-coding-agent", "binary": "pi",  "pinned": "",        "verified_at": ""}
//	}
//
// An empty pinned/verified_at string is allowed and means "not yet
// verified against any upstream version" (the initial state for newly
// added harnesses). Package and Binary are required for every entry.
package versions

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed versions.json
var embedded []byte

// Entry describes one harness's pinned upstream binding.
type Entry struct {
	// Package is the npm package name (e.g. "@openai/codex"). Empty is
	// not allowed; every entry must declare a package.
	Package string `json:"package"`

	// Binary is the on-PATH executable name installed by Package (e.g.
	// "claude" for the "@anthropic-ai/claude-code" package). Empty is
	// not allowed; consumers rely on this to probe availability.
	Binary string `json:"binary"`

	// Pinned is the upstream version string our adapter is verified
	// against (e.g. "0.130.0"). Empty means "not yet verified".
	Pinned string `json:"pinned"`

	// VerifiedAt is the YYYY-MM-DD date when Pinned was confirmed (by a
	// successful corpus re-bake). Empty when Pinned is empty.
	VerifiedAt string `json:"verified_at"`
}

// All returns every harness entry, keyed by harness name. The data is
// embedded into the package at build time, so the call works in any
// build mode (including -trimpath) and from any working directory.
func All() (map[string]Entry, error) {
	return parse(embedded)
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
// and tooling that want to operate on a different versions.json (e.g.
// the corpus rebake pipeline).
func ReadFrom(path string) (map[string]Entry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("versions: read %s: %w", path, err)
	}
	return parse(data)
}

func parse(data []byte) (map[string]Entry, error) {
	var out map[string]Entry
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("versions: parse: %w", err)
	}
	for name, e := range out {
		if e.Package == "" {
			return nil, fmt.Errorf("versions: entry %q has empty package", name)
		}
		if e.Binary == "" {
			return nil, fmt.Errorf("versions: entry %q has empty binary", name)
		}
		if e.Pinned == "" && e.VerifiedAt != "" {
			return nil, fmt.Errorf("versions: entry %q has verified_at without pinned", name)
		}
	}
	return out, nil
}
