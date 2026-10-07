package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/harness"
	// Registers every built-in harness profile: the CLI's supported harnesses
	// are exactly the pkg/harness registry's names.
	_ "github.com/olesho/harness-wrapper/pkg/harness/all"
)

// harnessSpec describes how to invoke a single harness from the CLI: only
// Bin, the executable name to look up via PATH. Per-harness behavior
// (classifier patterns, argv shape) lives in internal/wrapcore/harness/<name>.
type harnessSpec struct {
	Bin string
}

// lookupHarness resolves a CLI harness name through the pkg/harness profile
// registry — the single table of harnesses the CLI and RunTurn's callers share.
// The name is matched EXACTLY against the registry keys (the short names
// "claude", "codex", "opencode"); the "claude-code" alias is deliberately not
// folded here (see applySandboxDefaults and transcriptReaderFor, which key on
// the short name).
func lookupHarness(name string) (harnessSpec, bool) {
	p, ok := harness.For(name)
	if !ok {
		return harnessSpec{}, false
	}
	return harnessSpec{Bin: harness.BinaryName(p)}, true
}

// resolveHarness looks up a harness by short name and returns the
// absolute path to its binary on PATH. Returns an error with a hint if
// the name is unknown or if the binary is not installed.
func resolveHarness(name string) (string, error) {
	spec, ok := lookupHarness(name)
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
	return strings.Join(harness.Registered(), ", ")
}
