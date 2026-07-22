package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Layer 0 of the test pyramid (see TESTING.md): freeze the CLI flag surface so
// an accidental rename / removal / retype / default change fails loudly here and
// forces a conscious golden update. Regenerate after an INTENTIONAL change with:
// UPDATE_GOLDEN=1 go test ./cmd/harness-wrapper/

func TestContract_Flags(t *testing.T) {
	fs := harnessWrapperFlagSet(&harnessWrapperArgs{})
	assertGolden(t, "flags.golden", flagSurface(fs))
}

// TestContract_Usage freezes the human-facing --help text. printUsage is the
// only place a user learns that --permission-mode is NOT a drop-in for
// --sandbox-defaults (bypass sets no IS_SANDBOX=1, so the acceptance screen
// still appears and root is still refused) and that restrictive rungs bind
// fully only with a human at the TUI. Wording that load-bearing should not
// drift silently, so it lives behind a golden like the flag surface does.
func TestContract_Usage(t *testing.T) {
	var sb strings.Builder
	printUsage(&sb)
	assertGolden(t, "usage.golden", sb.String())
}

// flagSurface renders a FlagSet as a stable, sorted "name <type> = <default>
// : <usage>" line per flag — the frozen shape of the binary's CLI.
func flagSurface(fs *flag.FlagSet) string {
	var lines []string
	fs.VisitAll(func(f *flag.Flag) {
		typ := reflect.TypeOf(f.Value).String()
		lines = append(lines, fmt.Sprintf("--%s <%s> = %q : %s", f.Name, typ, f.DefValue, f.Usage))
	})
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

// assertGolden compares got against testdata/<name>, regenerating it when
// UPDATE_GOLDEN=1. A mismatch means the frozen contract changed.
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (create it with UPDATE_GOLDEN=1): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("contract drift in %s — if intentional, regenerate with UPDATE_GOLDEN=1\n--- got ---\n%s\n--- want ---\n%s", name, got, string(want))
	}
}
