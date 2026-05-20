// Package detector provides generic pattern primitives for harness
// classifiers. Patterns are matched against the recent harness output
// (typically the last ~64KB), with ANSI escapes already stripped.
//
// The package is intentionally minimal: it does not own state, it just
// runs string matches. Real classifiers compose these primitives with
// state from the wrapper (idle thresholds, quiet windows).
package detector

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Patterns groups the per-harness fingerprints a classifier consults.
// All slices are matched as case-insensitive substrings against
// already-stripped, lower-cased recent output, except Prompt which is
// matched against the trailing line of the original (case-preserved)
// stripped output.
type Patterns struct {
	// Cost matches messages indicating the harness has hit a budget,
	// quota, or rate limit and cannot proceed without operator
	// intervention.
	Cost []string

	// Retry matches messages indicating a transient failure that the
	// engine should retry after a backoff.
	Retry []string

	// Prompt matches trailing-line fingerprints (e.g. "(y/N)") that
	// indicate the harness is waiting for keyboard input.
	Prompt []string

	// APIError parses high-confidence upstream API error markers out of
	// the harness's stripped output. Each harness formats API errors
	// differently (Claude: "API Error: <code> ...", Gemini: "[API
	// Error: ... (Status: <code>)]", Codex: phrase-based), so the
	// matcher is supplied per-harness rather than a single shared
	// regex. nil means "no API-error detection for this harness".
	APIError APIErrorMatcher

	// SessionLimit parses high-confidence "session/usage limit reached"
	// banners that include a wall-clock reset time (e.g. Claude Code's
	// "You've hit your session limit · resets 6:40pm (Europe/Warsaw)").
	// Unlike Cost which only fires after the run has been idle for the
	// classify threshold, SessionLimit is anchored enough on the
	// rendering to fire as soon as the banner appears. nil means "no
	// session-limit detection for this harness".
	SessionLimit SessionLimitMatcher
}

// APIErrorHit is what an APIErrorMatcher returns when it recognizes an
// upstream API error in the harness's output.
type APIErrorHit struct {
	// Code is the HTTP status code parsed from the message. Zero when
	// the harness's output did not include a numeric code (e.g.
	// transport-layer failures like "socket connection closed
	// unexpectedly") or when the matcher recognized a phrase whose
	// code is implicit.
	Code int

	// Message is the human-readable detail extracted from the matched
	// line, used to populate the wrapper event's Reason.
	Message string

	// RetryAfter is the wait duration the harness suggested in the
	// error text (e.g. "Retry after 30 seconds"). Zero when the
	// message contained no parseable hint.
	RetryAfter time.Duration
}

// APIErrorMatcher inspects already-ANSI-stripped recent output and
// reports whether it contains a recognized upstream API error.
type APIErrorMatcher func(stripped string) (APIErrorHit, bool)

// SessionLimitHit is what a SessionLimitMatcher returns when it
// recognizes a session-limit banner in the harness's output.
type SessionLimitHit struct {
	// Message is the human-readable detail extracted from the matched
	// banner, used to populate the wrapper event's Reason.
	Message string

	// ResumeAt is the absolute wall-clock time at which the limit is
	// expected to reset. Resolved into the IANA location embedded in
	// the banner (or the caller's `now` location when the banner did
	// not name one). Zero when no parseable reset time was found.
	ResumeAt time.Time
}

// SessionLimitMatcher inspects already-ANSI-stripped recent output and
// reports whether it contains a recognized session-limit banner. The
// `now` parameter anchors relative time math (e.g. choosing today vs.
// tomorrow for a "resets 6:40pm" hint); pass time.Now() in production
// code and a fixed clock in tests.
type SessionLimitMatcher func(stripped string, now time.Time) (SessionLimitHit, bool)

// retryAfterRE captures "in N seconds", "in N minutes", "after N
// seconds", "after Ns", etc. The unit group is optional; a bare number
// is not matched because the surrounding text ("retry after 5") is
// ambiguous without a unit, and matchers should prefer no hint over a
// guessed one.
var retryAfterRE = regexp.MustCompile(`(?i)(?:try\s+again|retry)[^.\n]*?(?:in|after)\s+(\d+)\s*(s\b|sec|second|m\b|min|minute|h\b|hr|hour)s?`)

