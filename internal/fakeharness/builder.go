package fakeharness

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// defaultSessionID is a fixed UUID so fixtures are deterministic; override with
// Builder.Session. It matches the shape claudecode.resumeRE / codex.resumeRE
// expect so the chat layer's session-id extraction is exercised too.
const defaultSessionID = "11111111-2222-3333-4444-555555555555"

// Builder assembles a Script with harness-appropriate screen frames. The
// semantic methods (Idle, Working, Marker, Flicker, Reply, SettleIdle) stamp the
// exact glyphs the corresponding adapter keys off of, kept in one place so a
// future TUI drift updates fixtures and adapter patterns together.
//
// Methods chain; terminate with Build. The glyph helpers below cover
// claude-code (the harness whose completion timing these fixtures regression-
// lock) in most depth, but codex and pi each have a full turn vocabulary too:
// CodexWorking/CodexReply and PiWorking/PiReply drive a turn to completion, not
// just to readiness.
type Builder struct {
	s Script
	// box paints claude-code frames with the composer box claude 2.1.270+
	// draws; see ComposerBox.
	box bool
}

// New starts a Builder for the named harness with the default session ID.
func New(harness string) *Builder {
	return &Builder{s: Script{Harness: harness, SessionID: defaultSessionID}}
}

// Session overrides the session UUID emitted in the resume hint.
func (b *Builder) Session(id string) *Builder { b.s.SessionID = id; return b }

// Build returns the assembled Script.
func (b *Builder) Build() Script { return b.s }

func (b *Builder) frame(delayMs int, screen string, echo bool) *Builder {
	b.s.Steps = append(b.s.Steps, Step{Frame: &Frame{DelayMs: delayMs, Screen: screen, Echo: echo}})
	return b
}

func (b *Builder) waitInput(re string, capture bool, label string) *Builder {
	b.s.Steps = append(b.s.Steps, Step{WaitInput: &WaitInput{UntilRegex: re, Capture: capture, Label: label}})
	return b
}

// Exit appends a step that terminates the fake with the given code.
func (b *Builder) Exit(code int) *Builder {
	b.s.Steps = append(b.s.Steps, Step{Exit: &Exit{Code: code}})
	return b
}

// AwaitSubmit blocks until the wrapper submits a turn (CSI 13u) and captures the
// typed text as the prompt for later Echo frames.
func (b *Builder) AwaitSubmit() *Builder {
	return b.waitInput(regexp.QuoteMeta(SubmitCSI13u), true, "submit")
}

// AwaitInterrupt blocks until the wrapper writes the interrupt key
// (InterruptCSI27u). It does not capture: the keypress carries no prompt text.
func (b *Builder) AwaitInterrupt() *Builder {
	return b.waitInput(regexp.QuoteMeta(InterruptCSI27u), false, "interrupt")
}

// AwaitComposerClear blocks until the wrapper writes the keys that empty a
// composer of lines lines (ClearComposerKeys).
func (b *Builder) AwaitComposerClear(lines int) *Builder {
	return b.waitInput(regexp.QuoteMeta(ClearComposerKeys(lines)), false, "composer-clear")
}

// AwaitMenuChoice blocks until the wrapper selects a menu row (a digit followed
// by CR, the keys claudecode encodes for trust-dialog options).
func (b *Builder) AwaitMenuChoice() *Builder {
	return b.waitInput(`[0-9]\r`, false, "menu-choice")
}

// AwaitShiftTab blocks until the wrapper presses Shift+Tab (CSI 9;2u — the
// permission-mode cycle key for claude-code / codex). It does not capture: the
// keypress carries no prompt text. regexp.QuoteMeta escapes the ESC, "[" and
// ";" bytes, so the pattern matches the exported constant literally and NOT the
// legacy "\x1b[Z" form or a bare tab. Pins the Shift+Tab contract the way
// AwaitSubmit pins CSI 13u.
func (b *Builder) AwaitShiftTab() *Builder {
	return b.waitInput(regexp.QuoteMeta(ShiftTabCSI9_2u), false, "shift-tab")
}

// AwaitSubmitCR blocks until the wrapper submits a turn with a bare carriage
// return (pi's submit key) and captures the typed text as the prompt for later
// Echo frames. Pins pi's submit contract the way AwaitSubmit pins CSI 13u.
func (b *Builder) AwaitSubmitCR() *Builder {
	return b.waitInput(regexp.QuoteMeta(SubmitCR), true, "submit-cr")
}

