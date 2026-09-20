package wrapper_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// residualIDs is every row id the finished-output fallback can report, by
// name. They are a downstream CONTRACT — loom's
// docs/adr/0002-authfailure-stays-terminal.md names residual.auth in a revisit
// trigger and records these as its evidence rule — so this list is spelled out
// rather than derived: a rename has to be a conscious edit here, not a silent
// one downstream.
var residualIDs = []string{
	"residual.ratelimit",
	"residual.auth",
	"residual.billing",
	"residual.model_version",
	"residual.model_not_found",
	"residual.context",
	"residual.timeout",
	"residual.transient",
}

// TestFinishedOutput_EveryResidualRowByID drives each row by a text only that
// row matches, and asserts the id, the class and the matched text.
func TestFinishedOutput_EveryResidualRowByID(t *testing.T) {
	cases := []struct {
		id     string
		output string
		class  wrapper.ErrorClass
		match  string
	}{
		{"residual.ratelimit", "HTTP 429 from upstream", wrapper.ErrRateLimited, "429"},
		{"residual.auth", "request rejected: unauthorized", wrapper.ErrAuth, "unauthorized"},
		{"residual.billing", "your billing needs attention", wrapper.ErrBilling, "billing"},
		{"residual.model_version", "this model requires a newer version", wrapper.ErrModelNotFound, "model requires a newer version"},
		{"residual.model_not_found", "unsupported model", wrapper.ErrModelNotFound, "unsupported model"},
		{"residual.context", "prompt too long", wrapper.ErrContextOverflow, "prompt too long"},
		{"residual.timeout", "etimedout", wrapper.ErrTimeout, "etimedout"},
		{"residual.transient", "service unavailable", wrapper.ErrTransient, "service unavailable"},
	}
	if len(cases) != len(residualIDs) {
		t.Fatalf("%d cases for %d rows — every row needs one", len(cases), len(residualIDs))
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := wrapper.ClassifyFinishedOutput("claude", tc.output)
			if got.Rule != tc.id {
				t.Errorf("Rule = %q, want %q", got.Rule, tc.id)
			}
			if got.Class != tc.class {
				t.Errorf("Class = %v, want %v", got.Class, tc.class)
			}
			if got.Match != tc.match {
				t.Errorf("Match = %q, want %q", got.Match, tc.match)
			}
			// A residual hit is a text fingerprint, not a lifecycle state.
			if got.Status != "" {
				t.Errorf("Status = %q, want empty for a residual hit", got.Status)
			}
		})
	}
}

// TestFinishedOutput_RowPrecedence pins the order, which IS the precedence.
// Text naming both a quota and a 500 is a rate limit, because that is the
// verdict that recovers on its own.
func TestFinishedOutput_RowPrecedence(t *testing.T) {
	cases := []struct {
		name, output, wantRule string
	}{
		{"ratelimit beats transient", "upstream returned 429; also a 500 server error", "residual.ratelimit"},
		{"auth beats billing", "401 unauthorized — check your billing", "residual.auth"},
		{"billing beats model", "payment required; unsupported model", "residual.billing"},
		{"timeout beats transient", "500 server error after connection timed out", "residual.timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wrapper.ClassifyFinishedOutput("claude", tc.output); got.Rule != tc.wantRule {
				t.Errorf("Rule = %q, want %q", got.Rule, tc.wantRule)
			}
		})
	}
}

// TestFinishedOutput_RateLimitCarriesRetryAfter asserts the one row that reads
// a Retry-After hint does, and that the others do not invent one.
func TestFinishedOutput_RateLimitCarriesRetryAfter(t *testing.T) {
	got := wrapper.ClassifyFinishedOutput("claude", "429 slow down\nretry-after: 45\n")
	if got.Rule != "residual.ratelimit" {
		t.Fatalf("Rule = %q, want residual.ratelimit", got.Rule)
	}
	if got.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s", got.RetryAfter)
	}
	if other := wrapper.ClassifyFinishedOutput("claude", "unauthorized\nretry-after: 45\n"); other.RetryAfter != 0 {
		t.Errorf("residual.auth RetryAfter = %v, want 0 — only the rate-limit row reads the hint", other.RetryAfter)
	}
}