// ParseRetryAfter scans an API-error message for a numeric retry hint
// and returns it as a time.Duration. Returns zero when no hint was
// found or when the unit could not be recognized.
//
// Recognized phrasings include "try again in 30 seconds", "retry after
// 2 minutes", "try again in 5s". Non-numeric phrasings like "try again
// in a moment" return zero — better to surface "no hint" than guess.
func ParseRetryAfter(msg string) time.Duration {
	m := retryAfterRE.FindStringSubmatch(msg)
	if len(m) < 3 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0
	}
	switch strings.ToLower(m[2]) {
	case "s", "sec", "second":
		return time.Duration(n) * time.Second
	case "m", "min", "minute":
		return time.Duration(n) * time.Minute
	case "h", "hr", "hour":
		return time.Duration(n) * time.Hour
	}
	return 0
}

// resetTimeRE captures the "resets <clock-time> (<TZ>)" tail of a
// session-limit banner. The clock-time accepts:
//   - 12-hour: "6pm", "6:40pm", "6:40 PM" (am/pm optional space, any case)
//   - 24-hour: "18:40"
//
// The TZ group is optional and accepts an IANA identifier in parens
// (e.g. "(Europe/Warsaw)") or a short label like "(UTC)". When the
// banner does not name a TZ, callers get the time resolved in the
// `now` location.
var resetTimeRE = regexp.MustCompile(`(?i)resets?(?:\s+at)?\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?(?:\s*\(([^)]+)\))?`)

// ParseResetTime scans text for a "resets HH:MM(am|pm) (TZ)" hint and
// returns the next future absolute time at which the limit is expected
// to reset. The TZ portion is optional; when present and recognized as
// an IANA location, the returned time carries that location. When
// absent (or unrecognized), the time resolves in `now`'s location.
//
// "Future" is computed relative to `now`: if the parsed clock-time has
// already passed today, the returned time rolls to tomorrow. This
// matches how the banners are typically rendered — Claude Code prints
// the *next* reset, not a past one.
//
// Returns the zero time and false when no parseable hint was found.
func ParseResetTime(text string, now time.Time) (time.Time, bool) {
	m := resetTimeRE.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(m[1])
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, false
	}
	minute := 0
	if m[2] != "" {
		minute, err = strconv.Atoi(m[2])
		if err != nil || minute < 0 || minute > 59 {
			return time.Time{}, false
		}
	}
	ampm := strings.ToLower(m[3])
	switch ampm {
	case "am":
		if hour < 1 || hour > 12 {
			return time.Time{}, false
		}
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour < 1 || hour > 12 {
			return time.Time{}, false
		}
		if hour != 12 {
			hour += 12
		}
	default:
		// 24-hour form (no am/pm). Reject single-digit hours without a
		// minute component — "resets 6" is more likely a false positive
		// than a 6:00 wall-clock.
		if m[2] == "" {
			return time.Time{}, false
		}
		if hour > 23 {
			return time.Time{}, false
		}
	}
	loc := now.Location()
	if tz := strings.TrimSpace(m[4]); tz != "" {
		if parsed, err := time.LoadLocation(tz); err == nil {
			loc = parsed
		}
	}
	nowInLoc := now.In(loc)
	resume := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), hour, minute, 0, 0, loc)
	if !resume.After(now) {
		resume = resume.Add(24 * time.Hour)
	}
	return resume, true
}

// MatchAny returns the first pattern in patterns that appears as a
// substring of haystack, or "" if none match. Caller is expected to
// pre-lowercase haystack.
func MatchAny(haystack string, patterns []string) string {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.Contains(haystack, p) {
			return p
		}
	}
	return ""
}

// MatchPromptSuffix returns the first pattern in patterns that the
// trailing non-empty line of haystack ends with (case-insensitive),
// or "" if none match. Trailing whitespace on the last line is
// ignored so prompts ending with a space ("Continue? ") still match.
func MatchPromptSuffix(haystack string, patterns []string) string {
	tail := lastNonEmptyLine(haystack)
	if tail == "" {
		return ""
	}
	tailLower := strings.ToLower(strings.TrimRight(tail, " \t"))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.HasSuffix(tailLower, strings.ToLower(p)) {
			return p
		}
	}
	return ""
}

func lastNonEmptyLine(s string) string {
	end := len(s)
	for end > 0 {
		// Trim a trailing newline / space block.
		for end > 0 && (s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == ' ' || s[end-1] == '\t') {
			end--
		}
		start := end
		for start > 0 && s[start-1] != '\n' {
			start--
		}
		line := s[start:end]
		if line != "" {
			return line
		}
		if start == 0 {
			return ""
		}
		end = start - 1
	}
	return ""
}
