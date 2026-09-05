package claudecode

import (
	"regexp"
	"strings"
)

// horizontalSpace matches one space-like character that is NOT a line break.
// Mirrors the repo-wide idiom (pkg/chat/ready.go, pkg/wrapper/internal/harness/
// claude) and the `[^\S\r\n]*` runs already used by thinkingRE and bulletRE.
//
// It matters here more than it looks: Claude Code paints the footer with
// absolute-column jumps interleaved with SGR sequences rather than literal
// runs of spaces — see test/corpus/claude-code/multi-turn/bytes.raw, which
// carries "…uto\x1b[11Gmode\x1b[16Gon\x1b[38;2;153;153;153m (shift+tab…".
// The gaps that survive into the rendered snapshot are emulator artifacts of
// those jumps, so their width is not something the parser may depend on.
const horizontalSpace = `[^\S\r\n]`

// permissionModeRE matches Claude Code's permission-mode footer marker: the
// mode glyph (⏸ for the non-executing rungs, ⏵⏵ for the executing ones)
// followed by the mode's words and the literal "on". Group 1 is the words.
//
// The six live footer lines, captured from claude-code 2.1.217:
//
//	⏵⏵ auto mode on (shift+tab to cycle) · ← for agents
//	⏸ manual mode on · ← for agents
//	⏵⏵ accept edits on (shift+tab to cycle) · ← for agents
//	⏸ plan mode on (shift+tab to cycle) · ← for agents
//	⏵⏵ bypass permissions on (shift+tab to cycle) · ← for agents
//	⏵⏵ don't ask on (shift+tab to cycle) · ← for agents
//
// Six footer words, five rungs: "don't ask" and "manual mode" are two native
// spellings of ONE canonical rung, exactly as "accept edits" is a second
// spelling of "ask". See permissionModeRungs for the rank-table evidence.
// The apostrophe alternation ['’] is defensive: the recorded capture uses
// ASCII U+0027, and the U+2019 branch exists only so a future release that
// typesets the footer cannot silently drop the read to "unknown".
//
// Two deliberate shape decisions:
//
//  1. EVERYTHING after "<words> on" is optional, for EVERY mode — never make
//     "(shift+tab to cycle)" optional for one mode only. This repo's own
//     fixtures contradict any such asymmetry: "auto mode on" renders
//     suffix-less in busy_test.go / turncomplete_busy_test.go /
//     pkg/chat/quiescence_test.go, and the parenthetical's *tail* is swapped
//     out entirely while Claude works ("· esc to interrupt") or while
//     sub-agents run ("· ↓ to manage").
//
//  2. No `$` anchor. pkg/screen right-pads every row to the terminal width
//     (pkg/screen/screen.go:24-26 keeps per-row trailing whitespace), and the
//     footer row is shared with other chrome — in
//     test/corpus/claude-code/interrupted-mid-reply the effort indicator
//     ("○ low · /effort") sits on the same 120-column row. The marker is
//     matched as a substring within a line; everything after it is ignored.
//
// The alternation is CLOSED on purpose: it enumerates the six known modes
// instead of capturing a generic "<words> on". A renamed or newly-added
// Claude mode therefore fails to match and degrades to "unknown" — never to a
// wrong rung — and the ⏸/⏵ glyph appearing in ordinary prose (release notes
// render "Added a grey ⏸ badge to the footer when in manual permission mode")
// can never be mistaken for a footer.
var permissionModeRE = regexp.MustCompile(spaced(
	`(?m)[⏸⏵]{1,2} (plan mode|manual mode|accept edits|auto mode|bypass permissions|don['’]t ask) on\b`,
))

// spaced rewrites every literal space in pat into "one or more horizontal
// space characters", so the pattern stays readable while still tolerating the
// emulator's column-jump spacing at every inter-token gap.
func spaced(pat string) string {
	return strings.ReplaceAll(pat, " ", horizontalSpace+`+`)
}

// permissionModeRungs translates Claude Code's native footer wording to the
// canonical, harness-independent permission rung. It TRANSLATES ONLY; it
// rejects nothing, because an unknown word can never reach it — the closed
// alternation in permissionModeRE has already refused to match. Claude's
// native spellings therefore never leave this file.
var permissionModeRungs = map[string]string{
	"plan mode":          "plan",
	"manual mode":        "manual",
	"accept edits":       "ask",
	"auto mode":          "auto",
	"bypass permissions": "bypass",
	// claude's --permission-mode dontAsk paints its own footer word but is
	// NOT its own rung. Claude's bundled permissiveness rank table reads
	// {plan:0, bubble:1, default:1, dontAsk:1, acceptEdits:2, auto:3,
	// bypassPermissions:4}: dontAsk ties with default (claude's spelling of
	// manual), and wrapper.PermissionRungs() is a strict total order that
	// cannot express a tie. dontAsk is also strictly more restrictive in
	// effect ("deny if not pre-approved"), so reporting manual can never
	// UNDER-report permissiveness. Keyed with an ASCII apostrophe; the
	// regex's U+2019 variant is normalised to this form before the lookup.
	"don't ask": "manual",
}

// permissionModeFromFooter reads Claude Code's current permission posture off
// the rendered screen text and returns it as a canonical rung
// (plan|manual|ask|auto|bypass — the wrapper.PermissionRungs() vocabulary).
//
// It returns ("", false) when the screen carries no readable marker at all:
// an onboarding/auth wall that paints no footer, a modal covering it, or a
// future Claude release that renames the modes. That is deliberately
// distinguishable from a healthy read — callers can tell "unreadable" from
// "readable, and not plan".
//
// The input must be a pkg/screen render, never raw PTY bytes: only the
// emulator reassembles the footer's column jumps into a contiguous line.
func permissionModeFromFooter(text string) (string, bool) {
	m := permissionModeRE.FindStringSubmatch(text)
	if len(m) < 2 {
		return "", false
	}
	// Collapse the emulator's padding so the lookup key is the canonical
	// single-spaced wording regardless of how wide the column jump landed.
	// A curly apostrophe folds onto the ASCII key so one map entry suffices.
	key := strings.ReplaceAll(strings.Join(strings.Fields(m[1]), " "), "’", "'")
	rung, ok := permissionModeRungs[key]
	if !ok {
		return "", false
	}
	return rung, true
}
