package wrapper

import (
	"strings"

	"github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"
)

// harnessAdapter turns a per-harness pattern set into a Classifier.
// Pattern matching always runs on stripped output so ANSI escapes do
// not interfere.
type harnessAdapter struct {
	patterns detector.Patterns
}

// Classify checks recent output against the harness's patterns. Cost,
// quota, and retry-later matches are terminal — the wrapper terminates
// the process so the engine can decide what to do next. Prompt
// matches signal waiting_for_input; the harness keeps running.
func (h harnessAdapter) Classify(input ClassifierInput) Classification {
	stripped := stripANSIEscapes(input.RecentOutput)
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
