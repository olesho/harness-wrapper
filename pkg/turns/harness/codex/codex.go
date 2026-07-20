// Package codex provides a turn-detection adapter for the Codex CLI
// (codex-cli, github.com/openai/codex).
//
// Detection signal (legacy, codex ≤0.141): every turn ended with a "Token
// usage:" footer containing per-turn counts. The full footer acts as a per-turn
// fingerprint — a new fingerprint on screen emits TurnComplete (see
// tokenUsageRE / OnScreen).
//
// Codex 0.142+ REMOVED that on-screen footer (and the "codex resume <uuid>"
// hint). There is no longer ANY on-screen end-of-turn marker, so OnScreen stays
// silent and this adapter no longer drives completion for current Codex. Turn
// completion is instead detected by the chat layer's idle-completion (the screen
// settles at the "›" composer; see pkg/chat) and reply CONTENT comes from the
// on-disk rollout transcript (see pkg/transcript/codex), keyed on the session id
// the rollout's session_meta records. The legacy OnScreen path is retained for
// any codex still emitting the footer; the test/corpus/codex recordings (baked
// at 0.142.2) lock in that OnScreen correctly does NOT fire on current Codex.
//
// This adapter embeds generic.Adapter so wrapper.Status transitions
// (blocked_by_cost, retry_later, failed) continue to flow through to
// the event stream alongside the screen-derived signals.
//
// Verified against Codex 0.142.2 (footer/resume-hint removal confirmed; corpus
// re-baked). Interstitial detection (input.go) verified against 0.140.0, and the
// 0.141.0 idle composer + CSI-13u submit (see input_test.go). Markers may shift
// across upstream versions; the golden-recording tests under test/corpus/codex/
// are the early-warning signal for that drift.
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

// tokenUsageRE matches the per-turn Token usage footer Codex printed when a turn
// ended on codex ≤0.141. Format observed on 0.130.0:
//
//	Token usage: total=5,288 input=5,158 (+ 20,864 cached) output=130 (reasoning 42)
//
// The "(reasoning N)" suffix is optional. Numbers can include commas.
//
// codex 0.142+ no longer renders this footer at all (see the package comment), so
// on current Codex this never matches and OnScreen stays silent — completion is
// the chat layer's idle path. The regex is kept for any codex still emitting it
// and is exercised synthetically by TestCodexAdapterRefiresWhenFingerprintChanges;
// keep it strict (anchored full footer) so it cannot false-fire on reply prose.
var tokenUsageRE = regexp.MustCompile(`Token usage: total=[\d,]+ input=[\d,]+ \(\+ [\d,]+ cached\) output=[\d,]+(?: \(reasoning \d+\))?`)

// resumeRE matches the "codex resume <uuid>" hint Codex prints at end
// of session and after each turn. The UUID is the session ID the
// transcript file is named after.
var resumeRE = regexp.MustCompile(`codex resume ([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// Adapter implements turns.Adapter for Codex CLI.
type Adapter struct {
	generic.Adapter // inherits OnWrapperStatus + (no-op) OnScreen-but-we-override

	// SessionsRoot overrides the default ~/.codex/sessions location used by
	// the on-disk session-id fallback (LocateSessionID). Empty means default;
	// set only in tests.
	SessionsRoot string

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
			// A different interstitial replaced the one we were tracking without
			// an intervening dialog-free frame (e.g. the update notice giving way
			// to a model-migration or notice screen). Resolve the previous one
			// first so every InputRequested is balanced by an InputResolved and
			// the chat layer's currentInput is not silently overwritten — which
			// would drop the prior request's identity/kind (a client subscribed
			// from the start would otherwise see the replacement's kind on the
			// eventual resolve).
			if a.lastInputID != "" {
				prev := a.lastInput
				if prev == nil {
					prev = &turns.InputRequest{ID: a.lastInputID}
				}
				out = append(out, turns.Event{Kind: turns.InputResolved, Reason: "codex: input resolved", Input: prev})
			}
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

// LocateSessionID recovers the Codex session UUID from the most recent
// on-disk rollout whose session_meta cwd matches workingDir. This is the
// version-independent fallback for the screen-scrape ExtractSessionID, which
// returns nothing on Codex 0.142+ (the resume hint is no longer rendered).
// Implements turns.SessionIDLocator.
func (a *Adapter) LocateSessionID(workingDir string) (string, bool) {
	return (&transcriptcodex.Reader{SessionsRoot: a.SessionsRoot}).LocateLatestSession(workingDir)
}

// ReadTranscript reads the on-disk Codex session log. Implements
// turns.TranscriptReader.
func (a *Adapter) ReadTranscript(harnessSessionID, workingDir string) ([]transcript.Turn, error) {
	evs, err := (&transcriptcodex.Reader{SessionsRoot: a.SessionsRoot}).Read(harnessSessionID, workingDir)
	if err != nil {
		return nil, err
	}
	return transcript.TurnsFromEvents(evs), nil
}
