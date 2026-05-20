// Package claude holds the classifier patterns for the Claude Code CLI
// harness. Patterns are intentionally conservative: false positives
// here turn an active run into a stuck-looking one.
package claude

import (
	"regexp"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"
)

// apiErrorRE matches Claude Code's two API-error rendering shapes:
//
//   - HTTP errors with a 3-digit code: "API Error: 529 Overloaded..."
//   - Transport errors without a code: "API Error: The socket
//     connection was closed unexpectedly..."
//
// Both may appear with an optional leading tree-character decoration
// like "⎿  " (claudecode's tool-result drawing). The line-start anchor
// (with allowed leading whitespace + one optional decoration glyph)
// keeps the matcher from firing on in-prose mentions like "what does
// API Error: 500 mean?".
var apiErrorRE = regexp.MustCompile(`(?im)^[^\S\r\n]*(?:[⎿│├└╰─◯⏺]\s*)?API Error:\s*(?:(\d{3})\b\s+)?(.*)$`)

// MatchAPIError implements detector.APIErrorMatcher for Claude Code.
// On match, returns the parsed HTTP code (zero for the transport-error
// variant) and the trailing message with any whitespace trimmed.
//
// If the line starts with digits that aren't a valid 3-digit HTTP code
// (e.g. "API Error: 9999 unrecognized"), the match is rejected — that
// shape is almost certainly noise rather than a real upstream error.
func MatchAPIError(stripped string) (detector.APIErrorHit, bool) {
	m := apiErrorRE.FindStringSubmatch(stripped)
	if m == nil {
		return detector.APIErrorHit{}, false
	}
	hit := detector.APIErrorHit{
		Message: strings.TrimSpace(m[2]),
	}
	if m[1] != "" {
		// regex \d{3} guarantees Atoi succeeds.
		code := 0
		for _, r := range m[1] {
			code = code*10 + int(r-'0')
		}
		hit.Code = code
	} else if len(hit.Message) > 0 && hit.Message[0] >= '0' && hit.Message[0] <= '9' {
		// Looks like a malformed numeric code, not a real transport
		// error. Reject rather than misclassify.
		return detector.APIErrorHit{}, false
	}
	hit.RetryAfter = detector.ParseRetryAfter(hit.Message)
	return hit, true
}

// sessionLimitRE matches Claude Code's session-limit banner, of the
// shape "You've hit your session limit · resets 6:40pm (Europe/Warsaw)".
// The banner is typically wrapped in a tool-result decoration glyph
// (⎿) so the matcher tolerates a leading whitespace + decoration
// prefix on the same anchored line. The trailing "resets …" group is
// not captured here — ParseResetTime is run against the matched line
// to extract the absolute reset time.
var sessionLimitRE = regexp.MustCompile(`(?im)^[^\S\r\n]*(?:[⎿│├└╰─◯⏺]\s*)?(You(?:'ve|\s+have)\s+hit\s+your\s+(?:session|usage)\s+limit.*)$`)

// MatchSessionLimit implements detector.SessionLimitMatcher for Claude
// Code. On match, returns the matched banner line and the absolute
// reset time parsed from it. ResumeAt is zero when the banner did not
// embed a parseable clock-time (in that case the caller still treats
// the run as blocked_by_cost, just without a scheduled wakeup).
func MatchSessionLimit(stripped string, now time.Time) (detector.SessionLimitHit, bool) {
	m := sessionLimitRE.FindStringSubmatch(stripped)
	if m == nil {
		return detector.SessionLimitHit{}, false
	}
	hit := detector.SessionLimitHit{Message: strings.TrimSpace(m[1])}
	if resumeAt, ok := detector.ParseResetTime(hit.Message, now); ok {
		hit.ResumeAt = resumeAt
	}
	return hit, true
}

// Patterns is the Claude harness fingerprint set consumed by the
// wrapper's harness adapter. Matching happens on stripped, lower-cased
// recent output (Cost/Retry) and on the trailing line of stripped
// output (Prompt). APIError is a regex matcher for "API Error: ..."
// lines; see MatchAPIError. SessionLimit is a regex matcher for the
// "You've hit your session limit · resets …" banner; see
// MatchSessionLimit.
var Patterns = detector.Patterns{
	APIError:     MatchAPIError,
	SessionLimit: MatchSessionLimit,
	Cost: []string{
		"you've hit your limit",
		"you have hit your limit",
		"you've hit your session limit",
		"you have hit your session limit",
		"you've hit your usage limit",
		"you have hit your usage limit",
		"limit resets",
		"resets at",
		"usage limit",
		"rate limit",
		"rate-limit",
		"quota exceeded",
	},
	Retry: []string{
		"please try again",
		"transient error",
		"temporary failure",
		"network error",
		"upstream error",
	},
	Prompt: []string{
		"(y/n)",
		"(y/n/a)",
		"(yes/no)",
		"continue?",
		"continue? [y/n]",
		"approve?",
		"do you want to continue?",
	},
}
