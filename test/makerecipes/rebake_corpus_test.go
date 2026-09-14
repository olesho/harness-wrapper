// Package makerecipes_test pins contracts of recipes in the repository Makefile
// that no Go test can reach any other way: the defect lives in the shell the
// recipe runs, so the recipe itself is what the tests execute.
package makerecipes_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the package directory to the nearest Makefile.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		// A regular file: on a case-insensitive filesystem a directory named
		// "makefile" would otherwise answer for "Makefile".
		if fi, err := os.Stat(filepath.Join(dir, "Makefile")); err == nil && fi.Mode().IsRegular() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no Makefile found in any parent of the package directory")
		}
		dir = parent
	}
}

// fakeRecorder writes a stand-in for screenbench-record that records its argv,
// NUL-separated so an argument containing spaces stays one entry, to the file
// named by $HW_FAKE_RECORDER_ARGV, and does nothing else.
func fakeRecorder(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-recorder")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done > \"$HW_FAKE_RECORDER_ARGV\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// runRebake runs `make rebake-corpus` for an existing claude scenario with a
// fake recorder and a fake `claude` on PATH (the recipe only resolves it with
// `command -v`), and returns the argv the recorder was invoked with.
func runRebake(t *testing.T, extra ...string) []string {
	t.Helper()
	if testing.Short() {
		t.Skip("shells out to make; skipped under -short")
	}
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not on PATH")
	}
	claudeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(claudeDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	argvFile := filepath.Join(t.TempDir(), "argv")
	args := append([]string{
		"-C", repoRoot(t), "rebake-corpus",
		"HARNESS=claude", "SCENARIO=settled-after-turn", "RECORDER=" + fakeRecorder(t),
	}, extra...)
	cmd := exec.Command("make", args...)
	cmd.Env = append(os.Environ(),
		"PATH="+claudeDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HW_FAKE_RECORDER_ARGV="+argvFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make rebake-corpus: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("the recorder was never invoked: %v", err)
	}
	return strings.Split(string(bytes.TrimSuffix(raw, []byte{0})), "\x00")
}

// TestRebakeCorpusPassesWorkdirAsOneArgument: a WORKDIR with spaces must reach
// the recorder as exactly one --workdir argument. The recipe used to build the
// flag as a string and expand it unquoted, so "/tmp/work dir" arrived as
// "--workdir" "/tmp/work" "dir" and the recording ran somewhere else.
func TestRebakeCorpusPassesWorkdirAsOneArgument(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "work dir with spaces")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	argv := runRebake(t, "WORKDIR="+workdir)

	var at []int
	for i, a := range argv {
		if a == "--workdir" {
			at = append(at, i)
		}
	}
	if len(at) != 1 || at[0]+1 >= len(argv) {
		t.Fatalf("recorder argv has --workdir at %v; want exactly one, followed by the directory\nargv: %q", at, argv)
	}
	if got := argv[at[0]+1]; got != workdir {
		t.Errorf("--workdir %q, want %q\nargv: %q", got, workdir, argv)
	}
	for _, a := range argv {
		if a == "dir" || a == "with" || a == "spaces" {
			t.Errorf("a fragment of WORKDIR arrived as its own argument %q\nargv: %q", a, argv)
		}
	}
}

// TestRebakeCorpusOmitsWorkdirWhenUnset: without WORKDIR the recorder runs in
// its own directory, exactly as before the flag existed.
func TestRebakeCorpusOmitsWorkdirWhenUnset(t *testing.T) {
	argv := runRebake(t)
	for _, a := range argv {
		if a == "--workdir" {
			t.Fatalf("--workdir passed without WORKDIR\nargv: %q", argv)
		}
	}
	if len(argv) == 0 || !strings.Contains(strings.Join(argv, " "), "--script") {
		t.Fatalf("recorder argv looks wrong: %q", argv)
	}
}
