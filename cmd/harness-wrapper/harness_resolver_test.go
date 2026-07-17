package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveHarness_UnknownName(t *testing.T) {
	_, err := resolveHarness("not-a-real-harness")
	if err == nil {
		t.Fatal("expected error for unknown harness, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported harness") {
		t.Errorf("error %q should mention 'unsupported harness'", err)
	}
	for _, want := range []string{"codex", "claude", "opencode", "pi"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should list supported name %q", err, want)
		}
	}
}

func TestResolveHarness_KnownButNotInPath(t *testing.T) {
	t.Setenv("PATH", "/nonexistent-harness-wrapper-test-path")

	_, err := resolveHarness("codex")
	if err == nil {
		t.Fatal("expected error when binary missing from PATH, got nil")
	}
	if !strings.Contains(err.Error(), "not found in PATH") {
		t.Errorf("error %q should mention 'not found in PATH'", err)
	}
}

func TestResolveHarness_KnownInPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only test setup")
	}
	tmpDir := t.TempDir()
	codexPath := filepath.Join(tmpDir, "codex")
	if err := os.WriteFile(codexPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("PATH", tmpDir)

	got, err := resolveHarness("codex")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != codexPath {
		t.Errorf("got %q, want %q", got, codexPath)
	}
}

func TestSupportedHarnessNamesIsStable(t *testing.T) {
	got := supportedHarnessNames()
	if got != "claude, codex, opencode, pi" {
		t.Errorf("supportedHarnessNames() = %q, want sorted %q", got, "claude, codex, opencode, pi")
	}
}
