// Package codex holds the classifier patterns for the OpenAI Codex CLI
// harness.
package codex

import (
	"regexp"
	"strings"

	"github.com/olesho/harness-wrapper/internal/wrapcore/detector"
)

// retryLimitRE captures the explicit "exceeded retry limit, last
// status: NNN" text Codex prints after exhausting its internal
// reqwest retries. This is the only Codex error surface that
// stringifies a numeric HTTP code in the user-visible output.
var retryLimitRE = regexp.MustCompile(`(?i)exceeded retry limit,\s*last status:\s*(\d{3})`)

// codexPhraseHits maps known Codex error-display phrases (from
// codex-rs/protocol/src/error.rs CodexErr Display impls and from
// chatwidget rate-limit handling) to inferred HTTP codes. The matcher
// scans for the first phrase that appears in the output. Phrases are
// lower-case ASCII: MatchAPIError finds them in asciiLower of the output.
var codexPhraseHits = []struct {
	Phrase string
	Code   int
}{
	{"selected model is at capacity", 503},
	{"currently experiencing high demand", 500},
	{"usage limit reached", 429},
	{"you're out of credits", 429},
	{"quota exceeded", 429},
	{"stream disconnected before completion", 0},
}

// MatchAPIError implements detector.APIErrorMatcher for Codex CLI.
// It checks the explicit retry-limit form first, then falls back to a
// phrase table. Codex's prefix glyph (■ in TUI, ERROR: in exec) is
// not required — matching on the inner phrase is sufficient and works
// across both display paths.
func MatchAPIError(stripped string) (detector.APIErrorHit, bool) {
	if m := retryLimitRE.FindStringSubmatch(stripped); m != nil {
		code := 0
		for _, r := range m[1] {
			code = code*10 + int(r-'0')
		}
		hit := detector.APIErrorHit{
			Code:    code,
			Message: "exceeded retry limit, last status: " + m[1],
		}
		hit.RetryAfter = detector.ParseRetryAfter(stripped)
		return hit, true
	}

	// An offset found in lower must be the same offset in stripped, so only
	// ASCII letters are folded. strings.ToLower does not keep offsets: it
	// changes the UTF-8 length of some runes (Ⱥ is two bytes and ⱥ three, İ is
	// two and i one), so every offset after such a rune named another place in
	// stripped — past its end, a panic that took the host process down, or
	// the wrong text.
	lower := asciiLower(stripped)
	for _, p := range codexPhraseHits {
		if idx := strings.Index(lower, p.Phrase); idx >= 0 {
			// Extract the matched phrase as it appears in the
			// case-preserved output for the Message field.
			msg := stripped[idx : idx+len(p.Phrase)]
			hit := detector.APIErrorHit{
				Code:    p.Code,
				Message: msg,
			}
			hit.RetryAfter = detector.ParseRetryAfter(stripped)
			return hit, true
		}
	}

	return detector.APIErrorHit{}, false
}

// asciiLower returns s with its ASCII letters lower-cased and every other
// byte as it was, so each byte of the result sits at its offset in s.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// Patterns is the Codex harness fingerprint set consumed by the
// wrapper's harness adapter.
var Patterns = detector.Patterns{
	APIError: MatchAPIError,
	Cost: []string{
		"rate limit exceeded",
		"quota exceeded",
		"usage limit",
		"insufficient_quota",
		"you've hit your limit",
	},
	Retry: []string{
		"please try again",
		"server error",
		"upstream timed out",
		"temporary failure",
	},
	Prompt: []string{
		"(y/n)",
		"(yes/no)",
		"continue?",
		"approve change?",
		"apply patch?",
	},
}
