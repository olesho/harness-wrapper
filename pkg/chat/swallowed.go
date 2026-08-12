package chat

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// A prompt the harness never accepted looks exactly like a turn that finished:
// the screen is settled, the prompt is ready, and there is no assistant output.
// Completing it "successfully" hands the caller a turn whose Text is the raw
// ready screen — which is how eight consecutive paid agent runs in loom's
// daemon reported success while producing nothing at all.
//
// Adapters that can tell the two apart implement turns.SwallowedPromptDetector.
// Their verdict is screen-only, so it can false-fire when the TUI repaint lags
// the idle gap; the harness's own on-disk transcript is given the power to
// overturn it. This mirrors meta-harness's conversation.ts, whose comments call
// the same failure a "swallowed" prompt.
const (
	// transcriptFlushRetryGap is the single pause before re-reading the
	// transcript when the first read found no rollout at all — the shape a
	// harness that has not flushed yet produces.
	transcriptFlushRetryGap = 400 * time.Millisecond
)

// swallowedPromptVerdict is what the idle-completion path does with a screen
// the adapter says was never accepted.
type swallowedPromptVerdict struct {
	// proofText is the assistant reply recovered from the transcript. Non-empty
	// means the screen verdict was wrong and the turn really did complete.
	proofText string
	// diag explains why no proof was available, for the error Reason.
	diag string
}

// promptWasSwallowed asks the adapter whether this settled screen shows a
// prompt the harness never accepted. False whenever the adapter cannot tell —
// an adapter that has no opinion must not be read as accusing.
func (c *Conversation) promptWasSwallowed(snap screen.Snapshot) bool {
	d, ok := c.adapter.(turns.SwallowedPromptDetector)
	if !ok {
		return false
	}
	c.mu.Lock()
	sent := c.sentScreenText
	c.mu.Unlock()
	return d.PromptNotAccepted(snap, sent)
}

// transcriptProofOfCurrentTurn looks for assistant output in the harness's own
// rollout, which overturns the screen-only swallow verdict.
//
// Deliberately conservative about what counts as proof: any assistant turn with
// non-empty text in the transcript for this session. The alternative — matching
// the exact prompt — would need the pre-send watermark meta-harness keeps, and
// getting that wrong turns a rescue into a false success, which is the one
// direction this must not fail in.
func (c *Conversation) transcriptProofOfCurrentTurn() swallowedPromptVerdict {
	reader, hasReader := c.adapter.(turns.TranscriptReader)
	if !hasReader {
		return swallowedPromptVerdict{diag: "adapter cannot read the harness transcript"}
	}
	c.mu.Lock()
	sessionID := c.session.HarnessSessionID
	c.mu.Unlock()
	if sessionID == "" {
		return swallowedPromptVerdict{diag: "harness session id not known yet"}
	}

	if v, retryable := c.tryTranscriptProof(reader, sessionID); !retryable {
		return v
	}
	// The rollout may simply not be on disk yet. One pause, one retry: the
	// flush-lag shape is the only miss worth waiting for.
	select {
	case <-c.closed:
		return swallowedPromptVerdict{diag: "conversation closed before the transcript settled"}
	case <-time.After(transcriptFlushRetryGap):
	}
	v, _ := c.tryTranscriptProof(reader, sessionID)
	return v
}

// tryTranscriptProof is one read. retryable marks the flush-lag-shaped miss
// (no rollout yet) rather than a real failure.
func (c *Conversation) tryTranscriptProof(reader turns.TranscriptReader, sessionID string) (v swallowedPromptVerdict, retryable bool) {
	tturns, err := reader.ReadTranscript(sessionID, c.opts.WorkingDir)
	if err != nil {
		return swallowedPromptVerdict{diag: "transcript check failed: " + err.Error()}, true
	}
	for i := len(tturns) - 1; i >= 0; i-- {
		if Role(tturns[i].Role) != RoleAssistant {
			continue
		}
		if text := strings.TrimSpace(tturns[i].Text); text != "" {
			return swallowedPromptVerdict{proofText: text}, false
		}
	}
	return swallowedPromptVerdict{diag: fmt.Sprintf("transcript has no assistant output (%d turn(s))", len(tturns))}, false
}

// applySwallowedPromptVerdict finishes a turn the adapter reported as never
// accepted. It returns true when it has taken ownership of the turn, in which
// case the caller must not complete it itself.
//
// Order matters: the transcript gets to speak first, because a rescue is only
// possible before the turn is declared failed.
func (c *Conversation) applySwallowedPromptVerdict(turn *Turn, snap screen.Snapshot) bool {
	v := c.transcriptProofOfCurrentTurn()
	turn.CompletedAt = time.Now()

	if v.proofText != "" {
		turn.State = TurnStateComplete
		turn.Reason = c.opts.Harness + ": transcript-confirmed completion (screen looked swallowed; rollout shows assistant output)"
		// The clean transcript reply, NOT assistantText — which for an adapter
		// without an extractor would persist the raw ready screen.
		turn.Text = v.proofText
	} else {
		turn.State = TurnStateErrored
		// No assistant output was recoverable. A settled screen showing a
		// logged-out / re-auth banner means the turn did not fail on its
		// merits — record the canonical auth reason instead of the generic one.
		if authRequired(c.opts.Harness, snap.Text) {
			turn.Reason = ReasonAuthRequired
		} else {
			turn.Reason = c.opts.Harness + ": prompt not accepted / no assistant output"
			if v.diag != "" {
				turn.Reason += "; " + v.diag
			}
		}
	}

	if err := c.store.UpdateTurn(context.Background(), turn); err != nil {
		c.emit(ConversationEvent{Type: EventTurn, Turn: *turn, Err: err})
		return true
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: *turn})
	return true
}
