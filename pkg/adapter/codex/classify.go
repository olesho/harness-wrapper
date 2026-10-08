package codex

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// turnError is codex's account of a failed turn or model call: its message
// and codexErrorInfo, which is a name (usageLimitExceeded, …) or an object
// named by its one key ({"responseStreamDisconnected": {"httpStatusCode":
// 529}}).
type turnError struct {
	Message        string          `json:"message"`
	CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
}

// info names the error and the HTTP status it carries, when it does. The
// rollout writes the same names in snake case (usage_limit_exceeded), which
// info reads as their camel-case selves.
func errorInfo(raw json.RawMessage) (name string, status int) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return camel(s), 0
	}
	var m map[string]struct {
		HTTPStatusCode *int `json:"httpStatusCode"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return "", 0
	}
	for k, v := range m {
		if v.HTTPStatusCode != nil {
			status = *v.HTTPStatusCode
		}
		return camel(k), status
	}
	return "", 0
}

// camel is a snake_case name in camelCase.
func camel(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// errorClass classifies an error by its codexErrorInfo name.
func errorClass(name string) contract.ErrorClass {
	switch name {
	case "usageLimitExceeded":
		return contract.ErrorUsageLimit
	case "serverOverloaded":
		return contract.ErrorOverloaded
	case "unauthorized":
		return contract.ErrorAuth
	case "sandboxError", "threadRollbackFailed":
		return contract.ErrorInternal
	case "":
		return contract.ErrorInternal
	}
	// internalServerError, badRequest, contextWindowExceeded, the stream's and
	// the connection's failures, other, …
	return contract.ErrorAPI
}

// failure is the contract's account of a failed turn: its class, its HTTP
// status, and — at a usage wall — when the account can work again, from the
// latest rate-limit report, or else from the message's "try again at 9:58
// PM". That is a time of day in the Host's zone, whatever now's own: the
// live transport's now is local, a record's entry time UTC.
func failure(e turnError, limits *rateLimits, now time.Time) *contract.TurnError {
	name, status := errorInfo(e.CodexErrorInfo)
	te := &contract.TurnError{Class: errorClass(name), HTTPStatus: status}
	if te.Class == contract.ErrorUsageLimit {
		if at := limits.resumeAt(); at != nil {
			te.ResumeAt = at
		} else if at, ok := tryAgainAt(e.Message, now.In(time.Local)); ok {
			at = at.UTC()
			te.ResumeAt = &at
		}
	}
	return te
}

// tryAgain is a usage wall's message: "… or try again at 9:58 PM.", a time
// of day in codex's own time zone, the Host's.
var tryAgain = regexp.MustCompile(`(?i)try again at (\d{1,2}):(\d{2})\s*([ap])\.?m\b`)

// tryAgainAt is the next time the message's time of day comes, after now.
func tryAgainAt(msg string, now time.Time) (time.Time, bool) {
	m := tryAgain.FindStringSubmatch(msg)
	if m == nil {
		return time.Time{}, false
	}
	h, _ := strconv.Atoi(m[1])
	mins, _ := strconv.Atoi(m[2])
	if h < 1 || h > 12 || mins > 59 {
		return time.Time{}, false
	}
	h %= 12
	if strings.EqualFold(m[3], "p") {
		h += 12
	}
	at := time.Date(now.Year(), now.Month(), now.Day(), h, mins, 0, 0, now.Location())
	if !at.After(now) {
		at = at.AddDate(0, 0, 1)
	}
	return at, true
}

// rateLimits is an account/rateLimits/updated's snapshot.
type rateLimits struct {
	LimitID   string           `json:"limitId"`
	Primary   *rateLimitWindow `json:"primary"`
	Secondary *rateLimitWindow `json:"secondary"`
}

type rateLimitWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int64   `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"` // Unix seconds
}

// full reports a window used up: the account refused until it resets.
func (w *rateLimitWindow) full() bool {
	return w != nil && w.UsedPercent != nil && *w.UsedPercent >= 100
}

func (w *rateLimitWindow) resets() *time.Time {
	if w == nil || w.ResetsAt == nil || *w.ResetsAt <= 0 {
		return nil
	}
	t := time.Unix(*w.ResetsAt, 0).UTC()
	return &t
}

// resumeAt is when a full window lets the account work again: the latest of
// the full windows' resets.
func (r *rateLimits) resumeAt() *time.Time {
	if r == nil {
		return nil
	}
	var at *time.Time
	for _, w := range []*rateLimitWindow{r.Primary, r.Secondary} {
		if t := w.resets(); w.full() && t != nil && (at == nil || t.After(*at)) {
			at = t
		}
	}
	return at
}

// data is the rate_limit observation's payload: a window per reported one,
// named primary and secondary, and rejected when one is used up.
func (r *rateLimits) data() contract.RateLimitData {
	d := contract.RateLimitData{Status: "allowed"}
	for _, w := range []struct {
		name string
		w    *rateLimitWindow
	}{{"primary", r.Primary}, {"secondary", r.Secondary}} {
		if w.w == nil {
			continue
		}
		if w.w.full() {
			d.Status = "rejected"
		}
		d.Windows = append(d.Windows, contract.RateLimitWindow{Name: w.name, UsedPct: w.w.UsedPercent, ResetsAt: w.w.resets()})
	}
	if len(d.Windows) == 0 {
		d.Status = "unknown"
	}
	return d
}

// reconnecting is codex's retry notice: "Reconnecting... 1/2".
var reconnecting = regexp.MustCompile(`(\d+)/(\d+)`)

// retry is a retry notice's attempt and its limit, and the HTTP status of
// the failure retried, when the notice says.
func retry(e turnError) contract.RetryingData {
	var d contract.RetryingData
	if m := reconnecting.FindStringSubmatch(e.Message); m != nil {
		d.Attempt, _ = strconv.Atoi(m[1])
		d.Max, _ = strconv.Atoi(m[2])
	}
	_, d.HTTPStatus = errorInfo(e.CodexErrorInfo)
	return d
}
