package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// codex's errors, as its protocol and its rollout name them, in the
// contract's classes.
func TestErrorClass(t *testing.T) {
	for info, want := range map[string]struct {
		class  contract.ErrorClass
		status int
	}{
		`"usageLimitExceeded"`:   {contract.ErrorUsageLimit, 0},
		`"usage_limit_exceeded"`: {contract.ErrorUsageLimit, 0},
		`"serverOverloaded"`:     {contract.ErrorOverloaded, 0},
		`"server_overloaded"`:    {contract.ErrorOverloaded, 0},
		`"unauthorized"`:         {contract.ErrorAuth, 0},
		`"internalServerError"`:  {contract.ErrorAPI, 0},
		`"other"`:                {contract.ErrorAPI, 0},
		`"sandboxError"`:         {contract.ErrorInternal, 0},
		`{"responseStreamDisconnected": {"httpStatusCode": 529}}`:      {contract.ErrorAPI, 529},
		`{"response_stream_disconnected": {"http_status_code": null}}`: {contract.ErrorAPI, 0},
		`null`: {contract.ErrorInternal, 0},
	} {
		name, status := errorInfo(json.RawMessage(info))
		if got := errorClass(name); got != want.class || status != want.status {
			t.Errorf("%s: %s %d, want %s %d", info, got, status, want.class, want.status)
		}
	}
}

// A usage wall resumes when the full window resets, as the latest rate-limit
// report says; without one, when the message says to try again.
func TestUsageWallResumes(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	full, some := 100.0, 12.0
	at := reset.Unix()
	limits := &rateLimits{Primary: &rateLimitWindow{UsedPercent: &full, ResetsAt: &at}, Secondary: &rateLimitWindow{UsedPercent: &some}}
	e := failure(turnError{Message: "You've hit your usage limit.", CodexErrorInfo: json.RawMessage(`"usageLimitExceeded"`)}, limits, time.Now())
	if e.Class != contract.ErrorUsageLimit || e.ResumeAt == nil || !e.ResumeAt.Equal(reset) {
		t.Errorf("failure %+v", e)
	}
	d := limits.data()
	if d.Status != "rejected" || len(d.Windows) != 2 || d.Windows[0].Name != "primary" || *d.Windows[0].UsedPct != 100 {
		t.Errorf("data %+v", d)
	}
	now := time.Date(2026, 9, 28, 20, 0, 0, 0, time.Local)
	e = failure(turnError{Message: "… or try again at 9:58 PM.", CodexErrorInfo: json.RawMessage(`"usageLimitExceeded"`)}, nil, now)
	if e.ResumeAt == nil || e.ResumeAt.In(time.Local).Hour() != 21 || e.ResumeAt.In(time.Local).Minute() != 58 {
		t.Errorf("from the message: %+v", e.ResumeAt)
	}
}

// A rate-limit update is sparse: what it leaves out stands.
func TestRateLimitsMerge(t *testing.T) {
	p, s := 10.0, 20.0
	r := (*rateLimits)(nil).merge(&rateLimits{Primary: &rateLimitWindow{UsedPercent: &p}, Secondary: &rateLimitWindow{UsedPercent: &s}})
	q := 50.0
	r = r.merge(&rateLimits{Primary: &rateLimitWindow{UsedPercent: &q}})
	if *r.Primary.UsedPercent != 50 || r.Secondary == nil || *r.Secondary.UsedPercent != 20 {
		t.Errorf("merged %+v %+v", r.Primary, r.Secondary)
	}
}

func TestRetryNotice(t *testing.T) {
	d := retry(turnError{Message: "Reconnecting... 1/2", CodexErrorInfo: json.RawMessage(`{"responseStreamDisconnected": {"httpStatusCode": 529}}`)})
	if d.Attempt != 1 || d.Max != 2 || d.HTTPStatus != 529 {
		t.Errorf("retry %+v", d)
	}
}
