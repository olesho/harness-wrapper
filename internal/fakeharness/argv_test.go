package fakeharness_test

import (
	"os"
	"reflect"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// TestMain removes the once-built fake binary after all tests run. BuildOnce
// caches across tests, so cleanup must happen exactly once at the end — not per
// test, which would delete the binary out from under a later test.
func TestMain(m *testing.M) {
	code := m.Run()
	fakeharness.Cleanup()
	os.Exit(code)
}

// TestCapturedArgv builds and runs the fake with FAKEHARNESS_ARGV_OUT pointed at
// a temp file, then reads and JSON-unmarshals that file and asserts it equals the
// passed args as a single JSON array — the argv-prepending conformance check.
func TestCapturedArgv(t *testing.T) {
	bin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("cannot build fakeharness: %v", err)
	}

	args := []string{"--resume", "abc-123", "chat", "hello world"}
	got, err := fakeharness.CapturedArgv(bin, args...)
	if err != nil {
		t.Fatalf("CapturedArgv: %v", err)
	}
	if !reflect.DeepEqual(got, args) {
		t.Fatalf("argv dump = %#v, want %#v", got, args)
	}
}

// TestCapturedArgv_Empty covers the no-args case: the dump is a JSON array, and
// unmarshalling yields an empty (non-nil) slice.
func TestCapturedArgv_Empty(t *testing.T) {
	bin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("cannot build fakeharness: %v", err)
	}

	got, err := fakeharness.CapturedArgv(bin)
	if err != nil {
		t.Fatalf("CapturedArgv: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("argv dump = %#v, want empty", got)
	}
}
