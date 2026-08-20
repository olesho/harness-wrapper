package profile

import (
	"strings"
	"testing"
	"testing/fstest"
)

// goldenTree / goldenSum are THE wire-contract vector. The sum is
// sha256("a.txt" + 0x00 + "A" + "b/c.txt" + 0x00 + "C"), reproducible with:
//
//	python3 -c 'import hashlib;print(hashlib.sha256(b"a.txt\x00A"+b"b/c.txt\x00C").hexdigest())'
//
// If this test ever needs updating, the wire contract has been BROKEN and every
// provisioned profile and every other implementation of the manifest is
// invalidated. Change the code, not the constant.
const goldenSum = "27fa969635caf3dc34026424a3bfac5b066d7b20c8e96dcc2cfc991c0e4bd99b"

// emptySum is sha256 of the empty string: the fingerprint of a profile that
// provisions nothing, which is a valid, verifiable profile.
const emptySum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func goldenTree() fstest.MapFS {
	return fstest.MapFS{
		"a.txt":   {Data: []byte("A")},
		"b/c.txt": {Data: []byte("C")},
	}
}

func TestFingerprintGoldenVector(t *testing.T) {
	got, err := Fingerprint(goldenTree(), []string{"a.txt", "b/c.txt"})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != goldenSum {
		t.Errorf("fingerprint = %q, want %q (the wire contract changed)", got, goldenSum)
	}
}

func TestFingerprintEmptyList(t *testing.T) {
	got, err := Fingerprint(goldenTree(), nil)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != emptySum {
		t.Errorf("fingerprint of empty list = %q, want %q", got, emptySum)
	}
}

// TestFingerprintExcludesHarnessVersion is the regression test for the
// 2026-08-19 divergence: the provision script folded harness_version into the
// sha256 while the supervisor hashed only path+NUL+bytes, so the observer's
// first profiled run was refused for a mismatch that did not exist. The version
// is a separately-compared field and must never reach the hash.
func TestFingerprintExcludesHarnessVersion(t *testing.T) {
	fsys := goldenTree()
	files := []string{"a.txt", "b/c.txt"}

	a, err := BuildManifest(fsys, files, "2.1.234 (Claude Code)")
	if err != nil {
		t.Fatalf("BuildManifest a: %v", err)
	}
	b, err := BuildManifest(fsys, files, "2.1.235 (Claude Code)")
	if err != nil {
		t.Fatalf("BuildManifest b: %v", err)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("fingerprint changed with harness_version: %q vs %q", a.Fingerprint, b.Fingerprint)
	}
	if a.Fingerprint != goldenSum {
		t.Errorf("fingerprint = %q, want %q", a.Fingerprint, goldenSum)
	}
	if a.HarnessVersion == b.HarnessVersion {
		t.Errorf("harness_version not recorded distinctly: both %q", a.HarnessVersion)
	}
}

// TestFingerprintOrderMatters proves order is part of the contract, and that
// Validate admits only one order — so a builder and a verifier cannot disagree.
func TestFingerprintOrderMatters(t *testing.T) {
	fsys := goldenTree()
	forward, err := Fingerprint(fsys, []string{"a.txt", "b/c.txt"})
	if err != nil {
		t.Fatalf("Fingerprint forward: %v", err)
	}
	reverse, err := Fingerprint(fsys, []string{"b/c.txt", "a.txt"})
	if err != nil {
		t.Fatalf("Fingerprint reverse: %v", err)
	}
	if forward == reverse {
		t.Fatal("reversed file list hashed the same; order is not part of the hash")
	}
	m := Manifest{Files: []string{"b/c.txt", "a.txt"}, Fingerprint: reverse, HarnessVersion: "2.1.235"}
	if err := m.Validate(); err == nil {
		t.Error("Validate accepted an unsorted file list")
	}
}

func TestFingerprintMissingFileNamesPath(t *testing.T) {
	_, err := Fingerprint(goldenTree(), []string{"a.txt", "gone.txt"})
	if err == nil {
		t.Fatal("missing file was skipped instead of failing")
	}
	if !strings.Contains(err.Error(), "gone.txt") {
		t.Errorf("error %q does not name the missing path", err)
	}
}

func TestFingerprintIgnoresUnlistedFiles(t *testing.T) {
	fsys := goldenTree()
	fsys["runtime/sessions.jsonl"] = &fstest.MapFile{Data: []byte("churn")}
	got, err := Fingerprint(fsys, []string{"a.txt", "b/c.txt"})
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != goldenSum {
		t.Errorf("unlisted file affected the fingerprint: got %q", got)
	}
}

func TestFingerprintRejectsEscapingPath(t *testing.T) {
	if _, err := Fingerprint(goldenTree(), []string{"../secrets"}); err == nil {
		t.Error("Fingerprint accepted a path escaping the config root")
	}
}

func TestShortSumMatchesShellVector(t *testing.T) {
	// printf '%s' /tmp/p/claude | shasum -a 256 | cut -c1-8
	if got := shortSum("/tmp/p/claude"); got != "7629796b" {
		t.Errorf("shortSum = %q, want %q", got, "7629796b")
	}
}
