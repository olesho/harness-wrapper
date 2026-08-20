package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
)

// Fingerprint hashes the given files, IN THE ORDER GIVEN, out of fsys:
//
//	sha256( for each rel: rel-bytes || 0x00 || file bytes )
//
// The order matters and is part of the contract; Manifest.Validate requires a
// sorted, duplicate-free list, so the only legal order is the sorted one and a
// builder and a verifier cannot disagree.
//
// harness_version is not an input — see the package doc for why. Files present
// in fsys but absent from files are never opened (the allowlist rule). A file
// that is listed but missing is an error naming the path, never a skip: a
// deleted provisioned file must fail verification.
//
// An empty list is legal and yields the sha256 of the empty string.
func Fingerprint(fsys fs.FS, files []string) (string, error) {
	h := sha256.New()
	for _, rel := range files {
		if err := validRelPath(rel); err != nil {
			return "", fmt.Errorf("profile: fingerprint: %v", err)
		}
		data, err := fs.ReadFile(fsys, rel)
		if err != nil {
			return "", fmt.Errorf("profile: fingerprint: read %s: %w", rel, err)
		}
		// Writes to a hash.Hash never fail (documented on hash.Hash).
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0x00})
		_, _ = h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// shortSum is the sha256 of s in lowercase hex, truncated to its first 8
// characters. It is the suffix form used by the per-config-root keychain slot
// (see auth.go) and mirrors `printf '%s' "$dir" | shasum -a 256 | cut -c1-8`:
// no trailing newline is hashed.
func shortSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}
