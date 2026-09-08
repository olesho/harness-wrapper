package claudecode

import (
	"regexp"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/turns"
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

// permissionModeNatives translates Claude Code's native footer wording to
// CLAUDE'S OWN spelling of the posture — the --permission-mode vocabulary, NOT
// harness-wrapper's canonical rungs. The values are exactly the keys of
// claude's bundled permissiveness rank table
// ({plan:0, bubble:1, default:1, dontAsk:1, acceptEdits:2, auto:3,
// bypassPermissions:4}), which is why the manual rung is spelled "default"
// here: claude has no mode named "manual".
//
// Keyed identically to permissionModeRungs (ASCII apostrophe), off the same
// closed alternation, so the two maps cannot drift apart on a key. The values
// are DIAGNOSTIC — they exist for error messages and for the ring-membership
// question below, and must never be compared against wrapper.PermissionRungs().
var permissionModeNatives = map[string]string{
	"plan mode":          "plan",
	"manual mode":        "default",
	"accept edits":       "acceptEdits",
	"auto mode":          "auto",
	"bypass permissions": "bypassPermissions",
	"don't ask":          "dontAsk",
}

// cycleRingNatives answers the VOCABULARY question: can claude's Shift+Tab
// cycle ever paint this word? Claude's ring function returns
// ["plan","default","acceptEdits"], extended with "auto" and
// "bypassPermissions" for a session launched able to reach them — and
// "dontAsk" is absent from it in every configuration. dontAsk is a launch-only
// spelling: --permission-mode dontAsk puts the session there and nothing the
// cycle key does can put it back.
//
// "auto" and "bypassPermissions" map to true because the cycle CAN paint them;
// whether THIS session's ring contains them is a different question, answered
// by pkg/chat.cycleRing from the launch argv. Keeping the two apart is what
// lets this adapter stay a pure screen parser with no launch state in it.
var cycleRingNatives = map[string]bool{
	"plan":              true,
	"default":           true,
	"acceptEdits":       true,
	"auto":              true,
	"bypassPermissions": true,
	"dontAsk":           false,
}

// permissionPostureFromFooter reads Claude Code's current permission posture
// off the rendered screen: the canonical rung (plan|manual|ask|auto|bypass —
// the wrapper.PermissionRungs() vocabulary), claude's own spelling of it, and
// whether that spelling is one the Shift+Tab cycle can produce.
//
// It returns (turns.PermissionPosture{}, false) when the screen carries no
// readable marker at all: an onboarding/auth wall that paints no footer, a
// modal covering it, or a future Claude release that renames the modes. That
// is deliberately distinguishable from a healthy read — callers can tell
// "unreadable" from "readable, and not plan".
//
// This is the file's ONLY parse of the footer; permissionModeFromFooter is a
// projection of it, so the rung and the native spelling can never disagree.
//
// The input must be a pkg/screen render, never raw PTY bytes: only the
// emulator reassembles the footer's column jumps into a contiguous line.
func permissionPostureFromFooter(text string) (turns.PermissionPosture, bool) {
	m := permissionModeRE.FindStringSubmatch(text)
	if len(m) < 2 {
		return turns.PermissionPosture{}, false
	}
	// Collapse the emulator's padding so the lookup key is the canonical
	// single-spaced wording regardless of how wide the column jump landed.
	// A curly apostrophe folds onto the ASCII key so one map entry suffices.
	// One normalisation for BOTH lookups, since there is one parse.
	key := strings.ReplaceAll(strings.Join(strings.Fields(m[1]), " "), "’", "'")
	rung, ok := permissionModeRungs[key]
	if !ok {
		return turns.PermissionPosture{}, false
	}
	native, ok := permissionModeNatives[key]
	if !ok {
		// Unreachable while the two maps are keyed off the same closed
		// alternation; degrading to unknown rather than to a half-read
		// posture is the safe answer if that ever stops being true.
		return turns.PermissionPosture{}, false
	}
	return turns.PermissionPosture{Rung: rung, Native: native, OnRing: cycleRingNatives[native]}, true
}

// permissionModeFromFooter reads Claude Code's current permission posture off
// the rendered screen and returns it as a canonical rung
// (plan|manual|ask|auto|bypass — the wrapper.PermissionRungs() vocabulary).
//
// It returns ("", false) when the screen carries no readable marker at all:
// an onboarding/auth wall that paints no footer, a modal covering it, or a
// future Claude release that renames the modes. That is deliberately
// distinguishable from a healthy read — callers can tell "unreadable" from
// "readable, and not plan".
//
// It is the RUNG PROJECTION of permissionPostureFromFooter, deliberately not a
// second parse: the two readers are then incapable of disagreeing.
//
// The input must be a pkg/screen render, never raw PTY bytes: only the
// emulator reassembles the footer's column jumps into a contiguous line.
func permissionModeFromFooter(text string) (string, bool) {
	p, ok := permissionPostureFromFooter(text)
	if !ok {
		return "", false
	}
	return p.Rung, true
}
