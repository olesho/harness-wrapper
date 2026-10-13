package pi

import (
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// The error texts pi 1.0.4 records, against the mock (probes/pirpc
// TestErrorTexts), and the class each is.
func TestFailure(t *testing.T) {
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		text   string
		class  contract.ErrorClass
		status int
	}{
		{`529 {"error":{"message":"mock overloaded_error 3/99","type":"overloaded_error"},"type":"error"}`, contract.ErrorOverloaded, 529},
		{`429 {"error":{"message":"mock rate_limit_error 1/99","type":"rate_limit_error"},"type":"error"}`, contract.ErrorAPI, 429},
		{`500 {"error":{"message":"mock api_error 1/99","type":"api_error"},"type":"error"}`, contract.ErrorAPI, 500},
		{`401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, contract.ErrorAuth, 401},
		{`400 {"type":"error","error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."}}`, contract.ErrorBilling, 400},
		{`OpenAI API error (429): {"message":"You exceeded your current quota","type":"insufficient_quota"}`, contract.ErrorBilling, 429},
		{`OpenAI API error (401): {"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}`, contract.ErrorAuth, 401},
		{"server_is_overloaded: mock server_is_overloaded 3/3", contract.ErrorOverloaded, 0},
		{"server_error: mock server_error 1/99", contract.ErrorAPI, 0},
		{abortedMessage, contract.ErrorInternal, 0},
		{"", contract.ErrorInternal, 0},
	} {
		got := failure(c.text, now)
		if got.Class != c.class || got.HTTPStatus != c.status || got.ResumeAt != nil {
			t.Errorf("%q: %+v, want class %s, status %d", c.text, got, c.class, c.status)
		}
	}
}

// OpenAI's usage wall says when it resets: resets_at, or failing that
// resets_in_seconds from now.
func TestFailureUsageLimit(t *testing.T) {
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	got := failure(`OpenAI API error (429): {"message":"mock usage limit","resets_at":1791285234,"resets_in_seconds":3600,"type":"usage_limit_reached"}`, now)
	if got.Class != contract.ErrorUsageLimit || got.HTTPStatus != 429 || got.ResumeAt == nil || !got.ResumeAt.Equal(time.Unix(1791285234, 0)) {
		t.Errorf("resets_at: %+v", got)
	}
	got = failure(`OpenAI API error (429): {"message":"mock usage limit","resets_in_seconds":3600,"type":"usage_limit_reached"}`, now)
	if got.Class != contract.ErrorUsageLimit || got.ResumeAt == nil || !got.ResumeAt.Equal(now.Add(time.Hour)) {
		t.Errorf("resets_in_seconds: %+v", got)
	}
	got = failure(`OpenAI API error (429): {"message":"mock usage limit","type":"usage_limit_reached"}`, now)
	if got.Class != contract.ErrorUsageLimit || got.ResumeAt != nil {
		t.Errorf("no reset: %+v", got)
	}
}

// The reset time is read from the decoded JSON body, so a float, a nested
// error object or trailing text still yields it; a string value or a
// non-positive one does not.
func TestFailureUsageLimitDecodesBody(t *testing.T) {
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		text string
		want *time.Time
	}{
		{`429 {"type":"usage_limit_reached","resets_at":1.791285234e9}`, ptr(time.Unix(1791285234, 0))},
		{`429 {"error":{"type":"usage_limit_reached","resets_in_seconds":60.5}}`, ptr(now.Add(60500 * time.Millisecond))},
		{`429 {"type":"usage_limit_reached","resets_in_seconds":60} (retrying)`, ptr(now.Add(time.Minute))},
		{`429 {"type":"usage_limit_reached","resets_at":"1791285234"}`, nil},
		{`429 {"type":"usage_limit_reached","resets_at":0,"resets_in_seconds":-5}`, nil},
		{`429 usage_limit_reached "resets_at": 1791285234`, nil},
	} {
		got := failure(c.text, now)
		if (got.ResumeAt == nil) != (c.want == nil) || (c.want != nil && !got.ResumeAt.Equal(*c.want)) {
			t.Errorf("%q: ResumeAt %v, want %v", c.text, got.ResumeAt, c.want)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }
