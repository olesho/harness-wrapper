// Package opencode holds the classifier patterns for the OpenCode CLI
// (opencode-ai, binary "opencode" — github.com/sst/opencode).
//
// OpenCode is provider-agnostic: a single session can be backed by
// Anthropic, OpenAI, Google, local models, and dozens of others, so the
// error text it surfaces varies by provider. There is no single
// bracketed/anchored API-error format to key on the way Gemini and
// Claude Code have, so APIError is left nil here and the Cost/Retry
// string lists carry the conservative cross-provider fingerprints
// instead. Patterns should be tightened (and an APIError matcher added)
// once a recorded corpus under test/corpus/opencode/ exists.
package opencode

import (
	"github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"
)

// Patterns is the OpenCode harness fingerprint set consumed by the
// wrapper's harness adapter.
//
// APIError is intentionally nil: see the package comment. Cost and Retry
// are seeded from the provider-neutral phrasings OpenCode relays from
// upstream APIs; Prompt covers the generic confirmation shapes its TUI
// tool-permission dialog uses.
var Patterns = detector.Patterns{
	Cost: []string{
		"rate limit",
		"rate-limit",
		"rate limit exceeded",
		"quota exceeded",
		"insufficient_quota",
		"insufficient quota",
		"usage limit",
		"you have exceeded",
		"credit balance is too low",
		"billing",
		"resource_exhausted",
		"resource has been exhausted",
	},
	Retry: []string{
		"please try again",
		"try again later",
		"overloaded",
		"transient error",
		"temporary failure",
		"network error",
		"connection error",
		"upstream error",
		"deadline exceeded",
		"service unavailable",
		"unavailable",
	},
	Prompt: []string{
		"(y/n)",
		"(y/n/a)",
		"(yes/no)",
		"continue?",
		"allow?",
		"do you want to continue?",
		"do you want to proceed?",
		"approve?",
	},
}
