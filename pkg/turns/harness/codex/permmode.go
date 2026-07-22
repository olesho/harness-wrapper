package codex

import (
	"regexp"
	"strings"
)

// horizontalSpace matches one space-like character that is NOT a line break, so
// the (?m)-anchored pattern below stays on its own screen row. Mirrors the
// repo-wide idiom (pkg/chat/ready.go, pkg/turns/harness/claudecode/permmode.go)
// and the `[^\S\r\n]*` runs already used by menuRE and promptRE in this package.
//
// It matters more than it looks: codex paints the composer's hint row by
// right-aligning the marker with column jumps and SGR sequences rather than a
// literal run of spaces, so the width of the gap that survives into the
// rendered snapshot is an emulator artifact the parser may not depend on.
const horizontalSpace = `[^\S\r\n]`

// Collaboration-axis values reported by collaborationMode. These are NOT
// permission rungs (wrapper.PermissionRungs()) — see the doc comment on
// (*Adapter).PermissionMode for the axis distinction.
const (
	collabPlan    = "plan"
	collabDefault = "default"
)

// collaborationPlanRE matches codex's collaboration-mode footer marker, which
// codex paints ONLY while the mode is Plan, right-aligned on the composer's
// hint row (codex-cli 0.14x):
//
//	Plan mode (shift+tab to cycle)
//
// Four deliberate shape decisions:
//
//  1. The trailing "(shift+tab to cycle)" hint is OPTIONAL. Codex swaps the
//     tail of that parenthetical out as the composer's hint row changes (the
//     same way Claude's footer swaps "· esc to interrupt" in while it works),
//     so the marker must survive with the hint absent.
//
//  2. The match is CLOSED — the literal words "Plan mode", not a generic
//     "<words> mode" capture. A renamed or newly-added codex collaboration
//     mode therefore fails to match and degrades to the default reading rather
//     than to a wrong axis value.
//
//  3. No `$` anchor. pkg/screen preserves per-row trailing whitespace
//     (pkg/screen/screen.go:24-26), so every row is right-padded to the
//     terminal width and a `$` immediately after the marker could never match.
//     The row's end is instead reached by consuming the padding run and then
//     the line break (or end of text).
//
//  4. The marker must sit in the row's RIGHT-ALIGNMENT GUTTER: preceded by the
//     start of the row or by a run of two or more horizontal spaces, and
//     followed by nothing but padding to the end of the row. That is what
//     separates the footer marker from the same words appearing in ordinary
//     reply prose ("I'll stay in Plan mode until you say otherwise."), which
//     is single-spaced from its neighbours and continues past the phrase. The
//     capital "P" is load-bearing for the same reason and is deliberately not
//     case-folded.
var collaborationPlanRE = regexp.MustCompile(spaced(
	`(?m)(?:^[^\S\r\n]*|[^\S\r\n]{2,})Plan mode(?: \(shift\+tab to cycle\))?[^\S\r\n]*(?:\r|\n|\z)`,
))

// spaced rewrites every literal space in pat into "one or more horizontal
// space characters", so the pattern stays readable while still tolerating the
// emulator's column-jump spacing at every inter-token gap. Mirrors
// claudecode.spaced.
func spaced(pat string) string {
	return strings.ReplaceAll(pat, " ", horizontalSpace+`+`)
}

// collaborationMode reads codex's COLLABORATION-axis posture off the rendered
// screen text and returns "plan" or "default".
//
// Codex paints a marker only while the mode is Plan; the default mode paints no
// marker at all, so ABSENCE is the signal for "default" — not for "unknown".
//
// ("", false) is reserved for a screen that carries no readable signal at all,
// and the readability rule is codex's own composer signal, the same one the
// chat layer's readiness gate uses for this harness (pkg/chat/ready.go's codex
// arm): a screen is readable when it is not a logged-out sign-in wall
// (signinWallRE), carries no blocking dialog or startup interstitial
// (DetectInput), and paints the idle composer prompt (PromptReady). So a
// healthy codex screen with no Plan marker answers ("default", true), while an
// onboarding wall, an interstitial covering the composer, or a blank frame
// answers ("", false).
//
// A Plan marker that IS on screen is itself a readable signal and short-circuits
// the composer gate — an approval dialog replaces the composer while leaving the
// mode marker painted, and "plan" is the honest answer for that frame.
//
// The input must be a pkg/screen render, never raw PTY bytes: only the emulator
// reassembles the right-aligned marker's column jumps into a contiguous row.
func collaborationMode(text string) (string, bool) {
	if collaborationPlanRE.MatchString(text) {
		return collabPlan, true
	}
	if !composerReadable(text) {
		return "", false
	}
	return collabDefault, true
}

// composerReadable reports whether the screen shows a healthy codex composer —
// the precondition for reading "no marker" as "default" rather than "unknown".
// It is the readiness gate's codex arm (pkg/chat/ready.go:166-183) expressed
// over the package's own predicates, so the two cannot drift into disagreeing
// about what a usable codex screen looks like.
func composerReadable(text string) bool {
	if signinWallRE.MatchString(text) {
		return false
	}
	if _, blocking := DetectInput(text); blocking {
		return false
	}
	return PromptReady(text)
}