// TestFinishedOutput_TimeoutRefinement covers the one refinement that is not a
// residual row: an ErrTransient result whose surrounding text names a timeout
// is a timeout, so it keeps its own class (and its own backoff bucket
// downstream) instead of a generic 5xx.
func TestFinishedOutput_TimeoutRefinement(t *testing.T) {
	// "socket hang up" is a transport-retry hit (ErrTransient); the timeout
	// wording is elsewhere in the blob, which is why retryClass alone misses it.
	const out = "Error: socket hang up\ncontext deadline exceeded while streaming"
	before := wrapper.ClassifyOutput("claude", out)
	if before.Class != wrapper.ErrTransient {
		t.Fatalf("precondition: ClassifyOutput Class = %v, want ErrTransient", before.Class)
	}
	got := wrapper.ClassifyFinishedOutput("claude", out)
	if got.Class != wrapper.ErrTimeout {
		t.Errorf("Class = %v, want ErrTimeout", got.Class)
	}
	if got.Rule != wrapper.RuleTimeoutUpgrade {
		t.Errorf("Rule = %q, want %q", got.Rule, wrapper.RuleTimeoutUpgrade)
	}
	// Everything else about the classifier's verdict survives the refinement.
	if got.Status != before.Status || got.Reason != before.Reason {
		t.Errorf("refinement rewrote Status/Reason: %q/%q, want %q/%q", got.Status, got.Reason, before.Status, before.Reason)
	}
	// A transient with no timeout wording is left alone.
	plain := wrapper.ClassifyFinishedOutput("claude", "Error: socket hang up")
	if plain.Class != wrapper.ErrTransient || plain.Rule != "" {
		t.Errorf("plain transient = %v/%q, want ErrTransient with no rule", plain.Class, plain.Rule)
	}
}

// TestFinishedOutput_ActionableResultsPassThrough asserts the fallback never
// second-guesses a classifier that already had an answer — including the
// binary-not-found case, which is a statement about the LAUNCH and must not be
// re-read as output.
func TestFinishedOutput_ActionableResultsPassThrough(t *testing.T) {
	cases := []struct{ name, harness, output string }{
		// Carries "429", which residual.ratelimit would also match — the point
		// is that the anchored matcher's richer verdict (with its HTTPCode) is
		// what survives.
		{"api error", "claude", "API Error: 429 Too Many Requests"},
		{"cost", "", "ERROR: quota exceeded. try later."},
		{"transport", "codex", "request failed: connect ECONNREFUSED 127.0.0.1:443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := wrapper.ClassifyOutput(tc.harness, tc.output)
			got := wrapper.ClassifyFinishedOutput(tc.harness, tc.output)
			if got != before {
				t.Errorf("finished-output changed an actionable result:\n got  %+v\n want %+v", got, before)
			}
		})
	}
}

// TestFinishedOutput_NothingMatchedReturnsTheOriginal asserts the caller's own
// fallback (an exit code, typically) still gets its turn.
func TestFinishedOutput_NothingMatchedReturnsTheOriginal(t *testing.T) {
	for _, out := range []string{"", "wrote 3 files, all tests pass"} {
		got := wrapper.ClassifyFinishedOutput("claude", out)
		if got != (wrapper.Classification{}) {
			t.Errorf("ClassifyFinishedOutput(%q) = %+v, want the zero Classification", out, got)
		}
	}
}

