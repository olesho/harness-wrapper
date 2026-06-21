package fakeharness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// Build helpers shared across test packages (pkg/chat, pkg/harness,
// cmd/harness-chatd). Kept free of a testing dependency so the library can be
// imported anywhere; test packages wrap BuildOnce with t.Skip/t.Cleanup.

var (
	buildOnce sync.Once
	builtDir  string
	builtPath string
	builtErr  error
)

// BuildOnce compiles cmd/fakeharness once per test process and returns the
// binary path. Subsequent calls return the cached result. The Go toolchain must
// be available; callers typically t.Skip on error.
func BuildOnce() (string, error) {
	buildOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			builtErr = fmt.Errorf("go toolchain unavailable: %w", err)
			return
		}
		builtDir, builtErr = os.MkdirTemp("", "fakeharness-bin")
		if builtErr != nil {
			return
		}
		builtPath = filepath.Join(builtDir, "fakeharness")
		cmd := exec.Command(goBin, "build", "-o", builtPath,
			"github.com/olesho/harness-wrapper/cmd/fakeharness")
		if out, err := cmd.CombinedOutput(); err != nil {
			builtErr = fmt.Errorf("build fakeharness: %w\n%s", err, out)
		}
	})
	return builtPath, builtErr
}

// Cleanup removes the binary BuildOnce produced. Call it from a package's
// TestMain after m.Run(); it is a no-op if nothing was built.
func Cleanup() {
	if builtDir != "" {
		_ = os.RemoveAll(builtDir)
	}
}
