// Package fakeharness provides a scriptable stand-in for an interactive coding
// harness (claude-code, codex). It is a REAL program: cmd/fakeharness compiles
// it to a binary that chat.Open spawns over a real PTY, so replaying a script
// drives the genuine screen emulator, turn watcher, and idle-completion timers
// end to end. That exercises the timing-sensitive completion logic — Busy()
// flicker while sub-agents run, end-of-turn-marker quiescence — that
// adapter-level corpus replay cannot reach because it bypasses the conversation
// loop and its wall-clock timers.
//
// A test builds a Script with the fluent Builder, marshals it to JSON, and
// points the binary at it via the FAKEHARNESS_SCRIPT env var. The binary reads
// the script, switches its PTY slave to raw mode (as a real TUI does), and
// replays the timeline: paint frames on a delay, block until the wrapper types
// an expected byte sequence, optionally echo the captured prompt back.
package fakeharness

// EnvVar is the environment variable cmd/fakeharness reads the script-file path
// from. The chat test helper sets it when spawning the binary.
const EnvVar = "FAKEHARNESS_SCRIPT"

// ArgvOutVar is the environment variable cmd/fakeharness dumps its launch argv
// (os.Args[1:]) to, as a single JSON array, when set. It mirrors the TS fake's
// ArgvOutVar (meta-harness/test/chat/fakeharness.ts) so conformance tests can
// read back the exact args the wrapper spawned the fake with — asserting argv
// prepending. See Builder-side CapturedArgv for the read-back helper.
const ArgvOutVar = "FAKEHARNESS_ARGV_OUT"

// SubmitCSI13u is the byte sequence chat.Send writes to submit a turn for both
// claude-code and codex: CSI 13 u, the unmodified Enter key in the kitty /
// enhanced keyboard protocol those TUIs enable at startup. Scenarios wait for
// it via Builder.AwaitSubmit, which also pins the submit-key contract — if the
// wrapper ever stops sending exactly this, the fake never advances and the test
// fails loudly.
const SubmitCSI13u = "\x1b[13u"

// SubmitCR is the byte chat.Send writes to submit a turn for pi: a bare carriage
// return. pi does not enable the kitty keyboard protocol (unlike claude-code /
// codex), so plain Enter submits. Scenarios wait for it via Builder.AwaitSubmitCR,
// which pins the pi submit-key contract — if the wrapper stops sending exactly
// this, the fake never advances and the test fails loudly.
const SubmitCR = "\r"

// ShiftTabCSI9_2u is the byte sequence chat writes to press Shift+Tab — the key
// claude-code and codex bind to "cycle permission mode" — in the kitty /
// enhanced keyboard protocol those TUIs enable at startup: CSI 9 ; 2 u (Tab
// codepoint 9, Shift modifier 2), rather than the legacy "\x1b[Z". The inward
// contract lives in pkg/chat.shiftTabForHarness, whose doc comment records the
// live measurements behind that choice; this constant mirrors it so hermetic
// scenarios drive the fake with exactly the bytes the production writer emits.
// Scenarios wait for it via Builder.AwaitShiftTab — if the wrapper ever stops
// sending exactly this, the fake never advances and the test fails loudly.
const ShiftTabCSI9_2u = "\x1b[9;2u"

// PasteStart / PasteEnd are the bracketed-paste framing markers chat wraps a
// LARGE composer payload in — CSI 200 ~ and CSI 201 ~, what a real terminal
// emits around pasted text. A real TUI consumes them as FRAMING and keeps only
// what is between them, so cmd/fakeharness strips them from the captured prompt
// before {{prompt}} echoes it back; that strip is what lets a round-trip
// scenario prove no byte of a >=1KB prompt was lost. The inward contract lives
// in pkg/chat.pasteWrapForHarness, whose doc comment records the live
// measurement behind it; these constants mirror it so the fake and this
// production writer cannot drift, and TestPasteWrapMatchesFakeharness pins them
// byte-equal. If the wrapper ever stops sending exactly these, the strip becomes
// a no-op and the round-trip test fails loudly.
const (
	PasteStart = "\x1b[200~"
	PasteEnd   = "\x1b[201~"
)

// promptPlaceholder is substituted with the captured prompt in any Frame whose
// Echo is set. It lets a scenario assert a round-trip: the exact text the
// wrapper submitted reappears verbatim in the harness's reply.
const promptPlaceholder = "{{prompt}}"

// Script is the timeline the fake replays. It crosses the process boundary as
// JSON, so every field is exported and JSON-tagged.
type Script struct {
	// Harness selects the screen vocabulary the Builder stamps ("claude-code"
	// or "codex"). The binary itself is harness-agnostic — it only replays
	// frames and matches input — so this is consumed at build time, not replay.
	Harness   string `json:"harness"`
	SessionID string `json:"session_id,omitempty"`
	Steps     []Step `json:"steps"`
}

// Step is exactly one of: paint a Frame, WaitInput for typed bytes, Hold at the
// prompt until the wrapper stops the process, or Exit.
type Step struct {
	Frame     *Frame     `json:"frame,omitempty"`
	WaitInput *WaitInput `json:"wait_input,omitempty"`
	Hold      *Hold      `json:"hold,omitempty"`
	Exit      *Exit      `json:"exit,omitempty"`
}

// Frame is a full-screen repaint. The binary prefixes every frame with a
// clear+home so Screen never retains stale content (e.g. a lingering "esc to
// interrupt" footer bleeding into a settled frame and faking Busy()).
type Frame struct {
	// DelayMs is wall-clock sleep BEFORE painting. This is how a scenario
	// reproduces a cadence: an intra-turn delay shorter than markerConfirmGap
	// keeps the idle watcher's timer resetting (no premature completion), while
	// the quiet that follows the final frame lets it elapse and complete.
	DelayMs int    `json:"delay_ms"`
	Screen  string `json:"screen"`
	// Echo replaces the prompt placeholder with the last captured input before
	// painting, so the reply can carry the submitted text back verbatim.
	Echo bool `json:"echo,omitempty"`
	// NoClear emits Screen verbatim without the clear+home prefix — i.e. append
	// to the byte stream rather than repaint. Used for classifier lines (see
	// Builder.Raw) that must reach the wrapper's line tap as clean lines.
	NoClear bool `json:"no_clear,omitempty"`
}

// WaitInput blocks replay until the bytes the wrapper has typed match
// UntilRegex. Because the PTY slave is in raw mode, the match sees control
// bytes (the CSI-13u submit) directly, with no line buffering.
type WaitInput struct {
	UntilRegex string `json:"until_regex"`
	// Capture stashes the bytes received BEFORE the match as the prompt for
	// later Echo frames (i.e. the submitted text, minus the submit key).
	Capture bool   `json:"capture,omitempty"`
	Label   string `json:"label,omitempty"`
}

// Exit terminates the fake with Code, modelling a harness that crashes or quits
// mid-session.
type Exit struct {
	Code int `json:"code"`
}

// Hold keeps the fake at its prompt after the timeline — like a real interactive
// harness waiting for the next message — and blocks until the wrapper closes the
// PTY (Conversation.Close, which SIGTERMs it). It makes the "stay alive until
// stopped" intent explicit in the script; the binary also holds this way by
// default once the timeline ends.
type Hold struct{}