// --- claude-code screen vocabulary --------------------------------------
//
// busyMarker:  "esc to interrupt"           → claudecode busyMarker
// spinner:     "… (3s · ↓ …)"               → claudecode workingRE (ellipsis+dur+·)
// end marker:  "✻ <Verb> for <dur>"         → claudecode thinkingRE (fires TurnComplete)
// reply:       "⏺ <text>"                   → claudecode bulletRE (message body)
// ready:       "Claude Code" + "❯"          → readyForInput

const (
	ccHeader  = "Claude Code"
	ccPrompt  = "❯ "
	ccBusy    = "  ⏵⏵ esc to interrupt" // contains busyMarker substring
	ccSpinner = "✶ Cerebrating… (3s · ↓ 1.2k tokens)"
)

func (b *Builder) ccScreen(lines ...string) string {
	if b.box {
		lines = boxComposer(lines)
	}
	return strings.Join(lines, "\n") + "\n"
}

// ComposerBox paints every later claude-code frame the way claude 2.1.270+
// lays it out: the composer between two horizontal rules, the status line
// (spinner, retry countdown, end-of-turn summary) just above the box and the
// footer just below it. The adapter's Busy reads only that region, so a
// scenario that must tell a working Claude from a reply quoting the working
// markers opts in; without it Busy judges the whole screen.
func (b *Builder) ComposerBox() *Builder { b.box = true; return b }

// ccRule is one rule of the composer box.
var ccRule = strings.Repeat("─", 100)

// boxComposer wraps the composer line of a frame in the box's two rules.
func boxComposer(lines []string) []string {
	out := make([]string, 0, len(lines)+2)
	for _, ln := range lines {
		if ln == ccPrompt {
			out = append(out, ccRule, ln, ccRule)
			continue
		}
		out = append(out, ln)
	}
	return out
}

func (b *Builder) resumeHint() string {
	if b.s.Harness == "codex" {
		return "  codex resume " + b.s.SessionID
	}
	return "  claude --resume " + b.s.SessionID
}

// Idle paints the startup composer: ready for input, not busy. It MUST be the
// first step, because chat.Send's readiness gate (readyForInput) blocks until
// the screen shows the harness is accepting input.
func (b *Builder) Idle() *Builder {
	if b.s.Harness == "codex" {
		return b.frame(0, b.ccScreen("Codex", "", "› ", "", b.resumeHint()), false)
	}
	return b.frame(0, b.ccScreen(ccHeader, "", ccPrompt, "", b.resumeHint()), false)
}

// Working paints an in-flight frame: the spinner and the "esc to interrupt"
// footer are both present, so the adapter reports Busy() == true. status names
// the spinner verb shown to the user (cosmetic).
func (b *Builder) Working(delayMs int, status string) *Builder {
	spinner := strings.Replace(ccSpinner, "Cerebrating", status, 1)
	return b.frame(delayMs, b.ccScreen(ccHeader, "", spinner, "", ccPrompt, ccBusy), false)
}

// Marker paints an INTERMEDIATE end-of-turn summary: the "✻ <verb> for <dur>"
// line (which fires a TurnComplete event) while the frame is STILL busy (footer
// present). This models Claude printing that summary after a thinking block
// mid-turn — it must defer, not complete.
func (b *Builder) Marker(delayMs int, verb, dur string) *Builder {
	return b.frame(delayMs, b.ccScreen(ccHeader, "", "✻ "+verb+" for "+dur, ccSpinner, "", ccPrompt, ccBusy), false)
}

// RetryBackoff paints Claude backing off before retrying a failed API call, as
// recorded on 2.1.280: "✻ API error · Retrying in <seconds>s · attempt
// <attempt>/10" in the status line, and a footer WITHOUT "esc to interrupt",
// which a live run showed absent for the whole backoff. Busy must read it off
// the status line.
func (b *Builder) RetryBackoff(delayMs, seconds, attempt int) *Builder {
	status := fmt.Sprintf("✻ API error · Retrying in %ds · attempt %d/10", seconds, attempt)
	return b.frame(delayMs, b.ccScreen(ccHeader, "", status, "", ccPrompt, "  ⏵⏵ auto mode on (shift+tab to cycle) · ← 1 agent"), false)
}

