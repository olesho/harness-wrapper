package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/apparmor"
)

func TestContainAppArmorProfile(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runContainAppArmorProfile([]string{"--root", link}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	roots, err := apparmor.ParseRoots(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(real)
	if !slices.Equal(roots, []string{want}) {
		t.Fatalf("roots = %v, want the symlink resolved: [%s]", roots, want)
	}
}

func TestContainAppArmorProfileUsage(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no root":       nil,
		"missing root":  {"--root", "/nonexistent/harness-wrapper-root"},
		"file root":     {"--root", file},
		"slash root":    {"--root", "/"},
		"stray operand": {"--root", "/tmp", "extra"},
	} {
		var out, errb bytes.Buffer
		if code := runContainAppArmorProfile(args, &out, &errb); code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr %q)", name, code, errb.String())
		}
		if strings.Contains(out.String(), "profile ") {
			t.Errorf("%s: printed a profile on a usage error", name)
		}
	}
}
