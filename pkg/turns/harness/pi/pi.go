// Package pi provides a turn-detection adapter for the pi coding agent
// (@earendil-works/pi-coding-agent, binary "pi" —
// github.com/earendil-works/pi).
//
// Current state — v0.1, ahead of corpus recording:
//
//   - End-of-turn screen marker: NOT yet identified. The adapter embeds
//     generic.Adapter so turn-complete signals still flow through the
//     wrapper.StatusWaitingForInput path (driven by the per-harness
//     prompt patterns in pkg/wrapper/internal/harness/pi/). Once a
//     recording exists under test/corpus/pi/, replace the OnScreen-derived
//     fingerprint here, mirroring codex's Token-usage footer match or
//     claude-code's "✻ <verb> for Ns" line.
//
//   - Session ID extraction: NOT yet identified. pi names its session
//     file <timestamp>_<uuid>.jsonl and supports --continue/--resume, but
//     we have not confirmed a scrape-able on-screen UUID. ExtractSessionID
//     is therefore not implemented and History() falls back to the
//     in-memory Store mid-session until a harness session ID is set
//     (matching the conservative posture used for gemini).
//
//   - Transcript reading: implemented. pi's on-disk session format is
//     documented and versioned (JSONL, format v3), so ReadTranscript is
//     wired to pkg/transcript/pi and fires as soon as a harness session
//     ID is known.
//
// Markers may shift across upstream versions; the golden-recording tests
// under test/corpus/pi/ will be the early-warning signal when they're
// added.
package pi

import (
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
