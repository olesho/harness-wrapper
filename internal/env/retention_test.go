package env

import (
	"errors"
	"testing"
)

func TestShouldKeep(t *testing.T) {
	cases := []struct {
		name      string
		retention Retention
		outcome   Outcome
		want      bool
	}{
		// setup-failure ALWAYS destroys, whatever the retention.
		{"setup-failure absent", "", OutcomeSetupFailure, false},
		{"setup-failure always", RetentionAlways, OutcomeSetupFailure, false},
		{"setup-failure keep-on-failure", RetentionKeepOnFailure, OutcomeSetupFailure, false},

		// always keeps on success and run-failure.
		{"always success", RetentionAlways, OutcomeSuccess, true},
		{"always failure", RetentionAlways, OutcomeFailure, true},

		// keep-on-failure keeps only on a failed run.
		{"keep-on-failure success", RetentionKeepOnFailure, OutcomeSuccess, false},
		{"keep-on-failure failure", RetentionKeepOnFailure, OutcomeFailure, true},

		// absent destroys on both.
		{"absent success", "", OutcomeSuccess, false},
		{"absent failure", "", OutcomeFailure, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ShouldKeep(c.retention, c.outcome); got != c.want {
				t.Fatalf("ShouldKeep(%q, %q) = %v, want %v", c.retention, c.outcome, got, c.want)
			}
		})
	}
}

func TestTeardownError(t *testing.T) {
	e1 := errors.New("boom one")
	e2 := errors.New("boom two")

	t.Run("with context", func(t *testing.T) {
		te := &TeardownError{Errors: []error{e1, e2}, Context: "ctx"}
		if te.Error() != "ctx: boom one; boom two" {
			t.Fatalf("unexpected message: %q", te.Error())
		}
	})

	t.Run("without context", func(t *testing.T) {
		te := &TeardownError{Errors: []error{e1, e2}}
		if te.Error() != "boom one; boom two" {
			t.Fatalf("unexpected message: %q", te.Error())
		}
	})

	t.Run("unwrap is traversable", func(t *testing.T) {
		te := &TeardownError{Errors: []error{e1, e2}, Context: "ctx"}
		if !errors.Is(te, e2) {
			t.Fatalf("errors.Is could not find e2 inside TeardownError")
		}
	})
}

func TestRunAll(t *testing.T) {
	var order []int
	e := errors.New("x")
	errs := runAll([]func() error{
		func() error { order = append(order, 1); return nil },
		func() error { order = append(order, 2); return e },
		func() error { order = append(order, 3); return e },
	})
	// All thunks run in order, never short-circuited.
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("thunks did not all run in order: %v", order)
	}
	if len(errs) != 2 {
		t.Fatalf("expected 2 collected errors, got %d", len(errs))
	}
}
