package versions

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllAndPinnedAgainstRepo(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, want := range []string{"codex", "claude-code", "gemini", "opencode", "pi"} {
		entry, ok := all[want]
		if !ok {
			t.Errorf("expected entry for %q in versions.json", want)
			continue
		}
		if entry.Binary == "" {
			t.Errorf("entry %q must declare a non-empty binary", want)
		}
	}
	if got, ok := Pinned("codex"); !ok || got == "" {
		t.Errorf("expected codex to be pinned, got %q ok=%v", got, ok)
	}
	if _, ok := Pinned("nonexistent"); ok {
		t.Error("expected Pinned to return false for unknown harness")
	}
	// Gemini is intentionally unpinned in the initial versions.json.
	if got, ok := Pinned("gemini"); ok {
		t.Errorf("expected gemini to be unpinned, got %q", got)
	}
	// OpenCode is likewise unpinned until a corpus pins its version.
	if got, ok := Pinned("opencode"); ok {
		t.Errorf("expected opencode to be unpinned, got %q", got)
	}
	// pi is likewise unpinned until a corpus pins its version.
	if got, ok := Pinned("pi"); ok {
		t.Errorf("expected pi to be unpinned, got %q", got)
	}
	// claude-code's harness key differs from its on-PATH binary name;
	// the Binary field is what discovery probes against.
	if all["claude-code"].Binary != "claude" {
		t.Errorf("claude-code binary should be %q, got %q", "claude", all["claude-code"].Binary)
	}
}

func TestReadFromRejectsEmptyPackage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.json")
	if err := os.WriteFile(path, []byte(`{"foo":{"package":"","binary":"foo","pinned":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrom(path); err == nil {
		t.Error("expected error for empty package")
	}
}

func TestReadFromRejectsEmptyBinary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.json")
	if err := os.WriteFile(path, []byte(`{"foo":{"package":"pkg","binary":"","pinned":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrom(path); err == nil {
		t.Error("expected error for empty binary")
	}
}

func TestReadFromRejectsVerifiedAtWithoutPinned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.json")
	if err := os.WriteFile(path, []byte(`{"foo":{"package":"pkg","binary":"foo","pinned":"","verified_at":"2026-05-15"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrom(path); err == nil {
		t.Error("expected error for verified_at without pinned")
	}
}

func TestReadFromMissingFile(t *testing.T) {
	if _, err := ReadFrom(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("expected error for missing file")
	}
}

func TestReadFromMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.json")
	if err := os.WriteFile(path, []byte(`not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrom(path); err == nil {
		t.Error("expected error for malformed JSON")
	}
}
