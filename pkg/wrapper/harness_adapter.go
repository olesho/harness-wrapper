package wrapper

import (
	"fmt"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"
)

// harnessAdapter turns a per-harness pattern set into a Classifier.
// Pattern matching always runs on stripped output so ANSI escapes do
// not interfere.
type harnessAdapter struct {
	patterns detector.Patterns
}

// transportRetryPatterns are provider-independent transport/network
// failures that warrant a respawn-after-backoff. They are matched
// case-insensitively as substrings against stripped output and are shared
// by every classifier — the per-harness adapters here and the generic
// defaultClassifier — so connection-refused-style errors are recognized
// once, regardless of which harness produced them.
var transportRetryPatterns = []string{
	"connection refused",
	"econnrefused",
	"connection reset",
	"econnreset",
	"no route to host",
	"ehostunreach",
	"network is unreachable",
	"fetch failed",
	"socket hang up",
	"eai_again",
}

// matchTransportRetry reports a StatusRetryLater classification when lower
// (already lowercased, ANSI-stripped output) contains a transport-failure
// fingerprint. Terminal: the wrapper should respawn the harness after a
// backoff. The bool is false when nothing matched.
func matchTransportRetry(lower string) (Classification, bool) {
	if hit := detector.MatchAny(lower, transportRetryPatterns); hit != "" {
		return Classification{
			Status:   StatusRetryLater,
			Class:    retryClass(hit),
			Reason:   hit,
			Terminal: true,
		}, true
	}
	return Classification{}, false
}

// Classify checks recent output against the harness's patterns.
//
// Order of checks:
//  1. APIError — fires regardless of idle/quiet state because high-
//     confidence anchored matchers don't need a quiescence gate. Sets
//     StatusAPIError (non-terminal: harness keeps running).
//  2. SessionLimit — also unidled. The banner is anchored on the
//     decoration glyph + exact "hit your … limit" phrase, which is
//     specific enough that a false positive is extremely unlikely.
//     Terminal: wrapper SIGTERMs the harness; ResumeAt carries the
//     parsed reset time.
//  3. Cost / Retry — gated on Idle. Terminal: wrapper SIGTERMs harness.
//  4. Prompt — gated on Quiet. Non-terminal: harness stays at prompt.
func (h harnessAdapter) Classify(input ClassifierInput) Classification {
	stripped := stripANSIEscapes(input.RecentOutput)

	if h.patterns.APIError != nil {
		if hit, ok := h.patterns.APIError(stripped); ok {
			return Classification{
				Status:     StatusAPIError,
				Class:      classFromHTTPCode(hit.Code),
				Reason:     formatAPIErrorReason(hit),
				Terminal:   false,
				HTTPCode:   hit.Code,
				RetryAfter: hit.RetryAfter,
			}
		}
	}

	if h.patterns.SessionLimit != nil {
		if hit, ok := h.patterns.SessionLimit(stripped, time.Now()); ok {
			return Classification{
				Status:   StatusBlockedByCost,
				Class:    ErrRateLimited, // a usage/session limit resets — transient, not a billing failure
				Reason:   formatSessionLimitReason(hit),
				Terminal: true,
				ResumeAt: hit.ResumeAt,
			}
		}
	}

	lower := strings.ToLower(stripped)

	if input.Idle {
		if c, ok := h.classifyIdle(lower); ok {
			return c
		}
	}

	if input.Quiet {
		if hit := detector.MatchPromptSuffix(stripped, h.patterns.Prompt); hit != "" {
			return Classification{
				Status:   StatusWaitingForInput,
				Reason:   "prompt detected: " + hit,
				Terminal: false,
			}
		}
	}

	return Classification{}
}

// classifyIdle runs the idle-gated Cost / Retry / transport-retry
// matchers against already-lowercased stripped output. The bool is false
// when none matched.
func (h harnessAdapter) classifyIdle(lower string) (Classification, bool) {
	if hit := detector.MatchAny(lower, h.patterns.Cost); hit != "" {
		return Classification{
			Status:   StatusBlockedByCost,
			Class:    costClass(hit),
			Reason:   hit,
			Terminal: true,
		}, true
	}
	if hit := detector.MatchAny(lower, h.patterns.Retry); hit != "" {
		return Classification{
			Status:   StatusRetryLater,
			Class:    retryClass(hit),
			Reason:   hit,
			Terminal: true,
		}, true
	}
	if c, ok := matchTransportRetry(lower); ok {
		return c, true
	}
	return Classification{}, false
}

func formatAPIErrorReason(hit detector.APIErrorHit) string {
	if hit.Code == 0 {
		return "api error: " + hit.Message
	}
	return fmt.Sprintf("api error %d: %s", hit.Code, hit.Message)
}

func formatSessionLimitReason(hit detector.SessionLimitHit) string {
	if hit.ResumeAt.IsZero() {
		return "session limit reached: " + hit.Message
	}
	return fmt.Sprintf("session limit reached, resumes at %s: %s",
		hit.ResumeAt.Format(time.RFC3339), hit.Message)
}
