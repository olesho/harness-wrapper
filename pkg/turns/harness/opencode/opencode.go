// Package opencode provides a turn-detection adapter for the OpenCode
// CLI (opencode-ai, binary "opencode" — github.com/sst/opencode).
//
// Current state — v0.1, ahead of corpus recording:
//
//   - End-of-turn screen marker: NOT yet identified. The adapter embeds
//     generic.Adapter so turn-complete signals still flow through the
//     wrapper.StatusWaitingForInput path (driven by the per-harness
//     prompt patterns in pkg/wrapper/internal/harness/opencode/). Once a
//     recording exists under test/corpus/opencode/, replace the
//     OnScreen-derived fingerprint here, mirroring codex's Token-usage
//     footer match or claude-code's "✻ <verb> for Ns" line.
//
//   - Session ID extraction: NOT yet identified. OpenCode does not print
//     its internal session UUID in the TUI in a position we've confirmed
//     scrape-able, so ExtractSessionID is not implemented and History()
//     falls back to the in-memory Store for opencode conversations.
//
//   - Transcript reading: NOT implemented. OpenCode's on-disk store is
//     in flux — early versions wrote per-message/per-part JSON files
//     under $OPENCODE_DATA_DIR (default ~/.local/share/opencode/storage/:
//     session/<projectID>/<sessionID>.json, message/<sessionID>/<id>.json,
//     part/<messageID>/<id>.json), and later versions migrated to SQLite.
//     Rather than ship a reader that silently breaks across that
//     migration, the adapter omits turns.TranscriptReader; the chat layer
//     already degrades gracefully to the Store. Add a pkg/transcript/
//     opencode reader (and implement ReadTranscript here) once a recorded
//     corpus pins the format for the version we target in versions.json.
//
// Markers may shift across upstream versions; the golden-recording tests
// under test/corpus/opencode/ will be the early-warning signal when
// they're added.
package opencode

import (
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
)

// Adapter implements turns.Adapter for the OpenCode CLI.
//
// It currently delegates OnScreen to the embedded generic.Adapter (no
// per-screen signals yet) and inherits OnWrapperStatus. Once
// corpus-driven markers land, override OnScreen here.
type Adapter struct {
	generic.Adapter
}

// New constructs an OpenCode adapter.
func New() *Adapter { return &Adapter{} }

// Name returns "opencode".
func (*Adapter) Name() string { return "opencode" }
