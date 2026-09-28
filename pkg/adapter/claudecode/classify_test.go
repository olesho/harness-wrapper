package claudecode

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// A failed turn is classed by what claude wrote: its tag, the HTTP status and,
// for a 429, whether the account's limit refused it (claude 2.1.283's texts).
func TestErrorClass(t *testing.T) {
	now := time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)
	reset := now.Add(90 * time.Minute)
	for _, c := range []struct {
		name   string
		f      failure
		class  contract.ErrorClass
		resume bool
	}{
		{"auth", failure{tag: "authentication_failed", status: 401}, contract.ErrorAuth, false},
		{"org not allowed", failure{tag: "oauth_org_not_allowed"}, contract.ErrorAuth, false},
		{"billing", failure{tag: "billing_error", status: 400}, contract.ErrorBilling, false},
		{"on hold", failure{tag: "account_on_hold"}, contract.ErrorBilling, false},
		{"usage wall", failure{tag: "rate_limit", status: 429, text: "You've hit your session limit · resets 10:47am (Europe/Tirane)"}, contract.ErrorUsageLimit, true},
		{"refused by the account", failure{tag: "rate_limit", status: 429, text: "limit", rejected: true, resetsAt: &reset}, contract.ErrorUsageLimit, true},
		{"server's 429", failure{tag: "rate_limit", status: 429, text: "API Error: Server is temporarily limiting requests (not your usage limit) · mock"}, contract.ErrorAPI, false},
		{"max output", failure{tag: "max_output_tokens"}, contract.ErrorMaxOutput, false},
		{"exhausted 529", failure{tag: "server_error", status: 529, text: "API Error: Repeated 529 Overloaded errors."}, contract.ErrorOverloaded, false},
		{"500", failure{tag: "server_error", status: 500}, contract.ErrorAPI, false},
		{"connection refused", failure{tag: "server_error", text: "API Error: Connection refused"}, contract.ErrorAPI, false},
		{"no word", failure{}, contract.ErrorInternal, false},
	} {
		e := c.f.turnError(now)
		if e.Class != c.class || (e.ResumeAt != nil) != c.resume {
			t.Errorf("%s: %+v, want %s (resume known: %v)", c.name, e, c.class, c.resume)
		}
		if e.ResumeAt != nil && !e.ResumeAt.After(now) {
			t.Errorf("%s: resume at %v, not after %v", c.name, e.ResumeAt, now)
		}
	}
}

// A rate_limit_event becomes the account's standing: its status in the
// caller's vocabulary, its windows by name, and when a refusal ends.
func TestRateLimit(t *testing.T) {
	raw := json.RawMessage(`{"status":"rejected","resetsAt":1790585225,"rateLimitType":"five_hour","isUsingOverage":false,
		"unifiedWindows":{"seven_day":{"utilization":0.5},"five_hour":{"utilization":1,"resetsAt":1790585225}}}`)
	d, resume, ok := rateLimit(raw)
	if !ok || d.Status != "rejected" || resume == nil || resume.Unix() != 1790585225 {
		t.Fatalf("rateLimit = %+v %v %v", d, resume, ok)
	}
	if len(d.Windows) != 2 || d.Windows[0].Name != "five_hour" || *d.Windows[0].UsedPct != 100 || d.Windows[1].Name != "seven_day" || d.Windows[1].ResetsAt != nil {
		t.Errorf("windows %+v", d.Windows)
	}
	if d, _, _ := rateLimit(json.RawMessage(`{"status":"allowed_warning","rateLimitType":"five_hour","utilization":0.9}`)); d.Status != "warning" || len(d.Windows) != 1 || *d.Windows[0].UsedPct != 90 {
		t.Errorf("a warning: %+v", d)
	}
	if _, _, ok := rateLimit(json.RawMessage(`{"resetsAt":1}`)); ok {
		t.Error("a report without a status is not one")
	}
}
