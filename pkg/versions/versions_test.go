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
	for _, want := range []string{"codex", "claude-code", "gemini"} {
		if _, ok := all[want]; !ok {
			t.Errorf("expected entry for %q in versions.json", want)
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
}

func TestReadFromRejectsEmptyPackage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.json")
	if err := os.WriteFile(path, []byte(`{"foo":{"package":"","pinned":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFrom(path); err == nil {
		t.Error("expected error for empty package")
	}
}

func TestReadFromRejectsVerifiedAtWithoutPinned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.json")
	if err := os.WriteFile(path, []byte(`{"foo":{"package":"pkg","pinned":"","verified_at":"2026-05-15"}}`), 0o644); err != nil {
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
