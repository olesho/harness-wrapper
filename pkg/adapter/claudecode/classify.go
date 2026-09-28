package claudecode

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/internal/resettime"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// failure is what claude wrote about a failed model call: the synthetic
// assistant message's error tag and text, and the HTTP status.
type failure struct {
	tag    string
	status int
	text   string
	// rejected: the account's usage limit refused the turn (a rate_limit_event
	// with status rejected), resetting at resetsAt when known.
	rejected bool
	resetsAt *time.Time
}

// errorClass classifies f. A rate_limit tag is a usage wall only when the
// account's limit refused the call; claude tags a server-side 429 it gave up
// retrying the same way ("… (not your usage limit)", claude 2.1.283).
func (f failure) errorClass() contract.ErrorClass {
	switch f.tag {
	case "authentication_failed", "oauth_org_not_allowed", "verification_required", "cloud_credential_error":
		return contract.ErrorAuth
	case "billing_error", "account_on_hold":
		return contract.ErrorBilling
	case "rate_limit":
		if f.rejected || usageWallText(f.text) {
			return contract.ErrorUsageLimit
		}
		return contract.ErrorAPI
	case "max_output_tokens":
		return contract.ErrorMaxOutput
	case "overloaded":
		return contract.ErrorOverloaded
	}
	switch {
	case f.status == 529:
		return contract.ErrorOverloaded
	case f.tag != "" || f.status != 0:
		return contract.ErrorAPI
	}
	return contract.ErrorInternal
}

// usageWallText reports whether text is claude's usage wall: "You've hit your
// session limit · resets 10:47am (Europe/Tirane)".
func usageWallText(text string) bool {
	t := strings.ToLower(text)
	return strings.Contains(t, "hit your") && !strings.Contains(t, "not your usage limit")
}

// turnError is the contract's account of f.
func (f failure) turnError(now time.Time) *contract.TurnError {
	e := &contract.TurnError{Class: f.errorClass(), HTTPStatus: f.status}
	if e.Class == contract.ErrorUsageLimit {
		if at, ok := resettime.Parse(f.text, now); ok {
			at = at.UTC()
			e.ResumeAt = &at
		} else if f.resetsAt != nil {
			at := f.resetsAt.UTC()
			e.ResumeAt = &at
		}
	}
	return e
}

// rateLimitInfo is claude's rate_limit_info (claude 2.1.283). Numbers are
// read as floats, so an integer written as 1.79e9 still reads.
type rateLimitInfo struct {
	Status         string   `json:"status"`
	ResetsAt       *float64 `json:"resetsAt"` // Unix seconds
	RateLimitType  string   `json:"rateLimitType"`
	Utilization    *float64 `json:"utilization"`
	UnifiedWindows map[string]struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    *float64 `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

// rateLimit reads a rate_limit_info: the observation's data, and when the
// account can work again if it was refused. ok is false for a report without
// a status, which is not one.
func rateLimit(raw json.RawMessage) (data contract.RateLimitData, resumeAt *time.Time, ok bool) {
	var info rateLimitInfo
	if len(raw) == 0 || json.Unmarshal(raw, &info) != nil || info.Status == "" {
		return contract.RateLimitData{}, nil, false
	}
	data.Status = rateLimitStatus(info.Status)
	names := make([]string, 0, len(info.UnifiedWindows))
	for name := range info.UnifiedWindows {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w := info.UnifiedWindows[name]
		if w.Utilization == nil && w.ResetsAt == nil {
			continue
		}
		data.Windows = append(data.Windows, contract.RateLimitWindow{Name: name, UsedPct: pct(w.Utilization), ResetsAt: unixTime(w.ResetsAt)})
	}
	if len(data.Windows) == 0 && info.RateLimitType != "" && (info.Utilization != nil || info.ResetsAt != nil) {
		data.Windows = []contract.RateLimitWindow{{Name: info.RateLimitType, UsedPct: pct(info.Utilization), ResetsAt: unixTime(info.ResetsAt)}}
	}
	if data.Status == "rejected" {
		resumeAt = unixTime(info.ResetsAt)
	}
	return data, resumeAt, true
}

// rateLimitStatus is claude's status in the vocabulary a caller switches on.
func rateLimitStatus(s string) string {
	switch s {
	case "allowed":
		return "allowed"
	case "allowed_warning":
		return "warning"
	case "rejected":
		return "rejected"
	}
	return "unknown"
}

func pct(u *float64) *float64 {
	if u == nil {
		return nil
	}
	v := *u * 100
	return &v
}

func unixTime(v *float64) *time.Time {
	if v == nil || *v <= 0 {
		return nil
	}
	t := time.Unix(int64(*v), 0).UTC()
	return &t
}
