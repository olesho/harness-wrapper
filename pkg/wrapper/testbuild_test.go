package wrapper_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mockHarnessBin is the path to a freshly-built mock harness binary.
// It is set up by TestMain and reused across all tests in this package.
var mockHarnessBin string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	tmpDir, err := os.MkdirTemp("", "wrapper-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	mockHarnessBin = filepath.Join(tmpDir, "mock")
	cmd := exec.Command("go", "build", "-o", mockHarnessBin, "github.com/olesho/harness-wrapper/test/fakeharness/mock")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build mock harness: %v\n%s", err, out)
		return 1
	}

	return m.Run()
}
