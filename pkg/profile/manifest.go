package profile

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// ManifestName is the file a provisioned harness config root carries at its
// top level. It is part of the frozen wire contract.
const ManifestName = ".manifest.json"

// Manifest is the launch-verification record at <configRoot>/.manifest.json.
//
// The JSON field names and the fingerprint algorithm are the frozen wire
// contract: other implementations (a shell provisioner, a supervisor in another
// repo) read and write this exact shape.
type Manifest struct {
	// Files is the allowlist of provisioned files, relative to the config
	// root, slash-separated, sorted, duplicate-free. Files on disk that are
	// not listed here are never opened.
	Files []string `json:"files"`

	// Fingerprint is the lowercase sha256 hex (64 chars) produced by
	// Fingerprint over Files, in the listed order.
	Fingerprint string `json:"fingerprint"`

	// HarnessVersion is the trimmed first line of `<binary> --version` as
	// observed when the profile was provisioned. It is compared on its own and
	// is never an input to Fingerprint.
	HarnessVersion string `json:"harness_version"`
}

// ParseManifest decodes and validates manifest bytes. Reading the file is the
// caller's job: "the manifest is absent" is a lifecycle distinction this
// package deliberately does not own.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	// Unknown fields are tolerated on purpose: another implementation of this
	// contract may add a field before this one learns about it, and a manifest
	// that verifies must keep verifying.
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrManifestInvalid, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Marshal renders the manifest in the on-disk form: MarshalIndent with a
// single-space indent, plus a trailing newline. Byte-stable, so a provisioner
// that rewrites an unchanged profile produces an unchanged file.
func (m Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return nil, fmt.Errorf("profile: marshal manifest: %w", err)
	}
	return append(b, '\n'), nil
}

// Validate checks the manifest's shape. It touches no filesystem: a manifest
// can be rejected as malformed before any provisioned file is opened.
//
// The file list is attacker-adjacent input — it lives in a directory the agent
// itself can write — so path escapes are rejected explicitly rather than left
// to fs.FS.
func (m Manifest) Validate() error {
	if !isSHA256Hex(m.Fingerprint) {
		return fmt.Errorf("%w: fingerprint %q is not 64 lowercase hex characters",
			ErrManifestInvalid, m.Fingerprint)
	}
	if strings.TrimSpace(m.HarnessVersion) == "" {
		return fmt.Errorf("%w: harness_version is empty", ErrManifestInvalid)
	}
	for i, rel := range m.Files {
		if err := validRelPath(rel); err != nil {
			return fmt.Errorf("%w: files[%d]: %v", ErrManifestInvalid, i, err)
		}
		if i > 0 {
			switch prev := m.Files[i-1]; {
			case rel == prev:
				return fmt.Errorf("%w: files[%d]: duplicate entry %q",
					ErrManifestInvalid, i, rel)
			case rel < prev:
				return fmt.Errorf("%w: files[%d]: %q sorts before %q; the list must be sorted",
					ErrManifestInvalid, i, rel, prev)
			}
		}
	}
	return nil
}

// validRelPath rejects anything that is not a clean, relative, slash-separated
// path inside the config root.
func validRelPath(rel string) error {
	switch {
	case rel == "":
		return fmt.Errorf("empty path")
	case strings.ContainsRune(rel, '\\'):
		return fmt.Errorf("%q contains a backslash; paths are always slash-separated", rel)
	case strings.HasPrefix(rel, "/"):
		return fmt.Errorf("%q is absolute", rel)
	case path.Clean(rel) != rel:
		return fmt.Errorf("%q is not path.Clean-stable", rel)
	}
	for _, elem := range strings.Split(rel, "/") {
		if elem == ".." {
			return fmt.Errorf("%q escapes the config root", rel)
		}
	}
	return nil
}

// isSHA256Hex reports whether s is exactly 64 lowercase hex characters.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
