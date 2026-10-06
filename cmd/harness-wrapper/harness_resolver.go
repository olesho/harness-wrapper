package main

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// harnessSpec describes how to invoke a single harness from the CLI: only
// Bin, the executable name to look up via PATH. Per-harness behavior
// (classifier patterns, argv shape) lives in internal/wrapcore/harness/<name>.
type harnessSpec struct {
	Bin string
}

// supportedHarnesses is the CLI's registry of harness short names and their
// binaries. Per-harness behavior lives in internal/wrapcore/harness/<name>;
// this map only names what the CLI accepts.
var supportedHarnesses = map[string]harnessSpec{
	"codex":    {Bin: "codex"},
	"claude":   {Bin: "claude"},
	"opencode": {Bin: "opencode"},
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

// supportedHarnessNames lists the registry's names, sorted and comma-separated,
// for error messages and the usage text.
func supportedHarnessNames() string {
	return strings.Join(slices.Sorted(maps.Keys(supportedHarnesses)), ", ")
}
