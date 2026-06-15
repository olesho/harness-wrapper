package pi

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/wrapper/internal/detector"
)

// pi is provider-agnostic, so there is no single anchored API-error
// format to match. APIError must stay nil until a corpus reveals one;
// SessionLimit likewise has no pi-specific banner yet.
func TestPatternsHasNoAnchoredMatchers(t *testing.T) {
	if Patterns.APIError != nil {
		t.Error("APIError should be nil for pi until a corpus pins a per-provider format")
	}
	if Patterns.SessionLimit != nil {
		t.Error("SessionLimit should be nil for pi (no harness-specific banner observed)")
	}
}

// The conservative cost/retry/prompt lists must be populated and
// lower-cased: detector.MatchAny is called against ToLower(output), so
// any upper-case entry would be dead weight that never matches.
func TestPatternListsArePopulatedAndLowercase(t *testing.T) {
	for name, list := range map[string][]string{
		"Cost":   Patterns.Cost,
		"Retry":  Patterns.Retry,
		"Prompt": Patterns.Prompt,
	} {
		if len(list) == 0 {
			t.Errorf("%s pattern list is empty", name)
		}
		for _, p := range list {
			if p != strings.ToLower(p) {
				t.Errorf("%s pattern %q is not lower-case; MatchAny matches against lower-cased output", name, p)
			}
		}
	}
}

// Spot-check that representative provider-neutral phrasings resolve to
// the expected status through the same MatchAny path the wrapper uses.
func TestRepresentativeCostAndRetryPhrasesMatch(t *testing.T) {
	if hit := detector.MatchAny("error: rate limit exceeded, please slow down", Patterns.Cost); hit == "" {
		t.Error("expected a rate-limit phrase to match the Cost list")
	}
	if hit := detector.MatchAny("the model is currently overloaded", Patterns.Retry); hit == "" {
		t.Error("expected an overloaded phrase to match the Retry list")
	}
}
