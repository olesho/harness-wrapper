package wrapper

import "strings"

// ErrorClass is the canonical, lifecycle-free taxonomy of harness-output
// errors. It is the *mechanism* half of the classification contract: the
// wrapper assigns it from harness output; downstream consumers (loomcli,
// the SDK) map it to their own *policy*. It is deliberately distinct from
// Status, which mixes runtime lifecycle (idle, waiting_for_input, stale)
// with error states.
//
// ErrorClass is additive public API consumed by multiple repos: new values
// may be appended, but existing values and their String() forms are stable.
type ErrorClass int

const (
	// ErrNone is the zero value: not an error (clean exit, waiting-for-input,
	// idle). Callers handle clean outcomes before consulting any error policy.
	ErrNone ErrorClass = iota
	ErrRateLimited     // 429 / usage|session limit / rate limit — transient, resets
	ErrAuth            // 401 / invalid key — fatal
	ErrBilling         // 402 / payment required / insufficient credits / quota exceeded — fatal
	ErrModelNotFound   // 404 / model does not exist
	ErrContextOverflow // context length / token limit exceeded (reserved; not yet emitted by built-in packs)
	ErrTimeout         // request/connection timeout / deadline exceeded
	ErrTransient       // 5xx / transport reset / temporary failure
	ErrUnknown         // unclassifiable failure
)

// String returns the canonical wire/display name. These strings are a
// stable contract consumed by downstream serializers (e.g. loom's
// daemon-agents.json last_error_class, events, checkpoints), so they match
// the long-standing names rather than the Go identifiers (ErrAuth →
// "AuthFailure", not "ErrAuth").
func (c ErrorClass) String() string {
	switch c {
	case ErrNone:
		return "None"
	case ErrRateLimited:
		return "RateLimited"
	case ErrAuth:
		return "AuthFailure"
	case ErrBilling:
		return "BillingError"
	case ErrModelNotFound:
		return "ModelNotFound"
	case ErrContextOverflow:
		return "ContextOverflow"
	case ErrTimeout:
		return "Timeout"
	case ErrTransient:
		return "Transient"
	default:
		return "Unknown"
	}
}

// classFromHTTPCode maps an upstream API status code to an ErrorClass.
// Code 0 means a transport-level error (socket closed, connection reset)
// surfaced as an api_error without a numeric code. Codes the wrapper has
// no opinion on (e.g. 400/403/422) fall through to ErrUnknown.
func classFromHTTPCode(code int) ErrorClass {
	switch {
	case code == 401:
		return ErrAuth
	case code == 402:
		return ErrBilling
	case code == 404:
		return ErrModelNotFound
	case code == 429:
		return ErrRateLimited
	case code == 408, code >= 500 && code <= 599:
		return ErrTransient
	case code == 0:
		return ErrTransient
	default:
		return ErrUnknown
	}
}

// billingHints distinguish a billing/quota failure (fatal) from a
// usage/rate limit (transient) among cost-pattern hits.
var billingHints = []string{"payment", "insufficient", "credit", "billing", "quota exceeded"}

// costClass disambiguates a cost/quota pattern hit: billing-flavored
// phrases are ErrBilling (fatal); everything else (usage/session/rate
// limits, "limit resets") is ErrRateLimited (transient, recovers).
func costClass(hit string) ErrorClass {
	h := strings.ToLower(hit)
	for _, b := range billingHints {
		if strings.Contains(h, b) {
			return ErrBilling
		}
	}
	return ErrRateLimited
}

// timeoutHints mark a retryable failure as specifically a timeout.
var timeoutHints = []string{"timeout", "timed out", "deadline exceeded", "etimedout"}

// retryClass refines a transient retry hit into ErrTimeout when the text
// names a timeout, else ErrTransient.
func retryClass(hit string) ErrorClass {
	h := strings.ToLower(hit)
	for _, t := range timeoutHints {
		if strings.Contains(h, t) {
			return ErrTimeout
		}
	}
	return ErrTransient
}
