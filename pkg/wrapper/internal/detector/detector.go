// Package detector provides generic pattern primitives for harness
// classifiers. Patterns are matched against the recent harness output
// (typically the last ~64KB), with ANSI escapes already stripped.
//
// The package is intentionally minimal: it does not own state, it just
// runs string matches. Real classifiers compose these primitives with
// state from the wrapper (idle thresholds, quiet windows).
package detector

import "strings"

// Patterns groups the per-harness fingerprints a classifier consults.
// All slices are matched as case-insensitive substrings against
// already-stripped, lower-cased recent output, except Prompt which is
// matched against the trailing line of the original (case-preserved)
// stripped output.
type Patterns struct {
	// Cost matches messages indicating the harness has hit a budget,
	// quota, or rate limit and cannot proceed without operator
	// intervention.
	Cost []string

	// Retry matches messages indicating a transient failure that the
	// engine should retry after a backoff.
	Retry []string

	// Prompt matches trailing-line fingerprints (e.g. "(y/N)") that
	// indicate the harness is waiting for keyboard input.
	Prompt []string
}

// MatchAny returns the first pattern in patterns that appears as a
// substring of haystack, or "" if none match. Caller is expected to
// pre-lowercase haystack.
func MatchAny(haystack string, patterns []string) string {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.Contains(haystack, p) {
			return p
		}
	}
	return ""
}

// MatchPromptSuffix returns the first pattern in patterns that the
// trailing non-empty line of haystack ends with (case-insensitive),
// or "" if none match. Trailing whitespace on the last line is
// ignored so prompts ending with a space ("Continue? ") still match.
func MatchPromptSuffix(haystack string, patterns []string) string {
	tail := lastNonEmptyLine(haystack)
	if tail == "" {
		return ""
	}
	tailLower := strings.ToLower(strings.TrimRight(tail, " \t"))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.HasSuffix(tailLower, strings.ToLower(p)) {
			return p
		}
	}
	return ""
}

func lastNonEmptyLine(s string) string {
	end := len(s)
	for end > 0 {
		// Trim a trailing newline / space block.
		for end > 0 && (s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == ' ' || s[end-1] == '\t') {
			end--
		}
		start := end
		for start > 0 && s[start-1] != '\n' {
			start--
		}
		line := s[start:end]
		if line != "" {
			return line
		}
		if start == 0 {
			return ""
		}
		end = start - 1
	}
	return ""
}
