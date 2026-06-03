// Package cursor holds the classifier patterns for the Cursor CLI harness.
// Patterns are intentionally conservative: false positives here turn an
// active run into a stuck-looking one.
//
// Coverage note: Cursor surfaces errors as free prose (often without an
// anchored "API Error: <code>" line like Claude's), and the wrapper checks
// the APIError matcher un-gated on every live poll. An un-anchored prose
// matcher would risk false-positive mid-run classifications, so this pack
// ships only the idle-gated Cost/Retry/Prompt fingerprints for now. A
// line-anchored APIError matcher that maps Cursor's auth (401) /
// model-not-found (404) prose to ErrAuth/ErrModelNotFound is a deliberate
// follow-up, to be validated against real Cursor output samples; until then
// loom's residual classifier covers those cases.
package cursor

import "github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"

// Patterns is the Cursor harness fingerprint set. Cost/Retry are matched
// as substrings against stripped, lower-cased recent output (idle-gated);
// Prompt matches the trailing line.
var Patterns = detector.Patterns{
	Cost: []string{
		"rate limit",
		"rate-limit",
		"too many requests",
		"usage limit",
		"session limit",
		"quota exceeded",
		"limit resets",
		"resets at",
	},
	Retry: []string{
		"please try again",
		"temporary failure",
		"service unavailable",
		"internal server error",
		"overloaded",
		"timed out",
		"timeout",
		"etimedout",
		"econnreset",
	},
	Prompt: []string{
		"(y/n)",
		"(yes/no)",
		"[y/n]",
		"continue?",
		"proceed?",
	},
}
