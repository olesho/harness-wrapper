package env

import "strings"

// Retention resolution + error aggregation — shared by the Local provisioner,
// Compose, and the Env lifecycle engine.

// ShouldKeep reports whether a resource is KEPT (not destroyed) for the given
// retention + outcome.
//
// Contract (design §4):
//   - OutcomeSetupFailure   ⇒ ALWAYS destroy (never keep) regardless of
//     retention: a preflight/apply failure leaves nothing of debugging value.
//   - RetentionAlways        ⇒ keep on success and run-failure.
//   - RetentionKeepOnFailure ⇒ keep only on a failed run.
//   - the empty Retention    ⇒ destroy on both success and failure (common case).
func ShouldKeep(retention Retention, outcome Outcome) bool {
	if outcome == OutcomeSetupFailure {
		return false
	}
	if retention == RetentionAlways {
		return true
	}
	if retention == RetentionKeepOnFailure {
		return outcome == OutcomeFailure
	}
	return false
}

// TeardownError aggregates several teardown failures without short-circuiting
// (design §4: best-effort, errors aggregated, never short-circuited). It carries
// a stable name and a readable message so callers can log it directly.
type TeardownError struct {
	Errors  []error
	Context string
}

func (e *TeardownError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, err := range e.Errors {
		if err != nil {
			parts[i] = err.Error()
		}
	}
	detail := strings.Join(parts, "; ")
	if e.Context != "" {
		return e.Context + ": " + detail
	}
	return detail
}

// Unwrap exposes the aggregated errors so errors.Is/As can traverse them.
func (e *TeardownError) Unwrap() []error {
	return e.Errors
}

// runAll runs every cleanup thunk in order, collecting (never re-throwing)
// failures. It returns the collected errors so the caller decides how to
// surface them.
func runAll(thunks []func() error) []error {
	var errs []error
	for _, t := range thunks {
		if err := t(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
