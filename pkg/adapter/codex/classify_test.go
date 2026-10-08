package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
	tcodex "github.com/olesho/harness-wrapper/pkg/transcript/codex"
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

// A usage wall's "try again at 9:58 PM" is a time of day in the Host's zone,
// codex's own, on a Host whose zone is not UTC too: read so from the live
// transport, whose now is local, and from the record, whose entry times are
// UTC.
func TestUsageWallResumesInTheHostZone(t *testing.T) {
	local := time.Local
	time.Local = time.FixedZone("UTC+2", 2*60*60)
	t.Cleanup(func() { time.Local = local })
	want := time.Date(2026, 9, 28, 21, 58, 0, 0, time.Local) // 19:58 UTC
	wall := turnError{Message: "… or try again at 9:58 PM.", CodexErrorInfo: json.RawMessage(`"usageLimitExceeded"`)}
	for _, now := range []time.Time{
		time.Date(2026, 9, 28, 20, 0, 0, 0, time.Local), // the live transport's
		time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC),   // a record's: the same instant
	} {
		if e := failure(wall, nil, now); e.ResumeAt == nil || !e.ResumeAt.Equal(want) {
			t.Errorf("now %s: resumes at %v, want %s", now.Format(time.RFC3339), e.ResumeAt, want.UTC())
		}
	}
	d, ok := ended(&tcodex.Entry{Kind: "task_complete", ErrorInfo: "usageLimitExceeded", ErrorMessage: wall.Message},
		time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC))
	if !ok || d.Error == nil || d.Error.ResumeAt == nil || !d.Error.ResumeAt.Equal(want) {
		t.Errorf("the record's turn ended %+v, want it to resume at %s", d.Error, want.UTC())
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