// Flicker paints the danger frame: the footer AND spinner are absent for one
// redraw (so Busy() is momentarily false) while a sub-agent line shows work is
// not actually done. note is the sub-agent label; it must avoid the spinner
// shape (ellipsis + "(Ns ·") so workingRE does not match.
func (b *Builder) Flicker(delayMs int, note string) *Builder {
	return b.frame(delayMs, b.ccScreen(ccHeader, "", "⏺ "+note, "  running Explore sub-agent", "", ccPrompt), false)
}

// MarkerFlicker is the exact trigger for the bug fixed in 3eda8a8: a "✻ <verb>
// for <dur>" summary (which fires a TurnComplete event) on a frame where the
// busy footer AND spinner have flickered off for one redraw — so the marker
// arrives while Busy() is momentarily false, MID-turn. The old code completed
// instantly here, capturing this pre-final frame; the fix defers to quiescence,
// and because more work follows within markerConfirmGap, the turn correctly
// stays in flight. note must avoid the spinner shape so workingRE stays false.
func (b *Builder) MarkerFlicker(delayMs int, verb, dur, note string) *Builder {
	return b.frame(delayMs, b.ccScreen(ccHeader, "", "✻ "+verb+" for "+dur, "⏺ "+note, "  running Explore sub-agent", "", ccPrompt), false)
}

// Reply paints the genuine end-of-turn frame: the assistant bullet (echoing the
// captured prompt if body contains the placeholder), the FINAL "✻ <verb> for
// <dur>" marker, a settled prompt, and NO busy signal. After this frame the fake
// goes quiet, so the idle watcher confirms the marker and completes the turn
// with this frame's text. Use a distinct verb/dur from any intermediate Marker
// so the adapter does not dedupe the two summaries.
func (b *Builder) Reply(delayMs int, body, verb, dur string) *Builder {
	// The resume hint rides along on the settled frame so session-id extraction
	// (which scans the screen at TurnComplete) sees it — real claude-code keeps
	// the affordance painted.
	return b.frame(delayMs, b.ccScreen(ccHeader, "", "⏺ "+body, "", "✻ "+verb+" for "+dur, "", ccPrompt, b.resumeHint()), true)
}

// Interrupt frames, recorded on claude 2.1.280 (ADR-007). They paint the
// composer box whether or not ComposerBox was called: the interrupt reading
// locates the conversation above it. The submitted prompt's echo — "❯ " at
// column 0 — carries the captured prompt.
const ccInterruptMarker = "  ⎿ \u00a0Interrupted · What should Claude do instead?" // U+00A0 after the space, as claude paints it

// EchoWorking paints a turn in flight: the prompt's echo, the spinner, and the
// busy footer.
func (b *Builder) EchoWorking(delayMs int, status string) *Builder {
	spinner := strings.Replace(ccSpinner, "Cerebrating", status, 1)
	return b.frame(delayMs, b.ccBoxed(ccHeader, "", ccPrompt+promptPlaceholder, spinner, "", ccPrompt, ccBusy), true)
}

// Stopped paints a turn the harness stopped: the prompt's echo, the partial
// reply (none when partial is empty), the interrupt marker below it, and an
// empty composer. Prior lines are painted above the echo — an earlier turn,
// for instance, with its own marker.
func (b *Builder) Stopped(delayMs int, partial string, prior ...string) *Builder {
	lines := append([]string{ccHeader, ""}, prior...)
	lines = append(lines, ccPrompt+promptPlaceholder)
	if partial != "" {
		lines = append(lines, "⏺ "+partial)
	}
	lines = append(lines, ccInterruptMarker, "", ccPrompt, b.resumeHint())
	return b.frame(delayMs, b.ccBoxed(lines...), true)
}

// Cancelled paints a turn the harness cancelled before its first token: the
// echo gone and the prompt back in the composer, not busy. Prior lines are
// painted above the composer.
func (b *Builder) Cancelled(delayMs int, prior ...string) *Builder {
	lines := append([]string{ccHeader, ""}, prior...)
	lines = append(lines, "", ccComposer+promptPlaceholder, b.resumeHint())
	return b.frame(delayMs, b.ccBoxed(lines...), true)
}

// ComposerHolding paints a settled, ready frame whose composer holds text — a
// draft typed at the terminal, say.
func (b *Builder) ComposerHolding(delayMs int, text string) *Builder {
	return b.frame(delayMs, b.ccBoxed(ccHeader, "", ccComposer+text, b.resumeHint()), false)
}

