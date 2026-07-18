package wrapper

import (
	"strings"
	"testing"
	"time"

	claudeharness "github.com/olesho/harness-wrapper/pkg/wrapper/internal/harness/claude"
	codexharness "github.com/olesho/harness-wrapper/pkg/wrapper/internal/harness/codex"
)

// TestHarnessAdapter_Classify covers the classifier-level matrix from
// the plan: fast api_error detection (regardless of idle/quiet state),
// structured field propagation, and regression coverage for existing
// Cost/Retry/Prompt paths.
// classifyCase is one row of the classifier-level matrix exercised by
// TestHarnessAdapter_Classify.
type classifyCase struct {
	name           string
	adapter        harnessAdapter
	input          ClassifierInput
	wantStatus     Status
	wantCode       int
	wantRetry      time.Duration
	reasonHas      string
	wantResumeAtOK bool // verify Classification.ResumeAt is non-zero
}

func TestHarnessAdapter_Classify(t *testing.T) {
	claude := harnessAdapter{patterns: claudeharness.Patterns}
	codex := harnessAdapter{patterns: codexharness.Patterns}

	cases := []classifyCase{
		{
			name:       "A1: claude api_error 529 fires without idle gate",
			adapter:    claude,
			input:      ClassifierInput{RecentOutput: "API Error: 529 Overloaded."},
			wantStatus: StatusAPIError,
			wantCode:   529,
		},
		{
			name:       "A2: claude api_error 429 carries RetryAfter",
			adapter:    claude,
			input:      ClassifierInput{RecentOutput: "API Error: 429 Too Many Requests. Retry after 30 seconds."},
			wantStatus: StatusAPIError,
			wantCode:   429,
			wantRetry:  30 * time.Second,
		},
		{
			name:       "A2b: claude transport-error variant with tree-character prefix",
			adapter:    claude,
			input:      ClassifierInput{RecentOutput: "  ⎿  API Error: The socket connection was closed unexpectedly."},
			wantStatus: StatusAPIError,
			wantCode:   0,
			reasonHas:  "socket connection was closed",
		},
		{
			name:       "A4: codex exceeded retry limit with explicit 503",
			adapter:    codex,
			input:      ClassifierInput{RecentOutput: "■ exceeded retry limit, last status: 503"},
			wantStatus: StatusAPIError,
			wantCode:   503,
		},
		{
			name:       "A5: regression — cost path on idle",
			adapter:    claude,
			input:      ClassifierInput{RecentOutput: "you've hit your limit", Idle: true},
			wantStatus: StatusBlockedByCost,
		},
		{
			name:       "A6: regression — retry path on idle",
			adapter:    claude,
			input:      ClassifierInput{RecentOutput: "please try again", Idle: true},
			wantStatus: StatusRetryLater,
		},
		{
			name:       "A7: regression — prompt detection on quiet trailing line",
			adapter:    claude,
			input:      ClassifierInput{RecentOutput: "Some text\nContinue? [y/N]", Quiet: true},
			wantStatus: StatusWaitingForInput,
		},
		{
			name:       "A8: api_error wins over cost when both present",
			adapter:    claude,
			input:      ClassifierInput{RecentOutput: "you've hit your limit\nAPI Error: 529 Overloaded.", Idle: true},
			wantStatus: StatusAPIError,
			wantCode:   529,
		},
		{
			name:       "A9: false-positive guard — mid-line API Error in prose",
			adapter:    claude,
			input:      ClassifierInput{RecentOutput: "chitchat about API Error: 500 mid-line", Idle: true},
			wantStatus: "",
		},
		{
			// Golden session-limit transcript from a real Claude Code
			// session. The wrapper must classify this as blocked_by_cost
			// regardless of idle/quiet state (the banner is anchored on
			// the tree-character + exact phrase, so we don't need to
			// wait for the run to settle) AND populate ResumeAt.
			name:           "A11: claude session-limit banner fires without idle gate",
			adapter:        claude,
			input:          ClassifierInput{RecentOutput: "  ⎿  You've hit your session limit · resets 6:40pm (Europe/Warsaw)\n     /usage-credits to finish what you're working on."},
			wantStatus:     StatusBlockedByCost,
			reasonHas:      "session limit",
			wantResumeAtOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkClassifyCase(t, tc, tc.adapter.Classify(tc.input))
		})
	}
}

// checkClassifyCase asserts a single classifier result against its
// expectations.
func checkClassifyCase(t *testing.T, tc classifyCase, got Classification) {
	t.Helper()
	if got.Status != tc.wantStatus {
		t.Fatalf("Status = %q, want %q (got %+v)", got.Status, tc.wantStatus, got)
	}
	if tc.wantStatus == "" {
		return
	}
	if got.HTTPCode != tc.wantCode {
		t.Errorf("HTTPCode = %d, want %d", got.HTTPCode, tc.wantCode)
	}
	if got.RetryAfter != tc.wantRetry {
		t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, tc.wantRetry)
	}
	// Only api_error classifications are non-terminal at this
	// matcher entry point. Cost/Retry are terminal; prompts
	// are non-terminal but go through a different code path.
	wantTerminal := tc.wantStatus == StatusBlockedByCost || tc.wantStatus == StatusRetryLater
	if got.Terminal != wantTerminal {
		t.Errorf("Terminal = %v, want %v", got.Terminal, wantTerminal)
	}
	if tc.reasonHas != "" && !strings.Contains(got.Reason, tc.reasonHas) {
		t.Errorf("Reason = %q, want substring %q", got.Reason, tc.reasonHas)
	}
	if tc.wantResumeAtOK {
		if got.ResumeAt.IsZero() {
			t.Errorf("ResumeAt is zero, want non-zero")
		} else if !got.ResumeAt.After(time.Now()) {
			t.Errorf("ResumeAt = %s, want in the future", got.ResumeAt)
		}
	}
}
