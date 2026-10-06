package wrapcore

import (
	"fmt"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/internal/wrapcore/detector"
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
//  1. SessionLimit — unidled. The banner is anchored on the decoration
//     glyph + exact "hit your … limit" phrase, which is specific enough
//     that a false positive is extremely unlikely. Terminal: wrapper
//     SIGTERMs the harness; ResumeAt carries the parsed reset time. It
//     outranks APIError so an older API error still in the output window
//     cannot hide it.
//  2. Cost — gated on Idle. Terminal. It yields to an APIError only when
//     that error appears after the last cost phrase (the newer signal
//     wins); otherwise a quota message that both matchers recognise (e.g.
//     codex "usage limit reached") would stay a non-terminal api_error
//     forever and the run would never stop.
//  3. APIError — fires regardless of idle/quiet state because high-
//     confidence anchored matchers don't need a quiescence gate. Sets
//     StatusAPIError (non-terminal: harness keeps running).
//  4. Retry / transport retry — gated on Idle. Terminal: wrapper
//     SIGTERMs harness.
//  5. Prompt — gated on Quiet. Non-terminal: harness stays at prompt.
func (h harnessAdapter) Classify(input ClassifierInput) Classification {
	stripped := stripANSIEscapes(input.RecentOutput)

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
		if hit := detector.MatchAny(lower, h.patterns.Cost); hit != "" && !h.apiErrorAfter(stripped, hit) {
			return Classification{
				Status:   StatusBlockedByCost,
				Class:    costClass(hit),
				Reason:   hit,
				Terminal: true,
			}
		}
	}

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

	if input.Idle {
		if c, ok := h.classifyRetry(lower); ok {
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

// apiErrorAfter reports whether the APIError matcher fires on the output
// following the last occurrence of costHit (a lower-case ASCII pattern).
func (h harnessAdapter) apiErrorAfter(stripped, costHit string) bool {
	if h.patterns.APIError == nil {
		return false
	}
	i := lastIndexFoldASCII(stripped, costHit)
	if i < 0 {
		return false
	}
	_, ok := h.patterns.APIError(stripped[i+len(costHit):])
	return ok
}

// lastIndexFoldASCII is strings.LastIndex with ASCII case folding. It
// searches s itself rather than strings.ToLower(s), whose byte offsets
// differ from s whenever lower-casing changes a rune's encoded length.
// sub must be lower-case ASCII.
func lastIndexFoldASCII(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if equalFoldASCII(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

// equalFoldASCII reports whether s equals the lower-case ASCII sub once
// s's ASCII letters are lower-cased. s and sub have the same length.
func equalFoldASCII(s, sub string) bool {
	for j := 0; j < len(sub); j++ {
		c := s[j]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != sub[j] {
			return false
		}
	}
	return true
}

// classifyRetry runs the idle-gated Retry / transport-retry matchers
// against already-lowercased stripped output. The bool is false when
// none matched.
func (h harnessAdapter) classifyRetry(lower string) (Classification, bool) {
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
