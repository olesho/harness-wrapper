// Package pi holds the classifier patterns for the pi coding agent
// (@earendil-works/pi-coding-agent, binary "pi" —
// github.com/earendil-works/pi).
//
// pi is provider-agnostic: a single session can be backed by Anthropic,
// OpenAI, Google, local models, and others (its assistant messages carry
// the originating provider/model), so the error text it surfaces varies
// by provider. There is no single bracketed/anchored API-error format to
// key on the way Claude Code has, so APIError is left nil
// here and the Cost/Retry string lists carry the conservative
// cross-provider fingerprints instead. Patterns should be tightened (and
// an APIError matcher added) once a recorded corpus under
// test/corpus/pi/ exists.
package pi

import (
	"github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"
)

// Patterns is the pi harness fingerprint set consumed by the wrapper's
// harness adapter.
//
// APIError and SessionLimit are intentionally nil: see the package
// comment. Cost and Retry are seeded from the provider-neutral phrasings
// pi relays from upstream APIs; Prompt covers the generic confirmation
// shapes a tool-permission dialog uses.
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
