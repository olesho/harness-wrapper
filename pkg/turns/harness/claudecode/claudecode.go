// Package claudecode provides a turn-detection adapter for Anthropic's
// Claude Code CLI (claude / @anthropic-ai/claude-code).
//
// Detection signals first observed on 2.1.141; re-verified against 2.1.185
// (corpus multi-turn/tool-call re-baked, live sentinel round-trip). The pin
// in versions.json is 2.1.201, adopted for cross-repo parity with
// meta-harness; detection signals were last verified at 2.1.185:
//
//   - End of an assistant turn: a "✻ <verb> for Ns" thinking-summary
//     line appears, where <verb> is a colorful word like Baked, Brewed,
//     Crunched, Pondered, etc., and N is an integer second count.
//     The full line is a per-turn fingerprint: when a new one appears
//     on screen, the turn just completed.
//
//   - User interrupt: a "⎿  Interrupted · What should Claude do
//     instead?" line appears. The turn ended in a recoverable error
//     state.
//
// This adapter embeds generic.Adapter so wrapper-level status events
// (blocked_by_cost, retry_later, failed) keep flowing through.
//
// Markers may shift across upstream versions; the golden-recording
// tests under test/corpus/claude-code/ are the early-warning signal.
package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
)

// thinkingRE matches the end-of-turn thinking-summary line, anchored
// at the start and end of a screen line so it does not mis-fire when
// the model echoes the marker shape as part of its reply content
// (e.g. "you'd see '✻ Baked for 5s' here" in explanatory prose).
//
// Format: U+273B (✻) + space + capitalized verb + " for " + a duration,
// optionally surrounded by horizontal whitespace, on its own line. The
// marker text is the first capture group, so the fingerprint stored on the
// Adapter does not include the emulator's column padding.
//
// The duration is one or more space-separated <number><unit> components,
// unit ∈ {h,m,s}. Claude Code renders a sub-minute turn as "5s" but switches
// to "1m 22s" once the turn crosses 60s (and "1h 2m 3s" past an hour).
// Matching only `\d+s` silently missed EVERY turn ≥ 60s: the end-of-turn
// event never fired, so RunTurn hung until a caller-side idle guard or the
// liveness watchdog stepped in. Observed on Claude Code 2.1.178 — a long turn
// summarised as "✻ Cooked for 1m 22s".
//
// Examples that match: "✻ Baked for 5s", "✻ Brewed for 4s",
// "✻ Sautéed for 4s", "✻ Cooked for 1m 22s", "✻ Pondered for 1h 2m 3s" —
// each on a line by itself (trailing column padding from the emulator is
// allowed).
//
// Claude Code 2.1.247 appends a trailing status clause to the settled
// summary, separated by " · ": "✻ Churned for 2m 27s · done 2:26 AM", and it
// may append further clauses ("✻ Sautéed for 11m 51s · done 10:47 AM · 2
// shells still running"). The end anchor rejected every such frame, so
// TurnComplete never fired and RunTurn hung until an external watchdog killed
// the run — the same class of miss as the sub-minute duration bug above, and
// undetected across ~30 releases because the recorded corpus held only the
// bare pre-2.1.24x shape.
//
// The suffix is therefore matched LOOSELY on purpose: everything from the
// first "·" to end of line is accepted without modelling the clause
// vocabulary, so the next clause Claude Code invents cannot re-break
// detection. The optional tail sits OUTSIDE capture group 1: group 1 stays
// the BARE marker text, which is the adapter's de-duplication fingerprint
// (two frames of one settled turn differ only in the "· done <clock>" tail
// and must fingerprint identically) as well as the visible reason string.
//
// The line anchors are kept, so the marker shape embedded in explanatory
// prose ("you'd see '✻ Baked for 5s · done 1:00 PM' here") still does not
// match — the leading text before ✻ defeats the start anchor.
//
// Examples that do NOT match: the in-progress indicator
// "✻ Cooking… (1m 22s · esc to interrupt)". It does contain " · ", but the
// required "✻ <Verb> for <dur>" prefix never matches — the duration lives
// inside the parenthetical and there is no " for " component outside it.
//
// The loose tail does admit the in-flight variant
// "✻ Cooked for 1m 22s · ↑ 3.1k tokens · esc to interrupt", which the end
// anchor used to reject. That is deliberate and safe: OnScreen acts on a
// match only while !Busy(snap), and Busy keys off exactly that "esc to
// interrupt" footer (plus the working spinner), so an in-flight frame is
// still rejected — by the busy gate rather than by the regex.
var thinkingRE = regexp.MustCompile(
	`(?m)^[^\S\r\n]*(✻ \p{Lu}\p{L}+ for \d+[hms](?: \d+[hms])*)(?:[^\S\r\n]*·[^\S\r\n]*[^\r\n]*)?[^\S\r\n]*$`,
)