// EmptyComposer paints the settled, ready frame with an empty composer box.
func (b *Builder) EmptyComposer(delayMs int) *Builder {
	return b.frame(delayMs, b.ccBoxed(ccHeader, "", ccPrompt, b.resumeHint()), false)
}

// Paint paints a claude-code frame line by line, the composer box drawn
// around the empty composer line ("❯ ") or a ComposerLine, with PromptRef()
// replaced by the captured prompt — for a scenario the semantic frames do not
// cover.
func (b *Builder) Paint(delayMs int, lines ...string) *Builder {
	return b.frame(delayMs, b.ccBoxed(lines...), true)
}

// ComposerLine returns a Paint line for a composer holding text.
func ComposerLine(text string) string { return ccComposer + text }

// InterruptMarkerLine returns the interrupt marker line as claude paints it,
// for a Paint frame.
func InterruptMarkerLine() string { return ccInterruptMarker }

// ccComposer marks a composer line holding text, for ccBoxed: the ❯ glyph
// followed by a zero-width tag, stripped when painted.
const ccComposer = "❯ \x00"

// ccBoxed is ccScreen with the composer box always drawn: the empty composer
// line, or one tagged ccComposer, gets the box's two rules around it.
func (b *Builder) ccBoxed(lines ...string) string {
	out := make([]string, 0, len(lines)+2)
	for _, ln := range lines {
		switch {
		case ln == ccPrompt:
			out = append(out, ccRule, ln, ccRule)
		case strings.HasPrefix(ln, ccComposer):
			out = append(out, ccRule, ccPrompt+strings.TrimPrefix(ln, ccComposer), ccRule)
		default:
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n") + "\n"
}

// SettleIdle paints a settled, ready, non-busy frame with a reply bullet but NO
// "✻" marker. It models a turn whose end-of-turn marker was missed: completion
// must fall back to the idle path, which requires readyForInput. body is the
// reply text (echoed if it contains the placeholder).
func (b *Builder) SettleIdle(delayMs int, body string) *Builder {
	return b.frame(delayMs, b.ccScreen(ccHeader, "", "⏺ "+body, "", ccPrompt, b.resumeHint()), true)
}

// PromptRef returns the placeholder a scenario embeds in a reply body to have
// the captured prompt substituted at paint time.
func PromptRef() string { return promptPlaceholder }

// CapturedArgv runs the compiled fake binary at bin with the given args, having
// it dump its launch argv via ArgvOutVar, then reads that dump back and returns
// the decoded JSON array. It lets a conformance test assert argv prepending: the
// args the caller passes here are exactly what the fake should observe as
// os.Args[1:].
//
// The fake runs against a throwaway one-step script (an immediate Exit) so it
// terminates without a PTY — argv is dumped before script loading, so it is
// captured regardless. Any run error is surfaced only if the argv file is then
// missing, since the dump itself is what the test cares about.
func CapturedArgv(bin string, args ...string) ([]string, error) {
	dir, err := os.MkdirTemp("", "fakeharness-argv")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	scriptPath := filepath.Join(dir, "script.json")
	scriptData, err := json.Marshal(New("claude-code").Exit(0).Build())
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(scriptPath, scriptData, 0o644); err != nil {
		return nil, err
	}

	argvPath := filepath.Join(dir, "argv.json")
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), EnvVar+"="+scriptPath, ArgvOutVar+"="+argvPath)
	_ = cmd.Run() // exit code is irrelevant; the argv dump is best-effort but pre-exit.

	raw, err := os.ReadFile(argvPath)
	if err != nil {
		return nil, fmt.Errorf("read argv dump: %w", err)
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, fmt.Errorf("decode argv dump: %w", err)
	}
	return got, nil
}

// --- codex screen vocabulary --------------------------------------------
//
// Codex differs from claude-code: it has no Busy() detector and no quiescence
// dance — the chat layer completes a codex turn INSTANTLY when a new "Token
// usage: …" footer appears (codex.tokenUsageRE). Readiness is the "›" composer
// prompt (codex.PromptReady); the submit key is still CSI 13u. So a codex turn
// is just: ready → submit → (work) → a frame carrying a fresh Token-usage line.

const codexPrompt = "› " // matches codex.promptRE; readyForInput for codex

