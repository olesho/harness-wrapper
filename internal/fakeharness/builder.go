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
type Builder struct{ s Script }

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
	return strings.Join(lines, "\n") + "\n"
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
// process, so WrapperResult.Status ends up StatusInterrupted — matching real
// Claude Code (see TestRunTurn_RealClaudeDogfood). Append it last.
//
// The binary also holds this way by default once the timeline ends (see
// cmd/fakeharness); this just states the intent explicitly at the call site.
func (b *Builder) StayAliveUntilStopped() *Builder {
	b.s.Steps = append(b.s.Steps, Step{Hold: &Hold{}})
	return b
}
