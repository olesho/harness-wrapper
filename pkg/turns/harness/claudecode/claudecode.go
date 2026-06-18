// Package claudecode provides a turn-detection adapter for Anthropic's
// Claude Code CLI (claude / @anthropic-ai/claude-code).
//
// Detection signals observed on 2.1.141:
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
// Examples that do NOT match (and used to mis-fire): the same
// pattern surrounded by non-whitespace on the same line (e.g. the in-progress
// "✻ Cooking… (1m 22s · esc to interrupt)" indicator, which has trailing
// content the end anchor rejects).
var thinkingRE = regexp.MustCompile(`(?m)^[^\S\r\n]*(✻ \p{Lu}\p{L}+ for \d+[hms](?: \d+[hms])*)[^\S\r\n]*$`)

// resumeRE matches the "claude --resume <uuid>" hint Claude Code prints
// when it ends a session. The UUID names the on-disk transcript file.
var resumeRE = regexp.MustCompile(`claude --resume ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// interruptMarker is the literal text Claude Code writes after the user
// interrupts a streaming reply (Esc / Ctrl-C). Claude Code uses U+23BF
// (⎿), then a regular ASCII space, then U+00A0 (non-breaking space),
// then "Interrupted · ...". The NBSP is easy to miss — match it exactly.
const interruptMarker = "⎿  Interrupted · What should Claude do instead?"

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
		out = append(out, turns.Event{Kind: turns.Errored, Reason: "claude-code: " + interruptMarker})
	}
	a.lastInterruptSeen = interruptNow

	// Turn-complete detection — newest thinking marker differs from last fired.
	// Capture group 1 holds the marker text without surrounding column
	// padding so the fingerprint stays stable across redraws.
	matches := thinkingRE.FindAllStringSubmatch(snap.Text, -1)
	if len(matches) > 0 {
		latest := matches[len(matches)-1][1]
		if latest != a.lastFingerprint {
			a.lastFingerprint = latest
			out = append(out, turns.Event{Kind: turns.TurnComplete, Reason: "claude-code: " + latest})
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
			out = append(out, turns.Event{Kind: turns.InputRequested, Reason: "claude-code: " + req.Prompt, Input: req})
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
	var prompt string
	switch {
	case strings.Contains(text, trustAnchor):
		prompt = trustAnchor
	case strings.Contains(text, trustAnchorAlt):
		prompt = trustAnchorAlt
	case strings.Contains(text, bypassAnchor):
		prompt = bypassAnchor
	default:
		return nil, false
	}
	opts := parseMenuOptions(text)
	if len(opts) == 0 {
		// Anchor visible but the menu hasn't rendered yet — not actionable.
		return nil, false
	}
	req := &turns.InputRequest{Kind: "trust_prompt", Prompt: prompt, Options: opts}
	req.ID = inputID(req)
	return req, true
}

// parseMenuOptions extracts the numbered choices, de-duplicating by choice
// number so a redraw that paints the menu twice yields one option set.
func parseMenuOptions(text string) []turns.InputOption {
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
	limit := len(lines)
	for i, ln := range lines {
		if thinkingRE.MatchString(ln) {
			limit = i // keep the LAST footer's index
		}
	}

	start := -1
	for i := 0; i < limit; i++ {
		if bulletRE.MatchString(lines[i]) {
			start = i
		}
	}
	if start < 0 {
		// No bullet before the footer (or no footer): fall back to the last
		// bullet anywhere on screen.
		for i, ln := range lines {
			if bulletRE.MatchString(ln) {
				start = i
			}
		}
	}
	if start < 0 {
		return "", false
	}

	m := bulletRE.FindStringSubmatch(lines[start])
	block := []string{strings.TrimRight(m[1], " ")}

	// Consume indented continuation lines until a boundary.
	for i := start + 1; i < len(lines); i++ {
		ln := lines[i]
		if bulletRE.MatchString(ln) || toolResultRE.MatchString(ln) || boxOrRuleRE.MatchString(ln) {
			break
		}
		if thinkingRE.MatchString(ln) {
			break
		}
		if strings.HasPrefix(strings.TrimLeft(ln, " "), "❯") {
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

	// block[0] is already flush (the regex consumed the "⏺ " prefix). The
	// continuation lines are indented to align under that text, so dedent them
	// on their own before rejoining — otherwise the flush first line pins the
	// common indent at 0 and the continuations keep their alignment padding.
	msg := block[0]
	if len(block) > 1 {
		if tail := dedent(block[1:]); tail != "" {
			msg += "\n" + tail
		}
	}
	if strings.TrimSpace(msg) == "" {
		return "", false
	}
	return msg, true
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

// QuitSequence returns Claude Code's graceful-exit keys: two Ctrl-C presses
// (the first arms "press again to exit", the second quits), letting Claude
// shut down cleanly rather than being SIGTERM'd. Implements turns.Quitter.
func (*Adapter) QuitSequence() []byte { return []byte{0x03, 0x03} }

// ExtractSessionID scrapes the "claude --resume <uuid>" hint that
// identifies the on-disk transcript file. Implements turns.SessionIDExtractor.
func (*Adapter) ExtractSessionID(snap screen.Snapshot) (string, bool) {
	m := resumeRE.FindStringSubmatch(snap.Text)
	if len(m) < 2 {
		return "", false
	}
	return m[1], true
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
