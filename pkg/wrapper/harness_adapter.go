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
				Reason:   formatSessionLimitReason(hit),
				Terminal: true,
				ResumeAt: hit.ResumeAt,
			}
		}
	}

	lower := strings.ToLower(stripped)

	if input.Idle {
		if hit := detector.MatchAny(lower, h.patterns.Cost); hit != "" {
			return Classification{
				Status:   StatusBlockedByCost,
				Reason:   hit,
				Terminal: true,
			}
		}
		if hit := detector.MatchAny(lower, h.patterns.Retry); hit != "" {
			return Classification{
				Status:   StatusRetryLater,
				Reason:   hit,
				Terminal: true,
			}
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
