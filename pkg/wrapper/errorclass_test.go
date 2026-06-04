package wrapper_test

import (
	"context"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// TestClassifyOutput_Class verifies the one-shot assigns the canonical
// ErrorClass for each harness-output shape — the fine taxonomy the wrapper
// now owns (previously re-derived downstream in loom's adapter).
func TestClassifyOutput_Class(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		output  string
		want    wrapper.ErrorClass
	}{
		{"api 401 → auth", "claude", "API Error: 401 Unauthorized", wrapper.ErrAuth},
		{"api 402 → billing", "claude", "API Error: 402 Payment Required", wrapper.ErrBilling},
		{"api 404 → model-not-found", "claude", "API Error: 404 model not found", wrapper.ErrModelNotFound},
		{"api 429 → rate-limited", "claude", "API Error: 429 Too Many Requests", wrapper.ErrRateLimited},
		{"api 529 → transient", "claude", "API Error: 529 Overloaded", wrapper.ErrTransient},
		{"transport → transient", "claude", "Mock\nError: connection refused", wrapper.ErrTransient},
		{"usage limit → rate-limited", "claude", "you've hit your usage limit", wrapper.ErrRateLimited},
		{"quota exceeded → billing", "claude", "Error: quota exceeded", wrapper.ErrBilling},
		{"retry prose → transient", "claude", "upstream error, please try again", wrapper.ErrTransient},
		{"benign → none", "claude", "Step 1/3\nDONE", wrapper.ErrNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapper.ClassifyOutput(tc.harness, tc.output)
			if got.Class != tc.want {
				t.Fatalf("Class = %v (%q), want %v — status=%q reason=%q",
					got.Class, got.Class, tc.want, got.Status, got.Reason)
			}
		})
	}
}

// TestClassifyOutput_Cursor verifies the registered cursor pack adds
// cursor-specific Retry-prose coverage (timeout/transient) that the generic
// default classifier would miss, with the right ErrorClass.
func TestClassifyOutput_Cursor(t *testing.T) {
	cases := []struct {
		name   string
		output string
		status wrapper.Status
		class  wrapper.ErrorClass
	}{
		{"rate limit", "Error: rate limit exceeded", wrapper.StatusBlockedByCost, wrapper.ErrRateLimited},
		{"timeout prose", "the request timed out", wrapper.StatusRetryLater, wrapper.ErrTimeout},
		{"transient prose", "service unavailable, please try again", wrapper.StatusRetryLater, wrapper.ErrTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapper.ClassifyOutput("cursor", tc.output)
			if got.Status != tc.status || got.Class != tc.class {
				t.Fatalf("got status=%q class=%v, want status=%q class=%v",
					got.Status, got.Class, tc.status, tc.class)
			}
		})
	}
}

// TestErrorClass_StringWireCompat pins the canonical wire/display strings.
// Downstream serializers (loom's daemon-agents.json last_error_class, events,
// checkpoints) depend on these exact names — they must not drift from the Go
// identifiers (ErrAuth → "AuthFailure", not "ErrAuth").
func TestErrorClass_StringWireCompat(t *testing.T) {
	cases := map[wrapper.ErrorClass]string{
		wrapper.ErrNone:            "None",
		wrapper.ErrRateLimited:     "RateLimited",
		wrapper.ErrAuth:            "AuthFailure",
		wrapper.ErrBilling:         "BillingError",
		wrapper.ErrModelNotFound:   "ModelNotFound",
		wrapper.ErrContextOverflow: "ContextOverflow",
		wrapper.ErrTimeout:         "Timeout",
		wrapper.ErrTransient:       "Transient",
		wrapper.ErrUnknown:         "Unknown",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("ErrorClass(%d).String() = %q, want %q", c, got, want)
		}
	}
}

// TestRun_FailedExitCarriesClass is the carry-through guard (pt4): a harness
// that surfaces an API error and exits non-zero must report the class on
// Result.Class (not ErrNone/Unknown), so the retry layer can act on it.
func TestRun_FailedExitCarriesClass(t *testing.T) {
	w, drain := captureStdout(t)

	res, err := wrapper.Run(context.Background(), wrapper.Config{
		BinaryPath: mockHarnessBin,
		Args:       []string{"--mode", "failed", "--exit-code", "1", "--failed-msg", "API Error: 429 Too Many Requests"},
		Stdout:     w,
		Harness:    "claude",
	})
	_ = drain()

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != wrapper.ErrRateLimited {
		t.Fatalf("Result.Class = %v, want ErrRateLimited (failed exit carrying a 429 API error)", res.Class)
	}
	if res.Status != wrapper.StatusAPIError {
		t.Errorf("Result.Status = %q, want StatusAPIError", res.Status)
	}
}

// TestRun_PlainFailedClassNone guards against over-classification: a non-zero
// exit with no actionable fingerprint leaves Class ErrNone (loom's exit-code
// fallback maps that to Unknown).
func TestRun_PlainFailedClassNone(t *testing.T) {
	w, drain := captureStdout(t)

	res, err := wrapper.Run(context.Background(), wrapper.Config{
		BinaryPath: mockHarnessBin,
		Args:       []string{"--mode", "failed", "--exit-code", "5"},
		Stdout:     w,
		Harness:    "claude",
	})
	_ = drain()

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Class != wrapper.ErrNone {
		t.Errorf("Result.Class = %v, want ErrNone (plain failure, no fingerprint)", res.Class)
	}
}
