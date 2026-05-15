// Package claude holds the classifier patterns for the Claude Code CLI
// harness. Patterns are intentionally conservative: false positives
// here turn an active run into a stuck-looking one.
package claude

import "github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"

// Patterns is the Claude harness fingerprint set consumed by the
// wrapper's harness adapter. Matching happens on stripped, lower-cased
// recent output (Cost/Retry) and on the trailing line of stripped
// output (Prompt).
var Patterns = detector.Patterns{
	Cost: []string{
		"you've hit your limit",
		"you have hit your limit",
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
