// Package metrics provides comparison utilities for the screen emulator
// bake-off. The bench compares emulator-extracted screen text against
// hand-curated ground truth using a small set of stable metrics.
package metrics

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ansiCSI matches ANSI CSI escape sequences (cursor moves, SGR, etc.).
// Used only as a defensive fallback — emulator output should already be
// plain text.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// StripANSI removes ANSI escape sequences from s.
func StripANSI(s string) string {
	return ansiCSI.ReplaceAllString(s, "")
}

// Normalize collapses trailing whitespace per line, trims leading/trailing
// blank lines, and converts tabs to spaces. Used before comparison so
// emulator differences in padding don't dominate edit distance.
func Normalize(s string) string {
	s = StripANSI(s)
	s = strings.ReplaceAll(s, "\t", "    ")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \r")
	}
	// trim blank lines top/bottom
	start, end := 0, len(lines)
	for start < end && lines[start] == "" {
		start++
	}
	for end > start && lines[end-1] == "" {
		end--
	}
	return strings.Join(lines[start:end], "\n")
}

// ExactMatch reports whether two normalized strings are byte-identical.
func ExactMatch(a, b string) bool {
	return Normalize(a) == Normalize(b)
}

// Levenshtein returns the edit distance between two strings, counted in
// runes. Implementation is O(len(a)*len(b)) time and O(min) space.
func Levenshtein(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	// Make ar the shorter to reduce memory.
	if len(ar) > len(br) {
		ar, br = br, ar
	}
	prev := make([]int, len(ar)+1)
	curr := make([]int, len(ar)+1)
	for i := 0; i <= len(ar); i++ {
		prev[i] = i
	}
	for j := 1; j <= len(br); j++ {
		curr[0] = j
		for i := 1; i <= len(ar); i++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			ins := curr[i-1] + 1
			del := prev[i] + 1
			sub := prev[i-1] + cost
			m := ins
			if del < m {
				m = del
			}
			if sub < m {
				m = sub
			}
			curr[i] = m
		}
		prev, curr = curr, prev
	}
	return prev[len(ar)]
}

// NormalizedDistance returns Levenshtein(a,b) / max(runes(a), runes(b)),
// clamped to [0,1]. 0 means identical, 1 means fully different.
func NormalizedDistance(a, b string) float64 {
	a = Normalize(a)
	b = Normalize(b)
	d := Levenshtein(a, b)
	max := utf8.RuneCountInString(a)
	if r := utf8.RuneCountInString(b); r > max {
		max = r
	}
	if max == 0 {
		return 0
	}
	return float64(d) / float64(max)
}
