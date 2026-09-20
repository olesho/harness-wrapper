package wrapper

import (
	"regexp"
	"strconv"
	"time"
)

// ClassifyFinishedOutput classifies the output of a harness that has ALREADY
// EXITED, applying the same classifier ClassifyOutput does and then a residual
// fallback for the signals the per-harness anchored matchers miss.
//
// It is the post-exit entry point. ClassifyOutput remains the plain one-shot:
// same classifier, no fallback, results unchanged for every caller. The split
// exists because the residual rows below are the broadest patterns in the
// library — `\bbilling\b`, `\bquota\b`, API-key variable names — and sharing
// them with the live polling dispatcher would let an agent that merely PRINTS
// such a word terminate its own quiet, healthy process. Post-exit there is no
// process left to terminate, so breadth costs a misclassification at worst.
//
// Order:
//  1. The resolved classifier (custom override → per-harness adapter →
//     default), exactly as ClassifyOutput runs it.
//  2. An actionable result is returned unchanged — including
//     StatusBinaryNotFound, which is a statement about the launch, not the
//     output.
//  3. Only on ErrNone / ErrUnknown — "nothing actionable" — are the residual
//     rows consulted. A hit REPLACES the result rather than decorating it:
//     the rows are a different fingerprint of the same text, not a refinement
//     of a verdict the classifier did not reach.
//  4. An ErrTransient result whose surrounding text names a timeout is
//     refined to ErrTimeout, which keeps a network timeout in its own class
//     (and its own backoff bucket downstream) instead of a generic 5xx.
//  5. A rate-limited result with no wait hint gets one from a Retry-After
//     token anywhere in the output. The per-harness matchers only parse the
//     hint when it sits inside the message they anchored on; a CLI that
//     prints the header on its own line is the common case, and a caller
//     that has to scrape it itself is maintaining a harness-output pattern
//     outside the repository that owns them.
//
// Returns the classifier's own result when nothing matches, so a caller's
// exit-code fallback still applies.
func ClassifyFinishedOutput(harness, output string) Classification {
	c := ClassifyOutput(harness, output)
	if c.Status == StatusBinaryNotFound {
		return c
	}
	switch c.Class {
	case ErrNone, ErrUnknown:
		if r, ok := matchResidual(output); ok {
			return r
		}
	case ErrTransient:
		if loc := finishedTimeoutRe.FindStringIndex(output); loc != nil {
			c.Class = ErrTimeout
			c.Rule = RuleTimeoutUpgrade
			c.Match = output[loc[0]:loc[1]]
			return c
		}
	case ErrRateLimited:
		if c.RetryAfter == 0 {
			c.RetryAfter = parseRetryAfterSeconds(output)
		}
	}
	return c
}

// RuleTimeoutUpgrade is the Classification.Rule stamped on an ErrTransient
// result that ClassifyFinishedOutput refined to ErrTimeout.
//
// The "wrapper/" prefix is not a package name — it is the id loom already
// records for this rewrite in its evidence log (`source=wrapper_classifier
// rule=wrapper/timeout_upgrade`), which is what an operator reading a verdict
// sees. The rewrite moved here; the id stays what it was, because renaming it
// would silently change every record that names it.
const RuleTimeoutUpgrade = "wrapper/timeout_upgrade"

// finishedTimeoutRe recognizes timeout-worded errors anywhere in the output.
// retryClass refines a retry HIT by the matched phrase alone; this looks at
// the surrounding text, so a bare "socket hang up" landing next to
// "context deadline exceeded" is still a timeout.
var finishedTimeoutRe = regexp.MustCompile(`(?i)\btimeout\b|etimedout|connection.?timed?.?out|timed?.?out|deadline.?exceeded`)

// residualRow is one regex→class row of the residual table.
type residualRow struct {
	// id is a STABLE CONTRACT — see Classification.Rule.
	id     string
	re     *regexp.Regexp
	class  ErrorClass
	reason string
}

