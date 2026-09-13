// Package portability_test keeps the recorded corpora and recording scripts
// free of paths that only exist on the machine that recorded them.
package portability_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// machinePathRE matches paths that name one machine or one account: a real
// home directory, the fleet's workspace tree, claude's encoded form of a home
// directory (as it appears in its projects/ and scratchpad directory names),
// and macOS's per-user temp root. A neutral path such as /tmp/rebake-x does
// not match.
var machinePathRE = regexp.MustCompile(
	`/Users/[A-Za-z0-9._-]+/|/home/[A-Za-z0-9._-]+/|\.loom/workspaces|-Users-[A-Za-z0-9._]+-|-home-[A-Za-z0-9._]+-|/var/folders/`,
)

// historical lists recordings made before this check existed that carry a
// machine path and are kept anyway, each with the reason. Everything else
// under test/corpus and test/scripts must be clean. Re-recording one of these
// removes it from the list; adding to it needs the same kind of reason.
var historical = map[string]string{
	"claude-code/trust-dialog-unnumbered": "2.1.261 folder-trust capture under tmux (#37); the only recording of the dialog before any answer",
	"claude-code/trust-dialog-confirmed":  "2.1.261 folder-trust capture under tmux (#37); the only recording of the post-answer render",
	"codex/short-reply":                   "codex 0.142.2 bake (c604b34); replaced only by a codex rebake",
	"codex/approval-command":              "codex approval-dialog capture; replaced only by a codex rebake",
	"codex/approval-patch":                "codex approval-dialog capture; replaced only by a codex rebake",
	"pi/headless-json-simple.jsonl":       "pi headless JSON capture; its cwd field is the recording machine's",
	"pi/headless-json-toolcall.jsonl":     "pi headless JSON capture; its cwd field is the recording machine's",
	"permission-mode/claude-code":         "permission-mode footer captures (#46); the footer shows the recording cwd",
	"permission-mode/codex":               "permission-mode footer captures; the footer shows the recording cwd",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && fi.Mode().IsRegular() {
			if _, err := os.Stat(filepath.Join(dir, "test", "corpus")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no repository root with test/corpus above the package directory")
		}
		dir = parent
	}
}

// allowed reports whether rel (relative to test/corpus) is, or lies under, a
// historical entry.
func allowed(rel string) bool {
	for h := range historical {
		if rel == h || strings.HasPrefix(rel, h+"/") {
			return true
		}
	}
	return false
}

// TestCorpusAndScriptsCarryNoMachinePaths: a recording that embeds the path it
// was made in (claude's startup banner prints its cwd) cannot be reproduced on
// another machine without churning every byte, and publishes one operator's
// directory layout. Checked before re-recording, so the rebake is from a
// neutral directory and profile.
func TestCorpusAndScriptsCarryNoMachinePaths(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	for _, base := range []string{"test/corpus", "test/scripts"} {
		dir := filepath.Join(root, base)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(filepath.Join(root, "test", "corpus"), path)
			if base == "test/corpus" && allowed(filepath.ToSlash(rel)) {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if m := machinePathRE.Find(data); m != nil {
				r, _ := filepath.Rel(root, path)
				offenders = append(offenders, r+": "+string(m))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	sort.Strings(offenders)
	for _, o := range offenders {
		t.Errorf("machine path in %s", o)
	}
}

// TestHistoricalEntriesStillExist keeps the exception list honest: an entry
// whose recording is gone (or was re-recorded clean) must leave the list.
func TestHistoricalEntriesStillExist(t *testing.T) {
	root := repoRoot(t)
	for h, why := range historical {
		p := filepath.Join(root, "test", "corpus", filepath.FromSlash(h))
		if _, err := os.Stat(p); err != nil {
			t.Errorf("historical entry %q (%s) no longer exists: %v", h, why, err)
			continue
		}
		clean := true
		_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if data, rerr := os.ReadFile(path); rerr == nil && machinePathRE.Match(data) {
					clean = false
				}
			}
			return nil
		})
		if clean {
			t.Errorf("historical entry %q no longer carries a machine path; remove it from the list", h)
		}
	}
}
