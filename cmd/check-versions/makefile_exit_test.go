package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from this test file's directory to the nearest
// directory holding a Makefile. Deliberately not "../.." relative to the
// process CWD: `go test` sets CWD to the package directory today, but the
// point of this test is the Makefile, so it should find it by looking for
// it rather than by counting path segments.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "Makefile")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no Makefile found in any parent of the package directory")
		}
		dir = parent
	}
}

// TestMakefileReportsRegistryOutageAsError is the regression test for the
// defect this whole command's exit-code contract exists to serve, and it
// lives at the layer the defect actually lived in: the Makefile recipe.
//
// The recipe used to run the program under `go run`, which collapses ANY
// non-zero child status to 1 — so exit 2 ("could not query the registry")
// arrived at the `case` as 1 and was announced as "drift detected", exit 0.
// The 14 unit tests in main_test.go pin check(), exitCode(), writeVerdict()
// and writeTable(); none of them would notice someone reintroducing
// `go run ./cmd/check-versions` tomorrow. This one would.
//
// Port 9 is the reserved "discard" port: it is expected to refuse
// immediately, so every probe errors without touching the network. The
// message assertions below double as the guard for the unlikely host where
// something IS listening there — a confusing pass is worse than a failure.
func TestMakefileReportsRegistryOutageAsError(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to make and builds a binary; skipped under -short")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not on PATH")
	}

	cmd := exec.Command("make", "-C", repoRoot(t), "check-versions",
		"CHECK_VERSIONS_ARGS=-registry http://127.0.0.1:9 -timeout 2s")
	out, err := cmd.CombinedOutput()
	got := string(out)

	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running make: %v\noutput:\n%s", err, got)
		}
		code = exitErr.ExitCode()
	}

	if code != 2 {
		t.Errorf("make check-versions against a dead registry: exit %d, want 2\noutput:\n%s", code, got)
	}
	if !strings.Contains(got, "could not query") {
		t.Errorf("output does not report the outage; want a %q line\noutput:\n%s", "could not query", got)
	}
	for _, forbidden := range []string{"drift detected", "all pins match"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("an unreachable registry was reported as %q — nothing was compared to anything\noutput:\n%s", forbidden, got)
		}
	}
}
