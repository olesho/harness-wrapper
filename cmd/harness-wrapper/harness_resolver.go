package main

import (
	"fmt"
	"os/exec"
	"sort"
)

// harnessSpec describes how to invoke a single harness. Phase 1 only
// uses Bin (the executable name to look up via PATH); future phases
// will likely add per-harness defaults like prompt patterns and
// command-line shape, at which point this will move into a
// harness-specific package.
type harnessSpec struct {
	Bin string
}

// supportedHarnesses is the Phase 1 inline registry. When a third
// harness lands or per-harness behavior diverges, this should graduate
// to internal/wrapper/harness/<name>/ packages.
var supportedHarnesses = map[string]harnessSpec{
	"codex":  {Bin: "codex"},
	"claude": {Bin: "claude"},
	"gemini": {Bin: "gemini"},
}

// resolveHarness looks up a harness by short name and returns the
// absolute path to its binary on PATH. Returns an error with a hint if
// the name is unknown or if the binary is not installed.
func resolveHarness(name string) (string, error) {
	spec, ok := supportedHarnesses[name]
	if !ok {
		return "", fmt.Errorf("unsupported harness %q (supported: %s)", name, supportedHarnessNames())
	}
	path, err := exec.LookPath(spec.Bin)
	if err != nil {
		return "", fmt.Errorf("harness %q not found in PATH: %w", spec.Bin, err)
	}
	return path, nil
}

func supportedHarnessNames() string {
	names := make([]string, 0, len(supportedHarnesses))
	for name := range supportedHarnesses {
		names = append(names, name)
	}
	sort.Strings(names)
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