// residualRows is the backend-agnostic fallback table, consulted only by
// ClassifyFinishedOutput and only when the resolved classifier returned
// nothing actionable.
//
// It encodes the distinctions a consumer acts on that the per-harness packs do
// not model — auth, billing, model-not-found, context overflow, timeout — plus
// the bare numeric / timing / prose signals the anchored matchers miss (a bare
// 429 from an unknown harness, "try again at <time>"). Any overlap with the
// per-harness cost/transport patterns is dead-but-safe: those matched first.
//
// Ordered RateLimited-first, Transient-last. The order IS the precedence: text
// naming both a quota and a 500 is a rate limit, because that is the verdict
// that recovers on its own.
//
// These rows are deliberately UNCHANGED from the table they were moved out of
// (loom's internal/agenterr), so the move could be tested differentially.
// Narrowing them — `residual.billing` matches a bare "billing"; `residual.auth`
// matches API-key VARIABLE NAMES — is the follow-up this move makes possible,
// not part of it.
var residualRows = []residualRow{
	{"residual.ratelimit", regexp.MustCompile(`(?i)\b429\b|too many requests|tokens per min|overloaded_error|resource.?exhausted|resource_exhausted|rate.?limit|usage.?limit|session.?limit|resets at|resets \d{1,2}:\d{2}|try again at\s+\d`), ErrRateLimited, "rate limit exceeded"},
	{"residual.auth", regexp.MustCompile(`(?i)\b401\b|unauthorized|unauthenticated|permission.?denied|forbidden|invalid.?api.?key|incorrect.?api.?key|invalid.*key|authentication.?failed|ANTHROPIC_API_KEY|OPENAI_API_KEY|GEMINI_API_KEY|GOOGLE_API_KEY|CURSOR_API_KEY`), ErrAuth, "authentication failed"},
	{"residual.billing", regexp.MustCompile(`(?i)\b402\b|payment.?required|insufficient.?(?:credits|quota)|insufficient_quota|exceeded.*quota|quota.?exceeded|\bquota\b|\bcredits\b|\bbilling\b`), ErrBilling, "billing error"},
	{"residual.model_version", regexp.MustCompile(`(?i)model requires a newer version|requires a newer version of (?:codex|claude)|upgrade to the latest (?:app or )?cli`), ErrModelNotFound, "backend CLI is incompatible with the selected model"},
	{"residual.model_not_found", regexp.MustCompile(`(?i)model.?not.?found|model.*not found|model.*does not exist|model.*not.*exist|model_not_found|unsupported.?model|unknown.?model|invalid.?model|selected model.*may not exist|selected model.*may not have access to it|\b404\b.*model`), ErrModelNotFound, "model not found"},
	{"residual.context", regexp.MustCompile(`(?i)context.?length|context.?window|context_length_exceeded|maximum context length|max.?tokens|max.*tokens|token.?limit|prompt.?too.?long|too.?long`), ErrContextOverflow, "context length exceeded"},
	{"residual.timeout", regexp.MustCompile(`(?i)\btimeout\b|etimedout|connection.?timed?.?out|timed?.?out|deadline.?exceeded`), ErrTimeout, "connection timeout"},
	{"residual.transient", regexp.MustCompile(`(?i)\b50[023]\b|\b529\b|server.?error|server_error|internal.?server.?error|internal.?error|service.?unavailable|backend.?error|overloaded`), ErrTransient, "server error"},
}

// matchResidual runs the residual rows in order and builds a FRESH
// Classification for the first usable hit — it never inherits Status, HTTPCode
// or ResumeAt from the result it replaces. A residual row is a text
// fingerprint, not a harness lifecycle state, so Status stays empty and Rule is
// what a caller discriminates on.
func matchResidual(output string) (Classification, bool) {
	if output == "" {
		return Classification{}, false
	}
	for _, r := range residualRows {
		loc := firstUsableMatch(r.re, output)
		if loc == nil {
			continue
		}
		c := Classification{Class: r.class, Reason: r.reason, Rule: r.id, Match: output[loc[0]:loc[1]]}
		if r.class == ErrRateLimited {
			c.RetryAfter = parseRetryAfterSeconds(output)
		}
		return c, true
	}
	return Classification{}, false
}

// firstUsableMatch returns the first match of re in s that survives
// embeddedNumber, or nil when every match is one.
//
// It walks ALL matches rather than testing only the first, because the digits
// that fooled us are the ones most likely to appear early: a log tail opens
// with its timestamp and the real error arrives at the end.
//
// FindAllStringIndex is used on the WHOLE string on purpose — re-matching
// against a slice would move the `\b` anchors, and a boundary that only exists
// because of where the slice started is exactly the kind of false match this
// function exists to reject.
func firstUsableMatch(re *regexp.Regexp, s string) []int {
	for _, loc := range re.FindAllStringIndex(s, -1) {
		if !embeddedNumber(s, loc[0], loc[1]) {
			return loc
		}
	}
	return nil
}

// embeddedNumber reports whether an all-digit match at [lo,hi) is a fragment of
// a larger number rather than a status code standing on its own.
//
// This exists because of a measured incident. On 2026-09-11 a loom agent's log
// tail began `time=2026-09-11T17:08:17.402+02:00`; residual.billing's `\b402\b`
// matched the MILLISECOND FIELD, the turn was classified ErrBilling, the agent
// was stopped fatally and an account-wide wall parked five more agents for
// fifteen minutes. The turn's actual failure was a swallowed prompt. The same
// hazard covers 7 of the 10 ordinary millisecond values, two of them fatal
// (`.401` → ErrAuth, `.402` → ErrBilling).
//
// A digit can never directly precede a `\b`-anchored numeric match — digits are
// word characters, so there would be no boundary — which leaves a separator,
// and only one separator turns a token into a fragment: the decimal point. A
// match preceded by '.', or followed by '.' and another digit, is part of a
// number (a millisecond field, a version, a decimal) and says nothing about
// HTTP. Everything else — `429`, `Error: 401`, `HTTP 403`, `upstream returned
// 429` — is untouched.
func embeddedNumber(s string, lo, hi int) bool {
	for i := lo; i < hi; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false // not a bare numeric token; the word rows are unaffected
		}
	}
	if lo > 0 && s[lo-1] == '.' {
		return true // 17:08:17.402 — a millisecond field or a decimal fraction
	}
	if hi+1 < len(s) && s[hi] == '.' && s[hi+1] >= '0' && s[hi+1] <= '9' {
		return true // 402.5 — the integer part of a decimal
	}
	return false
}

// retryAfterRe extracts a Retry-After header value from log/output text. The
// format is provider-independent, which is why it is matched over the whole
// blob rather than per-harness.
var retryAfterRe = regexp.MustCompile(`(?i)retry.?after[:\s]+(\d+)`)

// parseRetryAfterSeconds extracts a "retry-after: N" value (seconds) from
// text. Zero when absent or unparseable.
func parseRetryAfterSeconds(text string) time.Duration {
	m := retryAfterRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return 0
	}
	secs, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return time.Duration(secs) * time.Second
}
