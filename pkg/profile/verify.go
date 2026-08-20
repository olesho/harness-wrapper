package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// VerifyInput is everything Verify needs. The caller does the I/O: it opens the
// config root, reads and parses the manifest, and observes the harness version.
type VerifyInput struct {
	// FS is the config root itself, e.g. os.DirFS(configDir).
	FS fs.FS
	// Manifest is the parsed record found at ManifestName in that root.
	Manifest Manifest
	// HarnessVersion is what the binary reports NOW. "" means the caller could
	// not observe it and yields ErrVersionUnknown — never a pass.
	HarnessVersion string
}

// Verify checks a provisioned config root against its manifest.
//
// Order of checks, deliberately: shape first (so a malformed manifest is
// rejected before any file is opened), then the observed version's presence,
// then the fingerprint, then version drift. Fingerprint before drift because a
// tampered profile is the more serious finding of the two.
//
// Returns nil, or ErrManifestInvalid, ErrVersionUnknown, or a *MismatchError
// wrapping ErrFingerprintMismatch or ErrVersionDrift. Read errors from FS are
// returned as-is, wrapped by Fingerprint.
//
// "The manifest file is missing" is not a case here: the caller owns the read
// and the never-provisioned distinction that goes with it.
func Verify(in VerifyInput) error {
	if err := in.Manifest.Validate(); err != nil {
		return err
	}
	actualVersion := strings.TrimSpace(in.HarnessVersion)
	if actualVersion == "" {
		return ErrVersionUnknown
	}
	sum, err := Fingerprint(in.FS, in.Manifest.Files)
	if err != nil {
		return err
	}
	if sum != in.Manifest.Fingerprint {
		return &MismatchError{Kind: "fingerprint", Expected: in.Manifest.Fingerprint, Actual: sum}
	}
	// Exact equality of the trimmed strings: no semver leniency, because the
	// version line is an opaque harness fact ("2.1.235 (Claude Code)").
	if wantVersion := strings.TrimSpace(in.Manifest.HarnessVersion); actualVersion != wantVersion {
		return &MismatchError{Kind: "version", Expected: wantVersion, Actual: actualVersion}
	}
	return nil
}

// The sentinel errors of the contract. Callers match with errors.Is and render
// their own messages; this package never formats a user-facing sentence.
var (
	// ErrManifestInvalid means the manifest is malformed as a record: bad JSON,
	// a fingerprint that is not 64 lowercase hex characters, an unsorted or
	// duplicated file list, a path that escapes the config root, or an empty
	// harness_version. It says nothing about the files on disk.
	ErrManifestInvalid = errors.New("profile: manifest invalid")

	// ErrFingerprintMismatch means the provisioned files no longer hash to the
	// manifest's fingerprint. The repair is to re-provision.
	ErrFingerprintMismatch = errors.New("profile: fingerprint mismatch")

	// ErrVersionDrift means the harness binary reports a different version than
	// the one the profile was provisioned against. The repair is to bless the
	// upgrade (re-provision, re-stamping harness_version) — a different act
	// from repairing a fingerprint, which is why the two are separate errors.
	ErrVersionDrift = errors.New("profile: harness version drift")

	// ErrVersionUnknown means the caller could not observe the harness version
	// and passed "". It is never treated as a match: an unverifiable profile is
	// not a verified one.
	ErrVersionUnknown = errors.New("profile: harness version unknown")

	// ErrUnknownHarness means a Harness value outside Harnesses() was passed.
	ErrUnknownHarness = errors.New("profile: unknown harness")
)

// MismatchError carries both sides of a comparison so callers can render their
// own message without re-deriving either. Unwrap yields ErrFingerprintMismatch
// or ErrVersionDrift, so errors.Is against the sentinels keeps working.
type MismatchError struct {
	// Kind is "fingerprint" or "version".
	Kind string
	// Expected is what the manifest recorded.
	Expected string
	// Actual is what was observed now.
	Actual string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("profile: %s mismatch: manifest %q, actual %q", e.Kind, e.Expected, e.Actual)
}

// Unwrap maps the kind onto its sentinel. An unrecognised kind unwraps to nil
// rather than guessing.
func (e *MismatchError) Unwrap() error {
	switch e.Kind {
	case "fingerprint":
		return ErrFingerprintMismatch
	case "version":
		return ErrVersionDrift
	default:
		return nil
	}
}
