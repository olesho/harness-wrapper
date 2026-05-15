// Package gemini holds the classifier patterns for Google's Gemini CLI
// (@google/gemini-cli).
//
// Patterns are seeded conservatively from observed Gemini API error
// surfaces ("RESOURCE_EXHAUSTED", quota / rate-limit phrasings) and the
// generic prompt shapes the Ink-based TUI uses for tool approval.
// They should be tightened once a recorded corpus is in place.
package gemini

import "github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"

// Patterns is the Gemini harness fingerprint set consumed by the
// wrapper's harness adapter.
var Patterns = detector.Patterns{
	Cost: []string{
		"quota exceeded",
		"resource has been exhausted",
		"resource_exhausted",
		"rate limit",
		"rate-limit",
		"rate limit exceeded",
		"usage limit",
		"you have exceeded",
		"free tier",
	},
	Retry: []string{
		"please try again",
		"transient error",
		"temporary failure",
		"network error",
		"upstream error",
		"deadline exceeded",
		"unavailable",
	},
	Prompt: []string{
		"(y/n)",
		"(y/n/a)",
		"(yes/no)",
		"continue?",
		"apply this change?",
		"do you want to continue?",
		"allow?",
	},
}
