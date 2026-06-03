package wrapper_test

import (
	"context"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// TestClassifyOutput_Transport verifies that connection-refused / transport
// failures classify as StatusRetryLater through the public one-shot, for both
// the per-harness adapters (claude/codex/gemini) and the generic default that
// backs unknown harnesses (e.g. cursor, opencode). This is the bare-transport
// case both classifiers previously missed.
func TestClassifyOutput_Transport(t *testing.T) {
	cases := []struct {
		name    string
		harness string
		output  string
	}{
		{"claude/connection refused", "claude", "Mock Agent CLI\nError: connection refused"},
		{"codex/ECONNREFUSED", "codex", "request failed: connect ECONNREFUSED 127.0.0.1:443"},
		{"gemini/fetch failed", "gemini", "fetch failed: read ECONNRESET"},
		{"unknown/connection refused", "", "node:internal/net: connection refused"},
		{"cursor/socket hang up", "cursor", "Error: socket hang up"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapper.ClassifyOutput(tc.harness, tc.output)
			if got.Status != wrapper.StatusRetryLater {
				t.Fatalf("Status = %q, want StatusRetryLater (reason=%q)", got.Status, got.Reason)
			}
		})
	}
}

// TestClassifyOutput_APIErrorCarriesHTTPCode verifies the one-shot surfaces the
// upstream HTTP code for a structured API error so consumers can dispatch on it.
func TestClassifyOutput_APIErrorCarriesHTTPCode(t *testing.T) {
	got := wrapper.ClassifyOutput("claude", "API Error: 429 Too Many Requests")
	if got.Status != wrapper.StatusAPIError {
		t.Fatalf("Status = %q, want StatusAPIError", got.Status)
	}
	if got.HTTPCode != 429 {
		t.Fatalf("HTTPCode = %d, want 429", got.HTTPCode)
	}
}

// TestClassifyOutput_CostSpecificReason verifies the default classifier reports
// the matched cost phrase (not a generic string) so downstream consumers can
// split billing from rate-limit signals.
func TestClassifyOutput_CostSpecificReason(t *testing.T) {
	got := wrapper.ClassifyOutput("", "ERROR: quota exceeded. try later.")
	if got.Status != wrapper.StatusBlockedByCost {
		t.Fatalf("Status = %q, want StatusBlockedByCost", got.Status)
	}
	if !strings.Contains(got.Reason, "quota exceeded") {
		t.Fatalf("Reason = %q, want the matched phrase 'quota exceeded'", got.Reason)
	}
}

// TestClassifyOutput_Benign returns the zero Classification for ordinary output.
func TestClassifyOutput_Benign(t *testing.T) {
	got := wrapper.ClassifyOutput("claude", "Step 1/3\nStep 2/3\nDONE")
	if got.Status != "" {
		t.Fatalf("Status = %q, want empty (no classification)", got.Status)
	}
}

// TestClassifyOutput_PromptTailNotWaiting verifies a trailing interactive
// prompt in a finished blob is NOT reported as waiting_for_input: the one-shot
// leaves Quiet off because a dead process is not awaiting input.
func TestClassifyOutput_PromptTailNotWaiting(t *testing.T) {
	got := wrapper.ClassifyOutput("claude", "Apply changes?\nContinue? (y/n)")
	if got.Status == wrapper.StatusWaitingForInput {
		t.Fatalf("Status = StatusWaitingForInput, want it suppressed on a post-hoc tail")
	}
}

// TestRun_FailedExitTransportUpgradesToRetryLater is the fast-exit regression:
// a harness that prints a transport error and exits non-zero before the idle
// classifier ever polls must still terminate as StatusRetryLater (retryable),
// not a bare StatusFailed. Without the on-exit final classification this run
// would report StatusFailed and the retry layer would give up.
func TestRun_FailedExitTransportUpgradesToRetryLater(t *testing.T) {
	w, drain := captureStdout(t)

	res, err := wrapper.Run(context.Background(), wrapper.Config{
		BinaryPath: mockHarnessBin,
		Args:       []string{"--mode", "failed", "--exit-code", "7", "--failed-msg", "Error: connect ECONNREFUSED 127.0.0.1:443"},
		Stdout:     w,
		Harness:    "claude",
	})
	_ = drain()

	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != wrapper.StatusRetryLater {
		t.Fatalf("Status = %q, want StatusRetryLater (transport error on fast exit)", res.Status)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
}

// TestRun_FailedExitPlainStaysFailed guards against over-classification: a
// non-zero exit whose output has no actionable fingerprint stays StatusFailed.
func TestRun_FailedExitPlainStaysFailed(t *testing.T) {
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
	if res.Status != wrapper.StatusFailed {
		t.Fatalf("Status = %q, want StatusFailed (plain failure, no upgrade)", res.Status)
	}
}