// resumeRE matches the "claude --resume <uuid>" hint Claude Code prints
// when it ends a session. The UUID names the on-disk transcript file.
var resumeRE = regexp.MustCompile(`claude --resume ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// interruptMarker is the literal text Claude Code writes after the user
// interrupts a streaming reply (Esc / Ctrl-C). Claude Code uses U+23BF
// (⎿), then a regular ASCII space, then U+00A0 (non-breaking space),
// then "Interrupted · ...". The NBSP is easy to miss — match it exactly.
const interruptMarker = "⎿  Interrupted · What should Claude do instead?"

// reasonPrefix labels the source harness on emitted turn-event reasons.
const reasonPrefix = "claude-code: "

// Blocking-dialog anchors. These full-screen prompts gate progress at
// startup and cannot be satisfied by the normal Send flow; they are
// answered out-of-band via the turns InputRequested/InputResolved channel.
// Each anchor is a stable substring so detection survives re-renders and so
// the request ID (derived from the anchor) stays constant across redraws.
const (
	// trustAnchor / trustAnchorAlt are the folder-trust dialog phrasings.
	trustAnchor    = "Do you trust the files in this folder?"
	trustAnchorAlt = "Is this a project you created or one you trust?"
	// bypassAnchor is the --dangerously-skip-permissions acceptance screen,
	// which is itself a blocking confirm even though it "skips" permissions.
	bypassAnchor = "Bypass Permissions mode"
)

// menuRE matches a numbered menu item line, e.g. "(selector) 1. Yes, proceed"
// or "  2. No, exit". Leading box-drawing / selector / whitespace (none of it
// alphanumeric) is skipped; group 1 is the single-digit choice number and
// group 2 is the rest of the line (label plus any trailing column padding or
// right-edge box border), which parseMenuOptions then cleans.
var menuRE = regexp.MustCompile(`(?m)^[^\dA-Za-z\n]*(\d)\.[^\S\n]+(\S[^\n]*)$`)

// Adapter implements turns.Adapter for Claude Code.
type Adapter struct {
	generic.Adapter

	mu                sync.Mutex
	lastFingerprint   string
	lastInterruptSeen bool

	// lastInputID is the ID of the blocking dialog currently on screen, or
	// "" when none. lastInput retains the full request so InputResolved can
	// name what cleared.
	lastInputID string
	lastInput   *turns.InputRequest
}

// New constructs a Claude Code adapter.
func New() *Adapter { return &Adapter{} }

// Name returns "claude-code".
func (*Adapter) Name() string { return "claude-code" }

// OnScreen scans the snapshot for the thinking-summary and interrupt
// markers and emits TurnComplete / Errored when transitions occur.
func (a *Adapter) OnScreen(snap screen.Snapshot) []turns.Event {
	a.mu.Lock()
	defer a.mu.Unlock()

	var out []turns.Event

	// Interrupt detection — transition not-seen → seen.
	interruptNow := strings.Contains(snap.Text, interruptMarker)
	if interruptNow && !a.lastInterruptSeen {
		out = append(out, turns.Event{Kind: turns.Errored, Reason: reasonPrefix + interruptMarker})
	}
	a.lastInterruptSeen = interruptNow

	// Turn-complete detection — newest thinking marker differs from last fired.
	// Capture group 1 holds the marker text without surrounding column
	// padding so the fingerprint stays stable across redraws.
	//
	// Claude prints a "✻ <verb> for Ns" summary after EACH thinking block, not
	// only at the very end — so an intermediate one (claude pausing to think,
	// then continuing to write files / run tools) must NOT be mistaken for
	// end-of-turn, or the turn is cut off before the work happens. Gate on
	// Busy(): a real end-of-turn marker is shown while idle (no "esc to
	// interrupt"); an intermediate one is shown while still working. When busy,
	// we neither emit nor advance lastFingerprint, so the genuine end-of-turn
	// marker (idle) still fires on a later snapshot.
	matches := thinkingRE.FindAllStringSubmatch(snap.Text, -1)
	if len(matches) > 0 && !a.Busy(snap) {
		latest := matches[len(matches)-1][1]
		if latest != a.lastFingerprint {
			a.lastFingerprint = latest
			out = append(out, turns.Event{Kind: turns.TurnComplete, Reason: reasonPrefix + latest})
		}
	}

	// Blocking interactive prompt (trust dialog, bypass acceptance, …) —
	// transition on the request ID. A new dialog (or a different one
	// replacing the current) emits InputRequested; the dialog clearing
	// emits InputResolved.
	if req, ok := DetectInput(snap.Text); ok {
		if req.ID != a.lastInputID {
			a.lastInputID = req.ID
			a.lastInput = req
			out = append(out, turns.Event{Kind: turns.InputRequested, Reason: reasonPrefix + req.Prompt, Input: req})
		}
	} else if a.lastInputID != "" {
		resolved := a.lastInput
		if resolved == nil {
			resolved = &turns.InputRequest{ID: a.lastInputID}
		}
		a.lastInputID = ""
		a.lastInput = nil
		out = append(out, turns.Event{Kind: turns.InputResolved, Reason: "claude-code: input resolved", Input: resolved})
	}

	return out
}

// DetectInput recognizes a blocking interactive dialog in the rendered
// screen text and returns the structured request, or (nil, false) when no
// dialog is present. It is a pure function so the chat layer's readiness
// check and this adapter share one source of truth about what counts as a
// blocking prompt.
func DetectInput(text string) (*turns.InputRequest, bool) {
	prompt, ok := dialogAnchor(text)
	if !ok {
		return nil, false
	}
	// The unnumbered fallback needs to know WHERE the question is: its menu sits
	// just below it, whereas a screen that merely quotes the anchor has nothing
	// there but transcript and the composer.
	anchorLine := strings.Count(text[:strings.Index(text, prompt)], "\n")
	opts := parseMenuOptions(text, anchorLine)
	if len(opts) == 0 {
		// Anchor visible but the menu hasn't rendered yet — not actionable.
		return nil, false
	}
	req := &turns.InputRequest{Kind: "trust_prompt", Prompt: prompt, Options: opts}
	req.ID = inputID(req)
	return req, true
}

// dialogAnchor returns the blocking-dialog anchor visible in text, if any. It
// is the ONE place the anchor list is consulted, so DetectInput and
// AnchorPresent can never disagree about what counts as a blocking prompt.
func dialogAnchor(text string) (string, bool) {
	switch {
	case strings.Contains(text, trustAnchor):
		return trustAnchor, true
	case strings.Contains(text, trustAnchorAlt):
		return trustAnchorAlt, true
	case strings.Contains(text, bypassAnchor):
		return bypassAnchor, true
	}
	return "", false
}

// AnchorPresent reports whether a blocking-dialog anchor is still painted on
// screen. It is deliberately WEAKER than DetectInput: an anchor whose menu has
// not rendered yet is not actionable (DetectInput returns false) but the dialog
// is very much still up, so "has my answer cleared it?" must ask this and not
// DetectInput — otherwise a mid-paint frame reads as success.
//
// Exported so pkg/chat can confirm an answer landed without copying the anchor
// list; the anchors stay the sole property of this package.
func AnchorPresent(text string) bool {
	_, ok := dialogAnchor(text)
	return ok
}

// numberedLabelRE strips the "N." prefix a numbered menu row carries, so the
// highlighted row's label is comparable with the option Labels DetectInput
// reports (which never include the number).
var numberedLabelRE = regexp.MustCompile(`^\d+\.[^\S\n]+`)

// HighlightedLabel returns the cleaned label of the row currently carrying the
// menu marker ("\u276f"), or ("", false) when no row is highlighted.
//
// This is what lets a caller confirm that NAVIGATION landed before it presses
// Enter: the marker's row is the row Enter will select, and matching it by
// LABEL rather than by index is required — claude-code 2.1.261 inverted the
// folder-trust option order, so an index that was "proceed" became "exit".
func HighlightedLabel(text string) (string, bool) {
	m := markerRE.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	label := cleanLabel(strings.TrimSpace(numberedLabelRE.ReplaceAllString(strings.TrimSpace(m[2]), "")))
	if label == "" {
		return "", false
	}
	return label, true
}

// parseMenuOptions extracts the numbered choices, de-duplicating by choice
// number so a redraw that paints the menu twice yields one option set.
// markerRE matches the highlighted row of an UNNUMBERED menu. Claude Code
// 2.1.261 dropped the "N." prefixes from the folder-trust dialog and moved the
// default highlight onto "No, exit", so the numbered parser below finds nothing
// and DetectInput reports the dialog as not-actionable — no InputRequested is
// emitted, a trust_prompt=allow policy is never consulted, and the harness exits
// on the default "No, exit".
//
// Only the real highlight glyphs are accepted: ❯ (claude-code) and › (codex).
// A bare ">" is ordinary prose punctuation — a quote, a diff marker, a shell
// transcript — and admitting it turned any screen that merely QUOTED one of
// the anchors into a menu.
var markerRE = regexp.MustCompile(`(?m)^([^\S\n]*)(?:❯|›)[^\S\n]+(\S[^\n]*)$`)

// menuNavKeys are the bytes that move an unnumbered menu's highlight. A digit
// cannot be used: there is no digit on screen to press.
var (
	menuKeyDown  = []byte("\x1b[B")
	menuKeyUp    = []byte("\x1b[A")
	menuKeyEnter = []byte("\r")
)

// maxUnnumberedMenuRows bounds how much of the screen an unnumbered menu may
// claim, so a stray marker line cannot turn arbitrary prose into options.
const maxUnnumberedMenuRows = 8

// maxAnchorToMenuRows bounds how far BELOW the anchor question the menu may
// sit. The live 2.1.261 capture has 5 lines between the anchor and "❯ No,
// exit"; the composer prompt of an agent that merely quotes the anchor is
// typically dozens of rows further down, at the bottom of the viewport.
const maxAnchorToMenuRows = 20

// parseUnnumberedMenuOptions extracts choices from a menu with no "N." prefixes
// by locating the ❯ marker and taking the contiguous block around it.
// Because selection is positional, each option's Keys walk the highlight from
// the marker row to that option's row and press Enter — never a bare digit,
// which would be typed into the dialog rather than selecting anything.
//
// anchorLine is the row of the anchor question DetectInput matched. The menu
// belongs to that question, so it must be BELOW it and within
// maxAnchorToMenuRows: prose that merely quotes an anchor arms this fallback,
// and without the offset the ❯ of the COMPOSER at the bottom of the screen was
// parsed as the menu — submitting whatever the operator had typed.
func parseUnnumberedMenuOptions(text string, anchorLine int) []turns.InputOption {
	lines := strings.Split(text, "\n")
	// A screen can carry several markers (the dialog's own, and the composer's).
	// Take the first that actually looks like this anchor's menu.
	for _, m := range markerRE.FindAllStringSubmatchIndex(text, -1) {
		markerLine := strings.Count(text[:m[0]], "\n")
		if opts := unnumberedMenuAt(lines, markerLine, anchorLine); opts != nil {
			return opts
		}
	}
	return nil
}

// unnumberedMenuAt reads the option block around the marker row, or nil when
// that row is not a menu highlight.
func unnumberedMenuAt(lines []string, markerLine, anchorLine int) []turns.InputOption {
	if markerLine <= anchorLine || markerLine-anchorLine > maxAnchorToMenuRows {
		return nil
	}
	if isComposerRow(lines, markerLine) {
		return nil
	}

	first := markerLine
	for first > 0 && !isMenuBoundary(lines[first-1]) && markerLine-first+1 < maxUnnumberedMenuRows {
		first--
	}
	last := markerLine
	for last < len(lines)-1 && !isMenuBoundary(lines[last+1]) && last-first+1 < maxUnnumberedMenuRows {
		last++
	}

	var opts []turns.InputOption
	cursor := -1
	for i := first; i <= last; i++ {
		label := stripMarker(lines[i])
		if label == "" {
			continue
		}
		if i == markerLine {
			cursor = len(opts)
		}
		opts = append(opts, turns.InputOption{
			ID:    strconv.Itoa(len(opts) + 1),
			Alias: aliasForLabel(label),
			Label: label,
		})
	}
	// Checked AFTER chrome filtering: a "menu" that only reaches two rows by
	// counting a border is not a menu.
	if cursor < 0 || len(opts) < 2 {
		// A lone highlighted line is a cursor, not a menu.
		return nil
	}
	for i := range opts {
		var keys []byte
		step, n := menuKeyDown, i-cursor
		if n < 0 {
			step, n = menuKeyUp, -n
		}
		for j := 0; j < n; j++ {
			keys = append(keys, step...)
		}
		opts[i].Keys = append(keys, menuKeyEnter...)
	}
	return opts
}

// isComposerRow reports whether the marker on this row is the INPUT BOX prompt
// rather than a menu highlight. The composer's ❯ is framed: its immediate
// neighbours on both sides are the box's rules. In the real dialog neither
// neighbour is chrome (a blank line above, the sibling option below), so the
// true positive is untouched — and a menu that merely ABUTS a rule on one side
// stays a menu.
func isComposerRow(lines []string, markerLine int) bool {
	if markerLine == 0 || markerLine == len(lines)-1 {
		return false
	}
	return boxOrRuleRE.MatchString(lines[markerLine-1]) && boxOrRuleRE.MatchString(lines[markerLine+1])
}

// isMenuBoundary reports whether ln terminates the option block: a blank line,
// or any of the transcript chrome that must never become an option (box rules,
// message bullets, tool-result continuations, the thinking footer).
func isMenuBoundary(ln string) bool {
	if strings.TrimSpace(ln) == "" {
		return true
	}
	return boxOrRuleRE.MatchString(ln) || bulletRE.MatchString(ln) ||
		toolResultRE.MatchString(ln) || thinkingRE.MatchString(ln)
}

// stripMarker drops the highlight glyph and column padding from a menu row.
func stripMarker(s string) string {
	s = strings.TrimSpace(s)
	for _, mk := range []string{"❯", "›"} {
		if strings.HasPrefix(s, mk) {
			s = strings.TrimSpace(strings.TrimPrefix(s, mk))
			break
		}
	}
	return cleanLabel(s)
}

func parseMenuOptions(text string, anchorLine int) []turns.InputOption {
	var opts []turns.InputOption
	seen := make(map[string]bool)
	for _, m := range menuRE.FindAllStringSubmatch(text, -1) {
		num, label := m[1], cleanLabel(m[2])
		if num == "0" || seen[num] || label == "" {
			continue
		}
		seen[num] = true
		opts = append(opts, turns.InputOption{
			ID:    num,
			Alias: aliasForLabel(label),
			Label: label,
			// Claude Code's startup menus accept the choice digit followed by
			// Enter. The digit selects the row regardless of the initial
			// highlight; the Enter confirms.
			Keys: []byte(num + "\r"),
		})
	}
	if len(opts) == 0 {
		return parseUnnumberedMenuOptions(text, anchorLine)
	}
	return opts
}

// cleanLabel strips trailing column padding and right-edge box borders from a
// captured menu label. The label text itself never contains a run of two or
// more spaces, so the first such run marks the start of padding before any
// border glyph.
func cleanLabel(s string) string {
	if i := strings.Index(s, "  "); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// aliasForLabel maps a menu label to a portable intent so policies can target
// "proceed"/"deny" without knowing the concrete wording.
func aliasForLabel(label string) string {
	l := strings.ToLower(label)
	switch {
	case containsAny(l, "proceed", "accept", "trust", "yes", "continue"):
		return "proceed"
	case containsAny(l, "exit", "deny", "reject", "cancel", "no,", "no ", "don't", "do not"):
		return "deny"
	default:
		return ""
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// inputID derives a stable id from the dialog's identity (kind + prompt +
// option labels) so consecutive redraws of one dialog collapse to a single
// request while a genuinely different dialog gets a fresh id.
func inputID(req *turns.InputRequest) string {
	var b strings.Builder
	b.WriteString(req.Kind)
	b.WriteByte(0)
	b.WriteString(req.Prompt)
	for _, o := range req.Options {
		b.WriteByte(0)
		b.WriteString(o.Label)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// bulletRE matches the start of a rendered assistant/tool message: Claude Code
// prefixes each with U+23FA (⏺) and a space. Leading column padding (none of it
// the bullet itself) is tolerated. Group 1 is the text after the bullet.
var bulletRE = regexp.MustCompile(`^[^\S\r\n]*⏺ (.*)$`)

// toolResultRE matches a tool-result continuation line (U+23BF "⎿"), which
// belongs to a tool call, not the assistant's prose reply.
var toolResultRE = regexp.MustCompile(`^[^\S\r\n]*⎿`)

// boxOrRuleRE matches a horizontal rule / box border line (runs of ─ or
// box-drawing chrome) that frames the input area below the transcript.
var boxOrRuleRE = regexp.MustCompile(`^[^\S\r\n]*[─━╭╮╰╯│┌┐└┘]`)

// ExtractMessage isolates the assistant's final reply from the rendered TUI.
// Claude Code renders each assistant message as a "⏺ <text>" block whose
// continuation lines are indented under the bullet; the block ends at the
// "✻ <verb> for Ns" thinking footer, a tool-result line, a box/rule, the next
// "⏺"/"❯", or a blank line. We take the LAST such block before the thinking
// footer — the model's final message for the turn — dedented and trimmed.
// Implements turns.MessageExtractor. Returns ("", false) when no bullet block
// is present (caller falls back to the raw screen).
func (*Adapter) ExtractMessage(snap screen.Snapshot) (string, bool) {
	lines := strings.Split(snap.Text, "\n")

	// Scope to the most-recently-completed turn: its "✻ <verb> for Ns" footer
	// is the lower bound. The final assistant message is the last "⏺" block
	// ABOVE that footer. Bounding this way ignores stale messages from earlier
	// turns/resumed sessions still on screen, and the empty input box below.
	start := lastBulletStart(lines, lastThinkingFooter(lines))
	if start < 0 {
		return "", false
	}

	msg := assembleMessage(collectBlock(lines, start))
	if strings.TrimSpace(msg) == "" {
		return "", false
	}
	return msg, true
}

// lastThinkingFooter returns the index of the LAST "✻ <verb> for Ns" thinking
// footer, or len(lines) when none is present.
func lastThinkingFooter(lines []string) int {
	limit := len(lines)
	for i, ln := range lines {
		if thinkingRE.MatchString(ln) {
			limit = i // keep the LAST footer's index
		}
	}
	return limit
}

// lastBulletStart returns the index of the last "⏺" bullet before limit. When
// there is no bullet before the footer (or no footer), it falls back to the last
// bullet anywhere on screen. Returns -1 when no bullet is present.
func lastBulletStart(lines []string, limit int) int {
	start := -1
	for i := 0; i < limit; i++ {
		if bulletRE.MatchString(lines[i]) {
			start = i
		}
	}
	if start >= 0 {
		return start
	}
	for i, ln := range lines {
		if bulletRE.MatchString(ln) {
			start = i
		}
	}
	return start
}

// isBlockBoundary reports whether ln terminates the current assistant message
// block: the next bullet, a tool-result line, a box/rule, the thinking footer,
// or the "❯" input prompt. A blank line is deliberately NOT a boundary.
func isBlockBoundary(ln string) bool {
	if bulletRE.MatchString(ln) || toolResultRE.MatchString(ln) || boxOrRuleRE.MatchString(ln) {
		return true
	}
	if thinkingRE.MatchString(ln) {
		return true
	}
	return strings.HasPrefix(strings.TrimLeft(ln, " "), "❯")
}

// collectBlock gathers the bullet line at start plus its indented continuation
// lines, stopping at the first boundary, then drops trailing blank lines.
func collectBlock(lines []string, start int) []string {
	m := bulletRE.FindStringSubmatch(lines[start])
	block := []string{strings.TrimRight(m[1], " ")}

	// Consume indented continuation lines until a boundary.
	for i := start + 1; i < len(lines); i++ {
		ln := lines[i]
		if isBlockBoundary(ln) {
			break
		}
		if strings.TrimSpace(ln) == "" {
			// A blank line is a PARAGRAPH BREAK within the message, not its end —
			// claude renders multi-paragraph replies (e.g. a summary followed by a
			// final "INTEGRATION: PASS" line) with a blank line between. Keep it;
			// the real boundaries above (next bullet / tool-result / box / footer /
			// "❯") terminate the block, and trailing blanks are trimmed below.
			block = append(block, "")
			continue
		}
		block = append(block, strings.TrimRight(ln, " "))
	}
	// Drop trailing blank lines (the gap between the message and the input box).
	for len(block) > 1 && strings.TrimSpace(block[len(block)-1]) == "" {
		block = block[:len(block)-1]
	}
	return block
}

// assembleMessage flattens a collected block into the final message. block[0] is
// already flush (the regex consumed the "⏺ " prefix). The continuation lines are
// indented to align under that text, so dedent them on their own before
// rejoining — otherwise the flush first line pins the common indent at 0 and the
// continuations keep their alignment padding.
func assembleMessage(block []string) string {
	msg := block[0]
	if len(block) > 1 {
		if tail := dedent(block[1:]); tail != "" {
			msg += "\n" + tail
		}
	}
	return msg
}

// dedent removes the longest common run of leading spaces shared by all
// non-empty lines, so message continuation lines indented under the "⏺ "
// bullet come back flush-left.
func dedent(lines []string) string {
	minIndent := -1
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		n := len(ln) - len(strings.TrimLeft(ln, " "))
		if minIndent < 0 || n < minIndent {
			minIndent = n
		}
	}
	if minIndent <= 0 {
		return strings.TrimRight(strings.Join(lines, "\n"), "\n")
	}
	out := make([]string, len(lines))
	for i, ln := range lines {
		if len(ln) >= minIndent {
			out[i] = ln[minIndent:]
		} else {
			out[i] = strings.TrimLeft(ln, " ")
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// busyMarker is the footer hint Claude Code shows ONLY while a turn is in
// flight (generating or running a tool): "esc to interrupt". When Claude is
// idle at the prompt the footer switches away from it (e.g. to "← for agents"),
// so its presence is a reliable "still working" signal. Verified against the
// screen corpus: every settled/idle final frame lacks it.
//
// It is NOT sufficient on its own: while Claude waits on SUB-AGENTS (the Task /
// Explore tool — "✶ Cerebrating… (57s · ↓ 4.8k tokens)" with "◯ Explore …" rows),
// the "esc to interrupt" footer can flicker out for a redraw frame while the work
// continues. A "✻ <verb> for Ns" intermediate summary landing on such a frame
// would then pass the turn-complete gate and cut the turn off mid-sub-agent. So
// Busy ALSO keys off the in-progress spinner line (workingRE), which is present
// throughout active work and absent on every settled frame.
const busyMarker = "esc to interrupt"

// workingRE matches Claude Code's in-progress spinner line: a "<gerund>… (<dur> ·
// <counter>)" indicator such as "✶ Cerebrating… (57s · ↓ 4.8k tokens)",
// "✢ Schlepping… (3s · ↓2 tokens)", or "✻ Cooking… (1m 22s · esc to interrupt)".
// It anchors on the structural signature — an ellipsis, then a parenthesized
// elapsed duration, then the " · " separator before the live token/keybind
// counter — NOT the rotating spinner glyph or the whimsical gerund (both of which
// vary run-to-run and across versions). This shape appears ONLY while Claude is
// actively working (generating, running a tool, or waiting on sub-agents); a
// settled/idle frame shows the past-tense "✻ <verb> for Ns" summary, which has
// no ellipsis or parenthetical and so never matches. Keeping it conservative
// matters: a false match here would hang a genuinely-finished turn.
var workingRE = regexp.MustCompile(`(?:…|\.\.\.)[^\S\r\n]*\(\d+[hms][^)\r\n]*·`)

// PromptNotAccepted implements turns.SwallowedPromptDetector. True when a
// settled screen shows no trace of assistant activity for the in-flight turn:
// no "⏺" message bullet (ExtractMessage fails) and either the screen is
// byte-identical to the one the prompt was submitted on, or it carries no
// "✻ … for Ns" thinking marker anywhere — i.e. Claude Code never accepted the
// prompt and merely repainted its ready screen.
//
// Ported from meta-harness's claude-code adapter, where the behaviour was
// observed live on 2.1.201. The chat layer lets an on-disk transcript overturn
// this verdict, because a repaint that lags the idle gap looks identical.
func (a *Adapter) PromptNotAccepted(snap screen.Snapshot, sentScreenText string) bool {
	if _, ok := a.ExtractMessage(snap); ok {
		return false
	}
	if snap.Text == sentScreenText {
		return true
	}
	return len(thinkingRE.FindAllString(snap.Text, -1)) == 0
}

// Busy reports whether Claude is still working on the current turn, so the chat
// layer's idle-completion fallback (and the turn-complete gate in OnScreen) won't
// complete a turn mid-flight (the "❯" prompt box is painted even while Claude
// works, and the footer can flicker during sub-agent execution). Implements
// turns.BusyDetector.
func (*Adapter) Busy(snap screen.Snapshot) bool {
	return strings.Contains(snap.Text, busyMarker) || workingRE.MatchString(snap.Text)
}

// PermissionMode reports Claude Code's current permission posture as a
// canonical rung (plan|manual|ask|auto|bypass), read off the footer marker in
// the rendered screen. It returns ("", false) when the screen carries no
// readable marker — an onboarding/auth wall, a modal covering the footer, or a
// release that renamed the modes. Implements turns.PermissionModeDetector.
func (*Adapter) PermissionMode(snap screen.Snapshot) (string, bool) {
	return permissionModeFromFooter(snap.Text)
}

// quitCommand is Claude Code's "/quit" slash command followed by its enhanced
// Enter (CSI 13 u). Claude's TUI runs the kitty keyboard protocol, so a plain
// CR does NOT submit the composer — it only inserts a newline and "/quit" never
// runs (see pkg/chat.submitKeyForHarness, which sends the same \x1b[13u for the
// normal Send path). The "/quit" command exits cleanly: Claude flushes its
// transcript and prints the "claude --resume <uuid>" hint we capture on the way
// out (verified live on 2.1.185).
var quitCommand = []byte("/quit\x1b[13u")

// QuitSequence returns Claude Code's graceful-exit keystrokes: the "/quit"
// slash command plus the enhanced Enter that submits it, letting Claude shut
// down cleanly (flushing its transcript) rather than being SIGTERM'd.
// Implements turns.Quitter.
func (*Adapter) QuitSequence() []byte { return quitCommand }

// ExtractSessionIDFromLine scrapes the "claude --resume <uuid>" hint that names
// the on-disk transcript file. Claude prints it to the normal screen as the TUI
// tears down on exit, so it shows up in the raw PTY line stream but NOT in the
// rendered vt100 snapshot — hence this is a turns.RawSessionIDExtractor (raw
// line) rather than a turns.SessionIDExtractor (screen scrape).
func (*Adapter) ExtractSessionIDFromLine(line string) (string, bool) {
	m := resumeRE.FindStringSubmatch(line)
	if len(m) < 2 {
		return "", false
	}
	return m[1], true
}

// ResumeArgs returns the argv fragment that resumes harnessSessionID:
// `claude --resume <uuid>`. Implements turns.SessionResumer.
func (*Adapter) ResumeArgs(harnessSessionID string) []string {
	return []string{"--resume", harnessSessionID}
}

// SessionControlFlags lists the chat-managed session-control flags a caller must
// not pass in Options.args. Implements turns.SessionControlFlags.
func (*Adapter) SessionControlFlags() []string {
	return []string{
		"--session-id",
		"-r",
		"--resume",
		"-c",
		"--continue",
		"--fork-session",
		"--from-pr",
		"--no-session-persistence",
	}
}

// ReadTranscript reads the on-disk Claude Code session log. Implements
// turns.TranscriptReader.
func (*Adapter) ReadTranscript(harnessSessionID, workingDir string) ([]transcript.Turn, error) {
	evs, err := transcriptcc.New().Read(harnessSessionID, workingDir)
	if err != nil {
		return nil, err
	}
	return transcript.TurnsFromEvents(evs), nil
}
