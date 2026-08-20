package profile

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestPackageIsPure enforces the boundary the package doc promises, mechanically:
// no process execution, no keychain access, no environment reads, no real
// filesystem. Every entry point takes an fs.FS or plain strings, so a
// provisioner that writes and a supervisor that refuses to boot can both depend
// on this package without inheriting each other's side effects.
//
// Non-test sources only: tests legitimately use os for t.TempDir() fixtures.
func TestPackageIsPure(t *testing.T) {
	forbidden := map[string]string{
		"os":            "the package must never touch the real filesystem or environment; take an fs.FS",
		"os/exec":       "the package must never execute a process (no security(1), no `claude --version`)",
		"os/user":       "host identity is the caller's to supply; see AuthSlot.Account",
		"path/filepath": "manifest paths are always slash-separated fs.FS paths",
		"net":           "no network",
		"net/http":      "no network",
	}

	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	parsed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: bad import %s", name, imp.Path.Value)
			}
			if why, bad := forbidden[path]; bad {
				t.Errorf("%s imports %q: %s", name, path, why)
			}
			// pkg/discovery execs binaries; pkg/versions answers a different
			// question (upstream npm pins, not what the installed binary
			// reports). This package depends on neither.
			if strings.HasPrefix(path, "github.com/olesho/harness-wrapper/") {
				t.Errorf("%s imports %q: pkg/profile has no internal dependencies", name, path)
			}
		}
	}
	if parsed == 0 {
		t.Fatal("no non-test sources found")
	}
}
