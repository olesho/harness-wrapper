package transcript

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveHarnessPath(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, workingDir, want string
	}{
		{"empty path stays empty", "", "/wt", ""},
		{"absolute path is untouched", "/agents/w3/claude", "/wt", "/agents/w3/claude"},
		{"relative path joins the child's cwd", ".agent/projects", "/wt", "/wt/.agent/projects"},
		{"dot segments are cleaned", "../shared/claude", "/wt/repo", "/wt/shared/claude"},
		{"empty workingDir means the inherited cwd", ".agent", "", filepath.Join(cwd, ".agent")},
		{"relative workingDir resolves like exec's Cmd.Dir", ".agent", "sub", filepath.Join(cwd, "sub", ".agent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveHarnessPath(tc.path, tc.workingDir); got != tc.want {
				t.Errorf("ResolveHarnessPath(%q, %q) = %q, want %q", tc.path, tc.workingDir, got, tc.want)
			}
		})
	}
}
