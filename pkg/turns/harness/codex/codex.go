// Package codex provides a turn-detection adapter for the Codex CLI
// (codex-cli, github.com/openai/codex).
//
// Detection signal: every turn ends with a "Token usage:" footer
// containing per-turn input/output counts. Because the counts change
// between turns, the full footer line acts as a per-turn fingerprint —
// when a new fingerprint appears on screen the adapter emits
// TurnComplete.
//
// This adapter embeds generic.Adapter so wrapper.Status transitions
// (blocked_by_cost, retry_later, failed) continue to flow through to
// the event stream alongside the screen-derived signals.
//
// Verified against Codex 0.130.0; interstitial detection (input.go)
// verified against 0.140.0. Markers may shift across upstream versions;
// the golden-recording tests under test/corpus/codex/ are the
// early-warning signal for that drift.
package codex

import (
	"regexp"
	"sync"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	transcriptcodex "github.com/olesho/harness-wrapper/pkg/transcript/codex"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
)

// tokenUsageRE matches the per-turn Token usage footer Codex prints
// when a turn ends. Format observed on 0.130.0:
//
//	Token usage: total=5,288 input=5,158 (+ 20,864 cached) output=130 (reasoning 42)
//
// The "(reasoning N)" suffix is optional. Numbers can include commas.
var tokenUsageRE = regexp.MustCompile(`Token usage: total=[\d,]+ input=[\d,]+ \(\+ [\d,]+ cached\) output=[\d,]+(?: \(reasoning \d+\))?`)

// resumeRE matches the "codex resume <uuid>" hint Codex prints at end
// of session and after each turn. The UUID is the session ID the
// transcript file is named after.
var resumeRE = regexp.MustCompile(`codex resume ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// Adapter implements turns.Adapter for Codex CLI.
type Adapter struct {
	generic.Adapter // inherits OnWrapperStatus + (no-op) OnScreen-but-we-override

	mu              sync.Mutex
	lastFingerprint string
	lastInputID     string
	lastInput       *turns.InputRequest
}

// New constructs a Codex adapter.
func New() *Adapter { return &Adapter{} }

// Name returns "codex".
func (*Adapter) Name() string { return "codex" }

// OnScreen scans the current screen for a Token usage footer. If a new
// footer (one whose full text differs from the last one seen) is
// present, emits TurnComplete.
//
// State: the adapter remembers the most recently fired fingerprint and
// suppresses repeat fires while the same footer remains on screen.
func (a *Adapter) OnScreen(snap screen.Snapshot) []turns.Event {
	a.mu.Lock()
	defer a.mu.Unlock()

	var out []turns.Event

	// Turn-complete detection — newest Token usage footer differs from last.
	if matches := tokenUsageRE.FindAllString(snap.Text, -1); len(matches) > 0 {
		latest := matches[len(matches)-1]
		if latest != a.lastFingerprint {
			a.lastFingerprint = latest
			out = append(out, turns.Event{Kind: turns.TurnComplete, Reason: "codex: " + latest})
		}
	}

	// Blocking startup interstitial (update notice, model migration, …) —
	// transition on the request ID. A new interstitial emits InputRequested;
	// it clearing emits InputResolved. The chat layer auto-dismisses these by
	// default; codex's real approval prompts are not detected here and so are
	// never auto-confirmed.
	if req, ok := DetectInput(snap.Text); ok {
		if req.ID != a.lastInputID {
			a.lastInputID = req.ID
			a.lastInput = req
			out = append(out, turns.Event{Kind: turns.InputRequested, Reason: "codex: " + req.Prompt, Input: req})
		}
	} else if a.lastInputID != "" {
		resolved := a.lastInput
		if resolved == nil {
			resolved = &turns.InputRequest{ID: a.lastInputID}
		}
		a.lastInputID = ""
		a.lastInput = nil
		out = append(out, turns.Event{Kind: turns.InputResolved, Reason: "codex: input resolved", Input: resolved})
	}

	return out
}

// ExtractSessionID scrapes the "codex resume <uuid>" line Codex prints
// to identify the on-disk transcript. Implements turns.SessionIDExtractor.
func (*Adapter) ExtractSessionID(snap screen.Snapshot) (string, bool) {
	m := resumeRE.FindStringSubmatch(snap.Text)
	if len(m) < 2 {
		return "", false
	}
	return m[1], true
}

// ReadTranscript reads the on-disk Codex session log. Implements
// turns.TranscriptReader.
func (*Adapter) ReadTranscript(harnessSessionID, workingDir string) ([]transcript.Turn, error) {
	evs, err := transcriptcodex.New().Read(harnessSessionID, workingDir)
	if err != nil {
		return nil, err
	}
	return transcript.TurnsFromEvents(evs), nil
}