// CodexWorking paints an in-flight codex frame: a status line, no composer
// prompt, and crucially no Token-usage footer (so the turn does not complete)
// and none of codex's interstitial anchors (so DetectInput stays false).
func (b *Builder) CodexWorking(delayMs int, status string) *Builder {
	return b.frame(delayMs, b.ccScreen("Codex", "", "• "+status+"…", ""), false)
}

// CodexReply paints the end-of-turn codex frame: the reply body (echoing the
// captured prompt), a fresh "Token usage: …" footer that fires the instant
// TurnComplete, and the composer prompt + resume hint (so session-id extraction
// works and a subsequent turn's readiness gate passes). The footer numbers are
// derived from the step index so consecutive replies have DISTINCT fingerprints
// — codex dedupes by exact footer string, so a repeated line would not re-fire.
func (b *Builder) CodexReply(delayMs int, body string) *Builder {
	n := len(b.s.Steps) + 1
	tokenUsage := fmt.Sprintf("Token usage: total=%d input=%d (+ 0 cached) output=%d", 1000*n, 800*n, 200*n)
	return b.frame(delayMs, b.ccScreen("Codex", "", body, "", tokenUsage, "", codexPrompt, b.resumeHint()), true)
}

// --- pi screen vocabulary -----------------------------------------------
//
// pi differs from both claude-code and codex: no end-of-turn screen marker and
// no kitty keyboard protocol. A turn completes via the chat layer's busy-aware
// idle fallback, so the vocabulary only needs a ready idle status line
// (pi.PromptReady — the "<pct>%/<ctx>k" context indicator), a busy spinner
// ("Working..." → pi.Busy), and a settled reply frame. Submit is a bare CR.

const (
	piStatus  = "↑1.2k ↓32 $0.000 0.9%/131k (auto)                      gpt-oss-120b • medium"
	piRule    = "────────────────────────────────────────"
	piSpinner = " ⠧ Working..." // contains pi.busyTexts "Working..."
)

// frameLines joins screen lines like ccScreen but without the claude-specific
// name, for the pi vocabulary.
func (b *Builder) frameLines(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// PiIdle paints pi's idle composer: the context-usage status line is up
// (pi.PromptReady true) and no spinner (not busy). MUST be the first step so
// chat.Send's readiness gate passes.
func (b *Builder) PiIdle() *Builder {
	return b.frame(0, b.frameLines(piRule, "", piRule, "~/proj (main)", piStatus), false)
}

// PiWorking paints an in-flight pi frame: the "Working..." spinner makes pi.Busy
// true, so the idle-completion fallback defers instead of closing the turn.
func (b *Builder) PiWorking(delayMs int) *Builder {
	return b.frame(delayMs, b.frameLines(piSpinner, "", piRule, "", piRule, piStatus), false)
}

// PiReply paints the settled end-of-turn pi frame: the reply body (echoing the
// captured prompt) and the idle status line, with NO spinner — so once the
// screen goes quiet the busy-aware idle fallback completes the turn with this
// frame's text. pi surfaces no on-screen session id, so no resume hint rides
// along (interactive History stays store-backed until a session id is known).
func (b *Builder) PiReply(delayMs int, body string) *Builder {
	return b.frame(delayMs, b.frameLines(body, "", piRule, "", piRule, "~/proj (main)", piStatus), true)
}

// --- raw output & lifecycle (wrapper-level scenarios) -------------------

// Raw emits text verbatim (with a trailing newline, no clear/home) so the
// wrapper's line classifier sees it as a clean line. Use for signals the
// classifier keys off the byte stream rather than the rendered screen — e.g. an
// "API Error: 429 …" line that must surface HTTPCode/RetryAfter on the turn, or
// a resume hint for session-id extraction. Echo substitutes the captured prompt.
func (b *Builder) Raw(delayMs int, text string) *Builder {
	b.s.Steps = append(b.s.Steps, Step{Frame: &Frame{DelayMs: delayMs, Screen: text + "\n", Echo: true, NoClear: true}})
	return b
}

// StayAliveUntilStopped holds the fake at its prompt after the scripted timeline
// — like a real interactive harness waiting for the next message — until the
// wrapper terminates it. In a one-shot (ExitAfterTurn) scenario this is what
// makes RunTurn's best-effort graceful quit time out and conv.Close SIGTERM the
// process, so WrapperResult.Status ends up StatusInterrupted. This models a
// harness that IGNORES the graceful quit (as claude <=2.1.217 did), one of the
// two shapes real harnesses take; claude >=2.1.245 exits on /quit instead — see
// QuitsOnQuit for that branch, and TestRunTurn_RealClaudeDogfood, which accepts
// either. Append it last.
//
// The binary also holds this way by default once the timeline ends (see
// cmd/fakeharness); this just states the intent explicitly at the call site.
func (b *Builder) StayAliveUntilStopped() *Builder {
	b.s.Steps = append(b.s.Steps, Step{Hold: &Hold{}})
	return b
}

// QuitsOnQuit holds the fake until the wrapper's graceful quit sequence arrives,
// then exits 0 — modelling a harness that HONORS the quit (claude >=2.1.245).
// In an ExitAfterTurn scenario this makes RunTurn's gracefulQuit succeed before
// gracefulQuitWait elapses, so conv.Close never needs to signal and
// WrapperResult.Status is StatusIdle with the harness's own exit code. This is
// the counterpart to StayAliveUntilStopped; append one or the other last, never
// both (Hold drains stdin and would swallow the quit).
func (b *Builder) QuitsOnQuit() *Builder {
	return b.waitInput(`/quit`, false, "quit").Exit(0)
}

// --- claude-code transcript records -------------------------------------
//
// The records a real claude-code session appends to its transcript, reduced to
// the fields pkg/transcript/claudecode reads; test/corpus/apierror holds real
// ones. They land where the launch's session keeps its transcript (see
// Transcript), so a scenario can give the chat layer the harness's own record
// of a turn — the source the transcript verdicts and History read.

// transcriptTimestamp stamps every record. The readers order by file position,
// not by time, so one fixed instant keeps fixtures deterministic.
const transcriptTimestamp = "2026-09-23T00:00:00.000Z"

// TranscriptUser appends the user record for the captured prompt.
func (b *Builder) TranscriptUser(delayMs int) *Builder {
	return b.transcript(delayMs, map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": promptPlaceholder},
	})
}

