package harness_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

func TestRenderHookCommandStructure(t *testing.T) {
	cmd := harness.RenderHookCommand([]string{"/abs/loom", "hooks"}, "claude", "stop", "loom")
	for _, want := range []string{
		"sh -c ",
		`test -n "$HW_EVENT_SPOOL" || exit 0`,
		"exec ",
		"/abs/loom",
		"hooks",
		"claude",
		"stop",
		"# harness-wrapper-hook:loom",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("rendered command missing %q:\n%s", want, cmd)
		}
	}
	if !harness.IsManagedHookCommand(cmd) {
		t.Error("IsManagedHookCommand should recognize a freshly rendered command")
	}
	if harness.IsManagedHookCommand(`sh -c 'echo user hook'`) {
		t.Error("a user hook must NOT be recognized as loom-managed")
	}
}

// TestRenderHookCommandBehavior runs the rendered command through a shell to
// prove (a) the env guard makes it INERT without HW_EVENT_SPOOL, (b) it execs
// loom with the right args WITH the spool set, and (c) a loom path containing a
// space survives the quoting.
func TestRenderHookCommandBehavior(t *testing.T) {
	for _, dirName := range []string{"loomdir", "loom dir with space"} {
		t.Run(dirName, func(t *testing.T) {
			checkRenderedHookBehavior(t, dirName)
		})
	}
}

// checkRenderedHookBehavior runs a rendered hook command through a shell and
// asserts the env guard, exec target, and quoting all behave.
func checkRenderedHookBehavior(t *testing.T, dirName string) {
	base := filepath.Join(t.TempDir(), dirName)
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(base, "invoked.txt")
	loom := filepath.Join(base, "loom")
	// Fake loom: record its args so we can assert exec reached it correctly.
	script := "#!/bin/sh\nprintf '%s' \"$*\" > " + posixQ(out) + "\n"
	if err := os.WriteFile(loom, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture executable
		t.Fatal(err)
	}
	cmd := harness.RenderHookCommand([]string{loom, "hooks"}, "claude", "stop", "loom")

	// (a) No spool → guard exits 0, loom NOT invoked.
	runSh(t, cmd, nil)
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("guard failed: loom was invoked without HW_EVENT_SPOOL")
	}
	// (b)+(c) With spool → loom invoked with "hooks claude stop".
	runSh(t, cmd, []string{"HW_EVENT_SPOOL=/tmp/whatever"})
	got, err := os.ReadFile(out) //nolint:gosec // test temp path
	if err != nil {
		t.Fatalf("loom was not invoked with HW_EVENT_SPOOL set: %v", err)
	}
	if string(got) != "hooks claude stop" {
		t.Errorf("loom args = %q, want %q", got, "hooks claude stop")
	}
}

func runSh(t *testing.T, command string, extraEnv []string) {
	t.Helper()
	c := exec.Command("sh", "-c", command)
	// Start from a CLEAN env so a real HW_EVENT_SPOOL in the test runner can't
	// leak in and defeat the no-spool case.
	c.Env = append([]string{"PATH=/usr/bin:/bin"}, extraEnv...)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("sh -c failed: %v\n%s", err, out)
	}
}

// posixQ mirrors the package's single-quote escaping for use inside the fake
// loom script (which itself is run by sh).
func posixQ(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func TestWithLockedFileSerializesConcurrentWriters(t *testing.T) {
	// N goroutines each read-modify-write the same file under the lock; without
	// serialization, lost updates would make the final count < N.
	target := filepath.Join(t.TempDir(), "counter")
	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := harness.WithLockedFile(target, func(existing []byte) ([]byte, error) {
				cur := 0
				if len(existing) > 0 {
					cur, _ = strconv.Atoi(strings.TrimSpace(string(existing)))
				}
				return []byte(strconv.Itoa(cur + 1)), nil
			})
			if err != nil {
				t.Errorf("WithLockedFile: %v", err)
			}
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(target) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := strconv.Atoi(strings.TrimSpace(string(data))); got != n {
		t.Fatalf("counter = %d, want %d (lost updates ⇒ lock not serializing)", got, n)
	}
}

func TestWithLockedFileNoChange(t *testing.T) {
	// Returning (nil, nil) writes nothing and leaves no file behind.
	target := filepath.Join(t.TempDir(), "untouched")
	if err := harness.WithLockedFile(target, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("no-change should not create the target file")
	}
}
