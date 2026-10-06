package pi

import (
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

// resetsRE finds when a usage wall resets: OpenAI's resets_at (seconds since
// the epoch) or resets_in_seconds.
var (
	resetsAtRE = regexp.MustCompile(`"resets_at"\s*:\s*([0-9]+)`)
	resetsInRE = regexp.MustCompile(`"resets_in_seconds"\s*:\s*([0-9]+)`)
)

// failure is the contract's account of a failed run, from the error text pi
// recorded: its class, its HTTP status, and — at a usage wall — when the
// account can work again.
func failure(text string, now time.Time) *contract.TurnError {
	status := httpStatus(text)
	e := &contract.TurnError{Class: errorClass(text, status), HTTPStatus: status}
	if e.Class != contract.ErrorUsageLimit {
		return e
	}
	if m := resetsAtRE.FindStringSubmatch(text); m != nil {
		if s, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			at := time.Unix(s, 0).UTC()
			e.ResumeAt = &at
			return e
		}
	}
	if m := resetsInRE.FindStringSubmatch(text); m != nil {
		if s, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			at := now.Add(time.Duration(s) * time.Second).UTC()
			e.ResumeAt = &at
		}
	}
	return e
}

// final reports whether pi retries no failed call of this class: it retries
// rate limits, overloads, server errors and usage walls, and gives up on a
// key it was refused and on billing.
func final(class contract.ErrorClass) bool {
	return class == contract.ErrorAuth || class == contract.ErrorBilling || class == contract.ErrorMaxOutput || class == contract.ErrorInternal
}
