package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/versions"
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

// pinnedRegistry serves <base>/<package>/latest the way registry.npmjs.org does,
// for every package pinned in versions.json, answering through respond. It is
// the controlled fixture for the Makefile contract: the recipe runs against a
// known registry state instead of the network or a presumed-dead port.
func pinnedRegistry(t *testing.T, respond func(pkg, pinned string) (status int, version string)) string {
	t.Helper()
	all, err := versions.All()
	if err != nil {
		t.Fatalf("versions.All: %v", err)
	}
	pinnedByPkg := make(map[string]string, len(all))
	for _, e := range all {
		pinnedByPkg[e.Package] = e.Pinned
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pkg := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/latest")
		pinned, ok := pinnedByPkg[pkg]
		if !ok {
			http.NotFound(w, r)
			return
		}
		status, version := respond(pkg, pinned)
		if status != http.StatusOK {
			http.Error(w, "registry unavailable", status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"version": version})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// hangUpRegistry accepts every connection and drops it without a response: a
// transport failure, not an HTTP one, and fully under the test's control.
func hangUpRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// current answers every package with its pin — or, for an unpinned package,
// with an arbitrary version, which check-versions reports as "unpinned".
func current(_, pinned string) (int, string) {
	if pinned == "" {
		return http.StatusOK, "1.0.0"
	}
	return http.StatusOK, pinned
}

// runCheckVersions runs `make check-versions` against registry and returns the
// exit code and combined output.
func runCheckVersions(t *testing.T, registry string) (int, string) {
	t.Helper()
	cmd := exec.Command("make", "-C", repoRoot(t), "check-versions",
		"CHECK_VERSIONS_ARGS=-registry "+registry+" -timeout 5s")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("running make: %v\noutput:\n%s", err, out)
	}
	return exitErr.ExitCode(), string(out)
}

// TestMakefileExitContract replays each registry state through the actual
// Makefile recipe, the layer the original defect lived in: it ran the program
// under `go run`, which collapses ANY non-zero child status to 1, so exit 2
// ("could not query the registry") arrived at the recipe's `case` as 1 and was
// announced as "drift detected", exit 0. The unit tests in main_test.go pin
// check(), exitCode(), writeVerdict() and writeTable(); none of them would
// notice someone reintroducing `go run ./cmd/check-versions`. This would.
//
// The contract, per state: pins current → exit 0, "all pins match"; drift →
// exit 0 (drift is reported, not a failure), "drift detected"; any probe
// failure → exit 2, "could not query" — including when other packages
// drifted, because a probe that never reached the registry says nothing about
// whether the pins are current.
func TestMakefileExitContract(t *testing.T) {
	if testing.Short() {
		t.Skip("shells out to make and builds a binary; skipped under -short")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not on PATH")
	}

	for _, tc := range []struct {
		name     string
		registry func(t *testing.T) string
		wantCode int
		want     string
		forbid   []string
	}{
		{
			name:     "all pins current",
			registry: func(t *testing.T) string { return pinnedRegistry(t, current) },
			wantCode: 0,
			want:     "✓ all pins match latest",
			forbid:   []string{"drift detected", "could not query"},
		},
		{
			name: "one pin behind",
			registry: func(t *testing.T) string {
				return pinnedRegistry(t, func(pkg, pinned string) (int, string) {
					if pkg == "@anthropic-ai/claude-code" {
						return http.StatusOK, "99.0.0"
					}
					return current(pkg, pinned)
				})
			},
			wantCode: 0,
			want:     "⚠ drift detected",
			forbid:   []string{"could not query", "all pins match"},
		},
		{
			name: "registry answers 503",
			registry: func(t *testing.T) string {
				return pinnedRegistry(t, func(string, string) (int, string) { return http.StatusServiceUnavailable, "" })
			},
			wantCode: 2,
			want:     "✗ could not query the npm registry",
			forbid:   []string{"drift detected", "all pins match"},
		},
		{
			name:     "registry drops the connection",
			registry: hangUpRegistry,
			wantCode: 2,
			want:     "✗ could not query the npm registry",
			forbid:   []string{"drift detected", "all pins match"},
		},
		{
			name: "one probe fails while another pin is behind",
			registry: func(t *testing.T) string {
				return pinnedRegistry(t, func(pkg, pinned string) (int, string) {
					switch pkg {
					case "@openai/codex":
						return http.StatusServiceUnavailable, ""
					case "@anthropic-ai/claude-code":
						return http.StatusOK, "99.0.0"
					}
					return current(pkg, pinned)
				})
			},
			wantCode: 2,
			want:     "✗ could not query the npm registry",
			forbid:   []string{"drift detected", "all pins match"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := runCheckVersions(t, tc.registry(t))
			if code != tc.wantCode {
				t.Errorf("exit %d, want %d\noutput:\n%s", code, tc.wantCode, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output lacks %q\noutput:\n%s", tc.want, out)
			}
			for _, f := range tc.forbid {
				if strings.Contains(out, f) {
					t.Errorf("output reports %q\noutput:\n%s", f, out)
				}
			}
		})
	}
}
