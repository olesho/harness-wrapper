package profile

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// semverRe matches the first X.Y.Z substring, optionally carrying a
// pre-release or build-metadata suffix. Shape copied from
// pkg/discovery/probes.go so both read the same noisy `--version` output the
// same way; it is duplicated rather than imported because pkg/discovery shells
// out and this package must not.
var semverRe = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][\w.]+)?`)

// VersionAtLeast reports whether the leading X.Y.Z of version is greater than
// or equal to the leading X.Y.Z of min.
//
// Both sides may be noisy: "2.1.235 (Claude Code)" compares as 2.1.235. Numeric
// comparison, component by component — a string compare would put "10.0.0"
// before "9.0.0", which is precisely the bug this exists to avoid. Pre-release
// and build suffixes are IGNORED: "2.1.234-rc.1" compares equal to "2.1.234".
//
// A string with no X.Y.Z anywhere in it is an error. Callers gating a
// behaviour on a version must decide what an unparseable version means; this
// package refuses to guess.
func VersionAtLeast(version, min string) (bool, error) {
	got, err := parseSemver(version)
	if err != nil {
		return false, err
	}
	want, err := parseSemver(min)
	if err != nil {
		return false, err
	}
	for i := range got {
		if got[i] != want[i] {
			return got[i] > want[i], nil
		}
	}
	return true, nil
}

// parseSemver extracts the first X.Y.Z from s as three numbers, discarding any
// pre-release or build suffix.
func parseSemver(s string) ([3]int, error) {
	var out [3]int
	m := semverRe.FindString(s)
	if m == "" {
		return out, fmt.Errorf("profile: no X.Y.Z version in %q", s)
	}
	// Trim any -pre.release / +build suffix; the regex guarantees the three
	// numeric components come first.
	if i := strings.IndexAny(m, "-+"); i >= 0 {
		m = m[:i]
	}
	for i, part := range strings.SplitN(m, ".", 3) {
		n, err := strconv.Atoi(part)
		if err != nil {
			return [3]int{}, fmt.Errorf("profile: parse version %q: %w", s, err)
		}
		out[i] = n
	}
	return out, nil
}
