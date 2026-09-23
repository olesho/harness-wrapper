// Package resettime reads the reset time out of a harness's usage-limit wall
// ("You've hit your session limit · resets 6:40pm (Europe/Warsaw)"). It is
// shared by the wrapper's session-limit matcher and the chat layer's turn
// relabels, so a wall reports the same reset time through every surface.
package resettime

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

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

// Parse scans text for a "resets HH:MM(am|pm) (TZ)" hint and
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
func Parse(text string, now time.Time) (time.Time, bool) {
	m := resetTimeRE.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	hour, minute, ok := parseResetClock(m)
	if !ok {
		return time.Time{}, false
	}
	loc := resetLocation(m[4], now.Location())
	nowInLoc := now.In(loc)
	resume := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), hour, minute, 0, 0, loc)
	if !resume.After(now) {
		resume = resume.Add(24 * time.Hour)
	}
	return resume, true
}

// parseResetClock extracts and validates the hour/minute from a
// resetTimeRE match (groups: 1=hour, 2=minute, 3=am/pm). The bool is
// false when the components are out of range or fail the am/pm rules.
func parseResetClock(m []string) (int, int, bool) {
	hour, err := strconv.Atoi(m[1])
	if err != nil || hour < 0 || hour > 23 {
		return 0, 0, false
	}
	minute := 0
	if m[2] != "" {
		minute, err = strconv.Atoi(m[2])
		if err != nil || minute < 0 || minute > 59 {
			return 0, 0, false
		}
	}
	hour, ok := applyMeridiem(hour, strings.ToLower(m[3]), m[2])
	if !ok {
		return 0, 0, false
	}
	return hour, minute, true
}

// applyMeridiem converts a 12-hour clock hour to 24-hour form per the
// am/pm marker, or validates the bare 24-hour form when no marker is
// present. minuteGroup is the raw minute submatch (empty when the banner
// had no ":MM"). The bool is false when the hour is invalid for the form.
func applyMeridiem(hour int, ampm, minuteGroup string) (int, bool) {
	switch ampm {
	case "am":
		if hour < 1 || hour > 12 {
			return 0, false
		}
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour < 1 || hour > 12 {
			return 0, false
		}
		if hour != 12 {
			hour += 12
		}
	default:
		// 24-hour form (no am/pm). Reject single-digit hours without a
		// minute component — "resets 6" is more likely a false positive
		// than a 6:00 wall-clock.
		if minuteGroup == "" {
			return 0, false
		}
		if hour > 23 {
			return 0, false
		}
	}
	return hour, true
}

// resetLocation resolves the optional IANA timezone captured from a
// session-limit banner, falling back to fallback when the banner named
// no zone or the zone is unrecognized.
func resetLocation(tzGroup string, fallback *time.Location) *time.Location {
	if tz := strings.TrimSpace(tzGroup); tz != "" {
		if parsed, err := time.LoadLocation(tz); err == nil {
			return parsed
		}
	}
	return fallback
}
