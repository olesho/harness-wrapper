package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// A Session's own working directory is a directory beneath the workspace,
// reached through no symbolic link: what the harness would run in, and keep
// its record by, is what the Session names.
func TestCheckWorkDir(t *testing.T) {
	ws := t.TempDir()
	l := contract.Layout{Workspace: ws}
	for _, d := range []string{"wt/a", "elsewhere"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "wt", "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "elsewhere"), filepath.Join(ws, "wt", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "wt"), filepath.Join(ws, "via")); err != nil {
		t.Fatal(err)
	}
	if err := checkWorkDir(l, "wt/a"); err != nil {
		t.Errorf("wt/a: %v", err)
	}
	for dir, want := range map[string]string{
		"wt/missing": "no such file", "wt/file": "not a directory", "wt/link": "symbolic link", "via/a": "symbolic link",
	} {
		if err := checkWorkDir(l, dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", dir, err, want)
		}
	}
	if got := workDirOf(l, ""); got != "" {
		t.Errorf("workDirOf(\"\") = %q, want the open_config's", got)
	}
	if got := workDirOf(l, "wt/a"); got != filepath.Join(ws, "wt", "a") {
		t.Errorf("workDirOf(wt/a) = %q", got)
	}
	if WorkingDir("/w", "") != "/w" || WorkingDir("/w", "/w/wt/a") != "/w/wt/a" {
		t.Error("WorkingDir prefers the Session's own directory")
	}
}
