package pi

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// pi records a failed model call as text alone, the provider's own error
// inside it (probes/pirpc): Anthropic's API reads "<status> {json}", with the
// error's type; OpenAI's "OpenAI API error (<status>): {json}", or
// "<code>: <message>". The class is read from that text.

// statusRE finds the HTTP status an error text leads with.
var statusRE = regexp.MustCompile(`^(?:[A-Za-z]+ API error \()?([1-5][0-9][0-9])\b`)

// httpStatus is the HTTP status an error text leads with, or 0.
func httpStatus(text string) int {
	m := statusRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// errorClass classifies a failed call by its text and status. A usage wall is
// OpenAI's usage_limit_reached, which says when it resets; a 429 that is a
// rate limit — Anthropic's rate_limit_error — is the API's own, which pi has
// retried already. A call pi aborted itself, unasked, is its own failure.
func errorClass(text string, status int) contract.ErrorClass {
	t := strings.ToLower(text)
	switch {
	case text == "", text == abortedMessage:
		return contract.ErrorInternal
	case strings.Contains(t, "usage_limit_reached"):
		return contract.ErrorUsageLimit
	case strings.Contains(t, "insufficient_quota"), strings.Contains(t, "credit balance is too low"):
		return contract.ErrorBilling
	case status == 401, status == 403, strings.Contains(t, "authentication_error"), strings.Contains(t, "permission_error"),
		strings.Contains(t, "invalid_api_key"), strings.Contains(t, "invalid x-api-key"), strings.Contains(t, "no api key"):
		return contract.ErrorAuth
	case status == 529, strings.Contains(t, "overloaded"):
		return contract.ErrorOverloaded
	}
	return contract.ErrorAPI
}

// resetFields are the usage wall's reset times in the provider's JSON body:
// OpenAI's resets_at (seconds since the epoch) or resets_in_seconds. Either
// may sit at the top of the body or inside an "error" object.
type resetFields struct {
	ResetsAt        *float64
	ResetsInSeconds *float64
}

// maxResetHorizon bounds a reported reset time: a provider value further out
// than this (a millisecond epoch, say) is not a usable wait.
const maxResetHorizon = 365 * 24 * time.Hour

// number reads a JSON number, or nil for anything else, so one field of the
// wrong type costs only itself.
func number(raw json.RawMessage) *float64 {
	var n float64
	if json.Unmarshal(raw, &n) != nil {
		return nil
	}
	return &n
}

// fill sets the fields of f still unset from the object obj, if it is one.
func (f *resetFields) fill(obj map[string]json.RawMessage) {
	if f.ResetsAt == nil {
		f.ResetsAt = number(obj["resets_at"])
	}
	if f.ResetsInSeconds == nil {
		f.ResetsInSeconds = number(obj["resets_in_seconds"])
	}
}

// resetsOf decodes the JSON body embedded in an error text, trying each "{"
// in turn so a brace in the text before the body does not hide it. It reports
// no fields when no object in the text names a reset time.
func resetsOf(text string) resetFields {
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		var obj map[string]json.RawMessage
		// A Decoder reads one value and ignores any text after it.
		if json.NewDecoder(strings.NewReader(text[i:])).Decode(&obj) != nil {
			continue
		}
		var f resetFields
		f.fill(obj)
		var inner map[string]json.RawMessage
		if json.Unmarshal(obj["error"], &inner) == nil {
			f.fill(inner)
		}
		if f.ResetsAt != nil || f.ResetsInSeconds != nil {
			return f
		}
	}
	return resetFields{}
}

// failure is the contract's account of a failed run, from the error text pi
// recorded: its class, its HTTP status, and — at a usage wall — when the
// account can work again.
func failure(text string, now time.Time) *contract.TurnError {
	status := httpStatus(text)
	e := &contract.TurnError{Class: errorClass(text, status), HTTPStatus: status}
	if e.Class != contract.ErrorUsageLimit {
		return e
	}
	f := resetsOf(text)
	if at := f.ResetsAt; at != nil && *at > 0 && *at <= float64(now.Add(maxResetHorizon).Unix()) {
		t := time.Unix(int64(*at), 0).UTC()
		e.ResumeAt = &t
		return e
	}
	if in := f.ResetsInSeconds; in != nil && *in > 0 && *in <= maxResetHorizon.Seconds() {
		t := now.Add(time.Duration(*in * float64(time.Second))).UTC()
		e.ResumeAt = &t
	}
	return e
}

// final reports whether pi retries no failed call of this class: it retries
// rate limits, overloads, server errors and usage walls, and gives up on a
// key it was refused and on billing.
func final(class contract.ErrorClass) bool {
	return class == contract.ErrorAuth || class == contract.ErrorBilling || class == contract.ErrorMaxOutput || class == contract.ErrorInternal
}
