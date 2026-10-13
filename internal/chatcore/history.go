package chatcore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/olesho/harness-wrapper/pkg/turns"
)

// History returns the conversation history for this Conversation.
//
// When the adapter supports turns.TranscriptReader and the harness
// session ID is known, History reads the harness's own JSONL log and
// returns its parsed contents — this is the higher-fidelity source
// because the harness records exactly what the model said, not what
// the TUI rendered.
//
// When transcript reading isn't possible (adapter has no reader, the
// harness session ID is not known, or the harness has not written its
// transcript yet), History falls back to the Store's recorded turns. The
// fallback only contains the user-side text and any screen-derived assistant
// text the watcher captured at TurnComplete.
func (c *Conversation) History(ctx context.Context) ([]Turn, error) {
	out, _, err := c.HistoryWithSource(ctx)
	return out, err
}

// HistorySource identifies where a History result came from.
type HistorySource string

const (
	// HistorySourceTranscript means the turns were read from the harness's own
	// persisted session log (turns.TranscriptReader) — authoritative and
	// complete, with no TUI chrome.
	HistorySourceTranscript HistorySource = "transcript"
	// HistorySourceStore means the turns came from the chat store fallback:
	// user-side text plus whatever screen-derived assistant text the watcher
	// captured. Used when the adapter can't read transcripts, the harness
	// session id is not known, or its transcript does not exist yet.
	HistorySourceStore HistorySource = "store"
)

// HistoryWithSource is History plus the provenance of the returned turns. The
// distinction matters to callers that need to know whether they got the
// authoritative transcript or the lossy screen-derived store fallback — the
// presence of turns alone does not tell them apart, since both paths return
// non-empty slices.
func (c *Conversation) HistoryWithSource(ctx context.Context) ([]Turn, HistorySource, error) {
	c.mu.Lock()
	sessionCopy := c.session
	c.mu.Unlock()

	reader, hasReader := c.adapter.(turns.TranscriptReader)
	if !hasReader || sessionCopy.HarnessID() == "" {
		out, err := c.store.ListTurns(ctx, sessionCopy.ID)
		return out, HistorySourceStore, err
	}

	tturns, err := reader.ReadTranscript(sessionCopy.HarnessID(), c.transcriptDir())
	if errors.Is(err, fs.ErrNotExist) {
		// The id is known but the harness has not written its transcript yet —
		// an id assigned at launch is known before the first flush. That is the
		// store's history, not a failure.
		out, err := c.store.ListTurns(ctx, sessionCopy.ID)
		return out, HistorySourceStore, err
	}
	if err != nil {
		return nil, HistorySourceTranscript, fmt.Errorf("chat: read transcript: %w", err)
	}
	out := make([]Turn, 0, len(tturns))
	for _, tt := range tturns {
		out = append(out, Turn{
			SessionID:   sessionCopy.ID,
			Role:        Role(tt.Role),
			State:       TurnStateComplete,
			Text:        tt.Text,
			StartedAt:   tt.Timestamp,
			CompletedAt: tt.Timestamp,
		})
	}
	return out, HistorySourceTranscript, nil
}

// transcriptDir is the working directory the harness's transcript is filed
// under: harnessDir, or Options.WorkingDir for a Conversation built without
// Open.
func (c *Conversation) transcriptDir() string {
	if c.harnessDir != "" {
		return c.harnessDir
	}
	return c.opts.WorkingDir
}