// TestFinishedOutput_LiveIsolation is the guard that keeps the residual rows
// out of the live path.
//
// The rows are the broadest patterns in the library: residual.billing matches a
// bare "billing", residual.auth matches API-key VARIABLE NAMES. An agent that
// merely prints such a word while working must not be able to terminate its own
// healthy process — which is what would happen if the rows ever reached
// ClassifyOutput or a live Classify.
//
// The property: any text whose ONLY classifier is a residual row must be
// invisible to the plain one-shot. This is true by construction today (the
// rows run only after ClassifyOutput yields nothing actionable) and the test
// exists so a future refactor that "shares" the table cannot land quietly.
//
// It says nothing about the per-harness packs' own breadth — cursor escalates
// on "timed out", opencode on "billing" — which predates this entry point and
// is pinned separately by TestClassifyOutput_UnchangedByFinishedOutput.
func TestFinishedOutput_LiveIsolation(t *testing.T) {
	prose := []string{
		"I'll check the billing module next",
		"reading ANTHROPIC_API_KEY from the environment",
		"the docs mention a quota of 50 rows",
		"renaming credits_total to credit_total",
		"the request timed out in the fixture, as expected",
		"fatal: 401 unauthorized",
		"prompt too long",
	}
	residualDriven := 0
	for _, harness := range []string{"claude", "claude-code", "codex", "cursor", "opencode", "pi", ""} {
		for _, text := range prose {
			fin := wrapper.ClassifyFinishedOutput(harness, text)
			if !strings.HasPrefix(fin.Rule, "residual.") {
				continue // some pack already had an opinion; not this test's business
			}
			residualDriven++
			if live := wrapper.ClassifyOutput(harness, text); live != (wrapper.Classification{}) {
				t.Errorf("harness %q, text %q: the residual row %s decided it, yet ClassifyOutput escalated to %+v",
					harness, text, fin.Rule, live)
			}
		}
	}
	if residualDriven == 0 {
		t.Fatal("no text in the set was decided by a residual row — the test proved nothing")
	}
}

// TestClassifyOutput_UnchangedByFinishedOutput pins the live one-shot against a
// verdict-for-verdict capture taken from v0.9.1, the release BEFORE the
// finished-output entry point existed.
//
// Adding a post-exit fallback must not move a single live result: Status and
// Terminal are what the wrapper acts on while a harness is still running, and
// a changed Terminal is a SIGTERM to a process that was fine. The golden is
// the evidence that it did not, rather than an argument that it could not.
func TestClassifyOutput_UnchangedByFinishedOutput(t *testing.T) {
	type row struct {
		Harness, Text, Status, Class, Reason string
		Terminal                             bool
		HTTPCode                             int
		RetryAfterMS                         int64
	}
	b, err := os.ReadFile("testdata/live_classification_v0.9.1.golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var want []row
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("golden is empty")
	}
	for _, w := range want {
		g := wrapper.ClassifyOutput(w.Harness, w.Text)
		got := row{w.Harness, w.Text, string(g.Status), g.Class.String(), g.Reason, g.Terminal, g.HTTPCode, g.RetryAfter.Milliseconds()}
		if got != w {
			t.Errorf("live verdict moved for harness %q, text %q:\n got  %+v\n want %+v", w.Harness, w.Text, got, w)
		}
	}
}

// TestFinishedOutput_ResidualReachesKnownHarnesses is why the rows could not
// simply be moved into defaultClassifier: a known harness selects its own
// adapter and never reaches the default, so a fallback placed there would have
// been dead code for every harness that matters.
func TestFinishedOutput_ResidualReachesKnownHarnesses(t *testing.T) {
	for _, harness := range []string{"claude", "claude-code", "codex", "cursor", "opencode", "pi", "", "some-unknown-harness"} {
		got := wrapper.ClassifyFinishedOutput(harness, "fatal: 401 unauthorized")
		if got.Rule != "residual.auth" || got.Class != wrapper.ErrAuth {
			t.Errorf("harness %q: Rule/Class = %q/%v, want residual.auth/ErrAuth", harness, got.Rule, got.Class)
		}
	}
}
