package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/screenbench/scenario"
)

// mockBin is the path to a freshly-built mock harness binary, set up
// by TestMain so integration tests don't pay the build cost per-test.
var mockBin string

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "screenbench-record-test-")
	if err != nil {
		os.Stderr.WriteString("setup: mkdtemp: " + err.Error() + "\n")
		os.Exit(1)
	}
	defer os.RemoveAll(tmpDir)
	mockBin = filepath.Join(tmpDir, "mock")
	cmd := exec.Command("go", "build", "-o", mockBin, "github.com/olesho/harness-wrapper/test/fakeharness/mock")
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Stderr.WriteString("setup: build mock: " + err.Error() + "\n" + string(out))
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// TestRun_ScriptedAgainstMockHarness drives the mock harness in
// needs-input mode via a scripted recording. After the run we verify:
//
//   - bytes.raw captures the prompt + the script's response;
//   - meta.json is written with the requested binary_version;
//   - the mock saw the script's input verbatim (it would exit non-zero
//     otherwise — we don't assert on the exit code because the script
//     forces shutdown via context cancellation).
func TestRun_ScriptedAgainstMockHarness(t *testing.T) {
	outDir := t.TempDir()
	scriptPath := filepath.Join(outDir, "script.json")
	if err := os.WriteFile(scriptPath, []byte(`{
		"steps": [
			{"wait_for": "Continue\\? \\[y/N\\] "},
			{"send": "y\n"},
			{"wait_for": "Approved\\. DONE"}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := recorderConfig{
		Harness:       "mock",
		Bin:           mockBin,
		Out:           outDir,
		Cols:          120,
		Rows:          40,
		BinaryVersion: "mock-test-1.0",
		Notes:         "scripted recording integration test",
		ScriptPath:    scriptPath,
		IdleTimeout:   500 * time.Millisecond,
		MaxDuration:   15 * time.Second,
		HarnessArgs:   []string{"--mode", "needs-input"},
		Stdout:        io.Discard,
	}

	if err := run(c); err != nil {
		t.Fatalf("run: %v", err)
	}

	// bytes.raw: should contain the prompt and the assistant's response.
	raw, err := os.ReadFile(filepath.Join(outDir, "bytes.raw"))
	if err != nil {
		t.Fatalf("read bytes.raw: %v", err)
	}
	if !strings.Contains(string(raw), "Continue? [y/N]") {
		t.Errorf("bytes.raw missing prompt; got first 256 bytes: %q", trunc(string(raw), 256))
	}
	if !strings.Contains(string(raw), "Approved. DONE") {
		t.Errorf("bytes.raw missing approval acknowledgement; got first 512 bytes: %q", trunc(string(raw), 512))
	}

	// meta.json: harness, binary_version, notes round-trip.
	metaBytes, err := os.ReadFile(filepath.Join(outDir, "meta.json"))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	var meta scenario.Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("parse meta.json: %v", err)
	}
	if meta.Harness != "mock" {
		t.Errorf("meta.Harness = %q, want %q", meta.Harness, "mock")
	}
	if meta.BinaryVersion != "mock-test-1.0" {
		t.Errorf("meta.BinaryVersion = %q, want %q", meta.BinaryVersion, "mock-test-1.0")
	}
	if meta.Notes != c.Notes {
		t.Errorf("meta.Notes mismatch: %q vs %q", meta.Notes, c.Notes)
	}
	if meta.Cols != 120 || meta.Rows != 40 {
		t.Errorf("meta dims wrong: cols=%d rows=%d", meta.Cols, meta.Rows)
	}
}

// TestRun_AutoVersionCapturesVersionString uses a tiny --version stub
// to verify --auto-version populates meta.binary_version.
func TestRun_AutoVersionCapturesVersionString(t *testing.T) {
	tmpDir := t.TempDir()
	// Build a tiny binary that, when invoked with `--version`, prints
	// a fake version and exits, but in normal mode behaves like the mock.
	versionedBin := filepath.Join(tmpDir, "versioned-mock")
	src := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(src, []byte(`package main
import (
	"fmt"
	"os"
)
func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("versioned-mock 9.9.9")
		return
	}
	// Default: print a banner and exit immediately so the recorder
	// completes without needing a script.
	fmt.Println("hello")
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", versionedBin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build versioned-mock: %v: %s", err, out)
	}

	outDir := t.TempDir()
	c := recorderConfig{
		Harness:     "versioned-mock",
		Bin:         versionedBin,
		Out:         outDir,
		Cols:        80,
		Rows:        24,
		AutoVersion: true,
		IdleTimeout: 300 * time.Millisecond,
		MaxDuration: 5 * time.Second,
		Stdout:      io.Discard,
	}
	if err := run(c); err != nil {
		t.Fatalf("run: %v", err)
	}

	metaBytes, err := os.ReadFile(filepath.Join(outDir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta scenario.Meta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.BinaryVersion != "versioned-mock 9.9.9" {
		t.Errorf("meta.BinaryVersion = %q, want %q", meta.BinaryVersion, "versioned-mock 9.9.9")
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
