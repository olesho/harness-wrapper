// Package pi provides a turn-detection adapter for the pi coding agent
// (@earendil-works/pi-coding-agent, binary "pi" —
// github.com/earendil-works/pi).
//
// Capability state (verified against pi 0.76.0 from the shipped docs/source;
// the interactive-screen signals still await a recorded corpus):
//
//   - Transcript reading: implemented. pi's on-disk session format is
//     documented and versioned (JSONL, format v3), so ReadTranscript is
//     wired to pkg/transcript/pi and fires as soon as a harness session ID
//     is known.
//
//   - Graceful quit: implemented. pi exposes a "/quit" slash command (see
//     core/slash-commands.js) that exits cleanly, flushing the session it
//     auto-saves — so QuitSequence sends it instead of leaving the chat layer
//     to SIGTERM the process. See QuitSequence for the submit-byte caveat.
//
//   - End-of-turn screen marker: NOT yet identified. The adapter embeds
//     generic.Adapter so turn-complete signals still flow through the
//     wrapper.StatusWaitingForInput path (driven by the per-harness prompt
//     patterns in pkg/wrapper/internal/harness/pi/). Once a recording exists
//     under test/corpus/pi/, add an OnScreen-derived fingerprint here, mirroring
//     codex's Token-usage footer match or claude-code's "✻ <verb> for Ns" line
//     (and, with it, a BusyDetector + MessageExtractor).
//
//   - Session ID extraction (interactive path): NOT implemented. pi surfaces
//     its session id via the "/session" command and the JSON header line of
//     `pi --mode json` (parsed by the headless pkg/harness/pi profile), but no
//     UUID is scraped from the interactive TUI, so History() falls back to the
//     in-memory Store mid-session until an id is known. A future option is to
//     inject a generated id with pi's "--session-id <uuid>" flag at launch.
//
// Markers may shift across upstream versions; the golden-recording tests under
// test/corpus/pi/ will be the early-warning signal when they're added.
package pi

import (
	"regexp"
	"strings"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	transcriptpi "github.com/olesho/harness-wrapper/pkg/transcript/pi"
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
)

// Adapter implements turns.Adapter for the pi coding agent.
//
// It currently delegates OnScreen to the embedded generic.Adapter (no
// per-screen signals yet) and inherits OnWrapperStatus. Once
// corpus-driven markers land, override OnScreen here.
type Adapter struct {
	generic.Adapter
}

// New constructs a pi adapter.
func New() *Adapter { return &Adapter{} }

// Name returns "pi".
func (*Adapter) Name() string { return "pi" }

// ReadTranscript reads the on-disk pi session log. Implements
// turns.TranscriptReader. Becomes useful as soon as a harness session ID
// is known — see the package-level comment for the open question around
// how that ID is sourced.
func (*Adapter) ReadTranscript(harnessSessionID, workingDir string) ([]transcript.Turn, error) {
	return transcriptpi.New().Read(harnessSessionID, workingDir)
}

// quitCommand is pi's "/quit" slash command followed by Enter. pi auto-saves its
// session continuously, and "/quit" exits cleanly; sending it lets pi flush
// rather than being SIGTERM'd by Close.
//
// The trailing "\r" is pi's submit key (the actual Enter byte) — matching
// pkg/chat.submitKeyForHarness("pi"). Verified live against pi 0.76.0 that a
// bare "\n" does NOT submit pi's composer (the text just sits there unsent),
// while a carriage return does. If the command isn't accepted, Conversation.Quit's
// caller falls back to a signal, so a regression degrades to today's behavior.
var quitCommand = []byte("/quit\r")

// QuitSequence returns pi's graceful-exit keystrokes: the "/quit" slash command
// plus Enter, letting pi flush/persist its session rather than being SIGTERM'd.
// Implements turns.Quitter.
func (*Adapter) QuitSequence() []byte { return quitCommand }

// busyTexts are the status-line labels pi paints while a turn is in flight: the
// spinner reads "Thinking..." while the model reasons and "Working..." while it
// generates / runs a tool. Matching the trailing ellipsis (ASCII "..." or the
// "…" glyph) avoids a false hit on the "Thinking Level" menu label. Verified
// live against pi 0.76.0 (cerebras/gpt-oss-120b).
var busyTexts = []string{"Working...", "Working…", "Thinking...", "Thinking…"}

func busy(text string) bool {
	for _, m := range busyTexts {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// Busy reports whether pi is still working on the current turn, so the chat
// layer's idle-completion fallback won't declare a turn complete mid-flight (the
// composer is painted even while pi works). Implements turns.BusyDetector.
func (*Adapter) Busy(snap screen.Snapshot) bool { return busy(snap.Text) }

// statusLineRE matches pi's idle status-line context-usage indicator, e.g.
// "0.0%/131k" or "12.3%/200K". It is painted once pi's composer is initialized
// and accepting input, so its presence (while not busy) marks the ready state.
var statusLineRE = regexp.MustCompile(`\d+(?:\.\d+)?%/\d+[kK]`)

// PromptReady reports whether pi's composer is initialized and idle — ready to
// accept a submitted message: the status line is painted and no turn is in
// flight. The chat layer's readiness gate uses it so Send waits past pi's
// (noisy, network-touching) startup instead of writing into a composer that is
// not listening yet, and its idle-completion fallback uses it to avoid closing a
// turn while pi is still working.
func PromptReady(text string) bool {
	return !busy(text) && statusLineRE.MatchString(text)
}
