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
	ResetsAt        *float64 `json:"resets_at"`
	ResetsInSeconds *float64 `json:"resets_in_seconds"`
}

type resetBody struct {
	resetFields
	Error *resetFields `json:"error"`
}

// resetsOf decodes the JSON body embedded in an error text. It reports no
// fields when the text holds no decodable object.
func resetsOf(text string) resetFields {
	i := strings.IndexByte(text, '{')
	if i < 0 {
		return resetFields{}
	}
	var b resetBody
	// A Decoder reads one value and ignores any text after it.
	if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&b); err != nil {
		return resetFields{}
	}
	f := b.resetFields
	if b.Error != nil {
		if f.ResetsAt == nil {
			f.ResetsAt = b.Error.ResetsAt
		}
		if f.ResetsInSeconds == nil {
			f.ResetsInSeconds = b.Error.ResetsInSeconds
		}
	}
	return f
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
	if f.ResetsAt != nil && *f.ResetsAt > 0 {
		at := time.Unix(int64(*f.ResetsAt), 0).UTC()
		e.ResumeAt = &at
		return e
	}
	if f.ResetsInSeconds != nil && *f.ResetsInSeconds > 0 {
		at := now.Add(time.Duration(*f.ResetsInSeconds * float64(time.Second))).UTC()
		e.ResumeAt = &at
	}
	return e
}

// final reports whether pi retries no failed call of this class: it retries
// rate limits, overloads, server errors and usage walls, and gives up on a
// key it was refused and on billing.
func final(class contract.ErrorClass) bool {
	return class == contract.ErrorAuth || class == contract.ErrorBilling || class == contract.ErrorMaxOutput || class == contract.ErrorInternal
}
