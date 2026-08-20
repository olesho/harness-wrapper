package profile

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
)

func goldenManifest(t *testing.T, version string) Manifest {
	t.Helper()
	m, err := BuildManifest(goldenTree(), []string{"a.txt", "b/c.txt"}, version)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	return m
}

func TestVerifyHappyPath(t *testing.T) {
	const version = "2.1.235 (Claude Code)"
	m := goldenManifest(t, version)
	if err := Verify(VerifyInput{FS: goldenTree(), Manifest: m, HarnessVersion: version}); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyIgnoresUnlistedChurn(t *testing.T) {
	const version = "2.1.235"
	m := goldenManifest(t, version)
	fsys := goldenTree()
	fsys[".credentials.json"] = &fstest.MapFile{Data: []byte("rotated by the harness")}
	fsys["sessions/live.jsonl"] = &fstest.MapFile{Data: []byte("churn")}
	if err := Verify(VerifyInput{FS: fsys, Manifest: m, HarnessVersion: version}); err != nil {
		t.Fatalf("runtime churn broke verification: %v", err)
	}
}

func TestVerifyFingerprintMismatchCarriesBothSides(t *testing.T) {
	const version = "2.1.235"
	m := goldenManifest(t, version)
	fsys := goldenTree()
	fsys["a.txt"] = &fstest.MapFile{Data: []byte("tampered")}

	err := Verify(VerifyInput{FS: fsys, Manifest: m, HarnessVersion: version})
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("err = %v, want ErrFingerprintMismatch", err)
	}
	var mm *MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("err %v is not a *MismatchError", err)
	}
	if mm.Kind != "fingerprint" {
		t.Errorf("Kind = %q, want %q", mm.Kind, "fingerprint")
	}
	if mm.Expected != m.Fingerprint || mm.Actual == "" || mm.Actual == mm.Expected {
		t.Errorf("mismatch does not carry both sides: %+v", mm)
	}
}

func TestVerifyVersionDrift(t *testing.T) {
	m := goldenManifest(t, "2.1.234 (Claude Code)")
	err := Verify(VerifyInput{FS: goldenTree(), Manifest: m, HarnessVersion: "2.1.235 (Claude Code)"})
	if !errors.Is(err, ErrVersionDrift) {
		t.Fatalf("err = %v, want ErrVersionDrift", err)
	}
	var mm *MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("err %v is not a *MismatchError", err)
	}
	if mm.Kind != "version" || mm.Expected != "2.1.234 (Claude Code)" || mm.Actual != "2.1.235 (Claude Code)" {
		t.Errorf("mismatch = %+v", mm)
	}
	// Drift is a different repair from a fingerprint mismatch; they must not
	// be confusable.
	if errors.Is(err, ErrFingerprintMismatch) {
		t.Error("version drift also matches ErrFingerprintMismatch")
	}
}

func TestVerifyVersionWhitespaceOnlyDifference(t *testing.T) {
	m := goldenManifest(t, "2.1.235 (Claude Code)")
	err := Verify(VerifyInput{FS: goldenTree(), Manifest: m, HarnessVersion: "  2.1.235 (Claude Code)\n"})
	if err != nil {
		t.Errorf("whitespace-only difference failed: %v", err)
	}
}

func TestVerifyUnknownVersion(t *testing.T) {
	m := goldenManifest(t, "2.1.235")
	for _, v := range []string{"", "   "} {
		if err := Verify(VerifyInput{FS: goldenTree(), Manifest: m, HarnessVersion: v}); !errors.Is(err, ErrVersionUnknown) {
			t.Errorf("HarnessVersion %q: err = %v, want ErrVersionUnknown", v, err)
		}
	}
}

// TestVerifyInvalidManifestReadsNothing: shape is checked before any file is
// opened, so a malformed manifest cannot be used to probe the filesystem.
func TestVerifyInvalidManifestReadsNothing(t *testing.T) {
	fsys := &countingFS{inner: goldenTree()}
	m := Manifest{Files: []string{"a.txt"}, Fingerprint: "nope", HarnessVersion: "2.1.235"}
	if err := Verify(VerifyInput{FS: fsys, Manifest: m, HarnessVersion: "2.1.235"}); !errors.Is(err, ErrManifestInvalid) {
		t.Fatalf("err = %v, want ErrManifestInvalid", err)
	}
	if fsys.opens != 0 {
		t.Errorf("Verify opened %d files before rejecting the manifest", fsys.opens)
	}
}

func TestVerifyMissingProvisionedFile(t *testing.T) {
	m := goldenManifest(t, "2.1.235")
	fsys := goldenTree()
	delete(fsys, "b/c.txt")
	if err := Verify(VerifyInput{FS: fsys, Manifest: m, HarnessVersion: "2.1.235"}); err == nil {
		t.Error("a deleted provisioned file verified successfully")
	}
}

func TestMismatchErrorUnwrapUnknownKind(t *testing.T) {
	e := &MismatchError{Kind: "something else"}
	if e.Unwrap() != nil {
		t.Error("an unrecognised kind guessed a sentinel")
	}
	if e.Error() == "" {
		t.Error("empty Error()")
	}
}

// countingFS counts Open calls so a test can assert nothing was read.
type countingFS struct {
	inner fs.FS
	opens int
}

func (c *countingFS) Open(name string) (fs.File, error) {
	c.opens++
	return c.inner.Open(name)
}
