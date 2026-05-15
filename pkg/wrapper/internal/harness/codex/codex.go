// Package codex holds the classifier patterns for the OpenAI Codex CLI
// harness.
package codex

import "github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"

// Patterns is the Codex harness fingerprint set consumed by the
// wrapper's harness adapter.
var Patterns = detector.Patterns{
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
