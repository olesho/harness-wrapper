package wrapper

import (
	"strings"
	"time"

	claudeharness "github.com/olesho/harness-wrapper/pkg/wrapper/internal/harness/claude"
	codexharness "github.com/olesho/harness-wrapper/pkg/wrapper/internal/harness/codex"
	cursorharness "github.com/olesho/harness-wrapper/pkg/wrapper/internal/harness/cursor"
	opencodeharness "github.com/olesho/harness-wrapper/pkg/wrapper/internal/harness/opencode"
	piharness "github.com/olesho/harness-wrapper/pkg/wrapper/internal/harness/pi"
)

// ClassifierInput is the snapshot a Classifier inspects when deciding
// whether to escalate the wrapper's status. It is rebuilt each time
// the wrapper polls the classifier; classifiers are stateless.
type ClassifierInput struct {
	// RecentOutput is the tail of the harness PTY output (last ~64KB),
	// ANSI escapes intact. Classifiers that grep should strip escapes.
	RecentOutput string

	// SinceLastOutput is the duration since the harness last produced a
	// byte. Classifiers can use this to distinguish "actively producing"
	// from "paused at a prompt".
	SinceLastOutput time.Duration

	// Quiet is true once SinceLastOutput >= IdleQuiet.
	Quiet bool

	// Idle is true once SinceLastOutput >= IdleClassify. The wrapper's
	// default behavior at this threshold is to classify the run as
	// idle; a Classifier can override by returning a non-zero
	// Classification.
	Idle bool
}

// Classification is a Classifier's verdict for a single ClassifierInput.
type Classification struct {
	// Status is the actionable status the classifier matched. The zero
	// value (empty string) means "no classification".
	Status Status

	// Class is the canonical harness-output error taxonomy for this
	// classification (the mechanism half of the contract; consumers map
	// it to policy). ErrNone for non-error states (waiting_for_input) and
	// the zero Classification.
	Class ErrorClass

	// Reason is a short human-readable description that surfaces in the
	// Result and any emitted events.
	Reason string

	// Terminal indicates the wrapper should terminate the harness
	// process to make progress. Set true for blocked_by_cost and
	// retry_later; leave false for waiting_for_input where the harness
	// is alive and just paused at a prompt.
	Terminal bool

	// HTTPCode is the upstream API's HTTP status code when Status is
	// StatusAPIError and the harness surfaced a numeric code. Zero for
	// transport errors (e.g. socket closed) and for all non-api_error
	// classifications.
	HTTPCode int

	// RetryAfter is the wait duration the harness suggested (e.g.
	// "Retry after 30 seconds"). Zero when the message contained no
	// parseable hint.
	RetryAfter time.Duration

	// ResumeAt is the absolute wall-clock time at which the harness
	// expects to be usable again, parsed from session-limit banners
	// like "resets 6:40pm (Europe/Warsaw)". Zero when no parseable
	// hint was present.
	//
	// Unlike RetryAfter (relative; advisory), ResumeAt is intended for
	// scheduling: callers that want to retry the run after the limit
	// resets should sleep / cron-schedule against this time rather
	// than against time.Now() + RetryAfter.
	ResumeAt time.Time
}

// Classifier inspects recent harness output and reports actionable
// status classifications. Implementations must be safe for concurrent
// use.
//
// Classifiers are stateless: the wrapper rebuilds ClassifierInput on
// each poll. Returning the same Classification across consecutive
// polls is fine; the wrapper de-duplicates emitted events.
type Classifier interface {
	Classify(input ClassifierInput) Classification
}

// ClassifierFunc adapts a function to the Classifier interface.
type ClassifierFunc func(input ClassifierInput) Classification

// Classify calls f.
func (f ClassifierFunc) Classify(input ClassifierInput) Classification { return f(input) }

// ClassifyOutput runs the resolved per-harness classifier as a one-shot
// over a finished output blob (e.g. a log tail, or the recent-output buffer
// of an exited harness). Idle is forced on so the Cost/Retry/transport
// patterns are eligible; Quiet is left off so a trailing interactive prompt
// in a *dead* process's tail is not misreported as waiting_for_input.
// Returns the zero Classification when nothing matches.
//
// It is the post-hoc counterpart to the live polling the wrapper performs
// during a run — the single entry point external callers (e.g. loom's
// agenterr adapter) should use to classify captured output with the same
// patterns the wrapper applies internally.
func ClassifyOutput(harness, output string) Classification {
	return resolveClassifier(Config{Harness: harness}).Classify(ClassifierInput{
		RecentOutput: output,
		Idle:         true,
		Quiet:        false,
	})
}

// resolveClassifier picks the Classifier for a config. Order:
//  1. cfg.Classifier if set.
//  2. A per-harness classifier matching cfg.Harness.
//  3. A generic default that detects cost/quota patterns.
func resolveClassifier(cfg Config) Classifier {
	if cfg.Classifier != nil {
		return cfg.Classifier
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Harness)) {
	case "claude", "claude-code":
		return harnessAdapter{patterns: claudeharness.Patterns}
	case "codex":
		return harnessAdapter{patterns: codexharness.Patterns}
	case "cursor":
		return harnessAdapter{patterns: cursorharness.Patterns}
	case "opencode":
		return harnessAdapter{patterns: opencodeharness.Patterns}
	case "pi":
		return harnessAdapter{patterns: piharness.Patterns}
	}
	return defaultClassifier{}
}

// defaultClassifier is the built-in fallback. It only escalates to
// blocked_by_cost when recent output matches a known cost/quota
// fingerprint, and only after the wrapper has decided the run looks
// idle, which keeps the fallback conservative.
type defaultClassifier struct{}

// Classify returns blocked_by_cost when the harness has been quiet for
// the classify threshold and the recent output looks like a quota or
// rate-limit message. Otherwise it returns the zero Classification,
// letting the wrapper apply its default idle outcome.
func (defaultClassifier) Classify(input ClassifierInput) Classification {
	if !input.Idle {
		return Classification{}
	}
	if phrase, ok := isCostOrQuotaLimited(input.RecentOutput); ok {
		return Classification{
			Status:   StatusBlockedByCost,
			Class:    costClass(phrase),
			Reason:   phrase,
			Terminal: true,
		}
	}
	if c, ok := matchTransportRetry(strings.ToLower(stripANSIEscapes(input.RecentOutput))); ok {
		return c
	}
	return Classification{}
}