// TranscriptReply appends an assistant record whose reply is text.
func (b *Builder) TranscriptReply(delayMs int, text string) *Builder {
	return b.transcript(delayMs, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant", "model": "claude-fake",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
	})
}

// TranscriptInterrupted appends the record claude-code writes when a turn is
// interrupted (recorded on 2.1.280): a user entry "[Request interrupted by
// user]", or "… for tool use]" after a tool call it stopped.
func (b *Builder) TranscriptInterrupted(delayMs int, forToolUse bool) *Builder {
	text := "[Request interrupted by user]"
	if forToolUse {
		text = "[Request interrupted by user for tool use]"
	}
	return b.transcript(delayMs, map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user", "content": []map[string]any{{"type": "text", "text": text}},
		},
	})
}

// TranscriptAPIError appends the synthetic assistant record claude-code writes
// when an API call fails: model "<synthetic>", the rendered error as its text,
// isApiErrorMessage set and the machine-readable error tag (server_error,
// rate_limit, billing_error, …).
func (b *Builder) TranscriptAPIError(delayMs int, tag, text string) *Builder {
	return b.transcript(delayMs, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role": "assistant", "model": "<synthetic>",
			"content": []map[string]any{{"type": "text", "text": text}},
		},
		"isApiErrorMessage": true,
		"error":             tag,
	})
}

// TranscriptRaw appends records verbatim, for a scenario that needs fields the
// helpers above do not write (usage, message ids, tool blocks).
func (b *Builder) TranscriptRaw(delayMs int, lines ...string) *Builder {
	b.s.Steps = append(b.s.Steps, Step{Transcript: &Transcript{DelayMs: delayMs, Lines: lines}})
	return b
}

// transcript appends a Transcript step holding one record, numbering its uuid
// by its position in the script so every record's id is distinct.
func (b *Builder) transcript(delayMs int, record map[string]any) *Builder {
	record["uuid"] = fmt.Sprintf("00000000-0000-4000-8000-%012d", len(b.s.Steps))
	record["timestamp"] = transcriptTimestamp
	line, err := json.Marshal(record)
	if err != nil {
		panic(fmt.Sprintf("fakeharness: marshal transcript record: %v", err))
	}
	b.s.Steps = append(b.s.Steps, Step{Transcript: &Transcript{DelayMs: delayMs, Lines: []string{string(line)}}})
	return b
}
