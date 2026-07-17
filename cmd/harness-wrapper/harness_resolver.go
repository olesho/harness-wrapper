package main

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
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
	"codex":    {Bin: "codex"},
	"claude":   {Bin: "claude"},
	"gemini":   {Bin: "gemini"},
	"opencode": {Bin: "opencode"},
	"pi":       {Bin: "pi"},
}

// resolveHarness looks up a harness by short name and returns the
// absolute path to its binary on PATH. Returns an error with a hint if
// the name is unknown or if the binary is not installed.
func resolveHarness(name string) (string, error) {
	spec, ok := supportedHarnesses[name]
	if !ok {
		return "", fmt.Errorf("unsupported harness %q (supported: %s)", name, supportedHarnessNames())
	}
	// Binary override seam (mirrors MH structured-runner's resolveBinaryPath):
	// honor HARNESS_BINARY_<NAME> then a generic HARNESS_BINARY, checked BEFORE
	// PATH resolution so tests can inject a scripted fake hermetically. When no
	// override env is present, plain PATH resolution is unaffected.
	if override := harnessBinaryOverride(name); override != "" {
		return override, nil
	}
	path, err := exec.LookPath(spec.Bin)
	if err != nil {
		return "", fmt.Errorf("harness %q not found in PATH: %w", spec.Bin, err)
	}
	return path, nil
}

// harnessBinaryOverride returns the caller-supplied override binary path for a
// harness short name, or "" when none is set. HARNESS_BINARY_<NAME> (name
// upper-cased, '-'→'_') wins over the generic HARNESS_BINARY.
func harnessBinaryOverride(name string) string {
	key := "HARNESS_BINARY_" + strings.ReplaceAll(strings.ToUpper(name), "-", "_")
	if v := os.Getenv(key); v != "" {
		return v
	}
	return os.Getenv("HARNESS_BINARY")
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
