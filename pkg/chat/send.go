package chat

import (
	"context"
	"fmt"
	"time"

	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Send transmits a user message to the harness and records two turns:
// the user turn (immediately complete) and a placeholder assistant
// turn (TurnStatePending). The returned turnID identifies the
// assistant turn — observe Events() to learn when it transitions to
// Complete or Errored.
//
// Preconditions:
//   - The caller must currently hold the control token from
//     AcquireControl. Send returns ErrNoControl otherwise.
//   - No prior assistant turn may be in flight. Send returns
//     ErrTurnInFlight otherwise.
//
// The text is sent verbatim followed by the harness's submit key. Senders
// that need richer input (multi-line, control characters) should use
// Conversation.Wrapper().WriteStdin directly after acquiring control.
func (c *Conversation) Send(ctx context.Context, text string) (turnID string, err error) {
	select {
	case <-c.closed:
		return "", ErrClosed
	default:
	}

	if !c.queue.Held() {
		return "", ErrNoControl
	}

	c.mu.Lock()
	if c.currentTurn != nil {
		c.mu.Unlock()
		return "", ErrTurnInFlight
	}
	c.mu.Unlock()

	if err := c.waitReadyForSend(ctx); err != nil {
		return "", err
	}

	now := time.Now()

	userTurn := Turn{
		ID:          newID(),
		SessionID:   c.session.ID,
		Role:        RoleUser,
		State:       TurnStateComplete,
		Text:        text,
		StartedAt:   now,
		CompletedAt: now,
	}
	if err := c.store.AppendTurn(ctx, &userTurn); err != nil {
		return "", fmt.Errorf("chat: append user turn: %w", err)
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: userTurn})

	assistantTurn := Turn{
		ID:        newID(),
		SessionID: c.session.ID,
		Role:      RoleAssistant,
		State:     TurnStatePending,
		StartedAt: now,
	}
	if err := c.store.AppendTurn(ctx, &assistantTurn); err != nil {
		return "", fmt.Errorf("chat: append assistant turn: %w", err)
	}

	c.mu.Lock()
	turnCopy := assistantTurn
	c.currentTurn = &turnCopy
	c.endMarkerSeen = false // fresh turn: no end-of-turn marker seen yet
	c.mu.Unlock()

	submitKey := submitKeyForHarness(c.opts.Harness, c.screen.Snapshot().Text)
	if _, err := c.sess.WriteStdin(append([]byte(text), submitKey...)); err != nil {
		// Roll back the in-flight pointer and mark the turn errored.
		c.mu.Lock()
		c.currentTurn = nil
		c.mu.Unlock()
		assistantTurn.State = TurnStateErrored
		assistantTurn.Reason = "WriteStdin: " + err.Error()
		assistantTurn.CompletedAt = time.Now()
		if uerr := c.store.UpdateTurn(ctx, &assistantTurn); uerr != nil {
			return "", fmt.Errorf("chat: write stdin + update turn: write=%v update=%w", err, uerr)
		}
		c.emit(ConversationEvent{Type: EventTurn, Turn: assistantTurn, Err: err})
		return assistantTurn.ID, fmt.Errorf("chat: write stdin: %w", err)
	}

	c.emit(ConversationEvent{Type: EventTurn, Turn: assistantTurn})
	return assistantTurn.ID, nil
}

// Wrapper returns the underlying wrapper.Session for callers that need
// to reach past the chat API — e.g. to Resize, AttachOutput, or read
// the raw RecentOutput buffer. Use with care: writing directly to
// stdin bypasses the control-token guard.
func (c *Conversation) Wrapper() *wrapper.Session { return c.sess }

// Quit asks the harness to exit gracefully by sending its adapter-defined quit
// sequence (Claude Code: the "/quit" slash command) through the stdin writer the
// Conversation already holds, so the harness can flush and persist its
// transcript before terminating — rather than being SIGTERM'd by Close. It takes
// the control token for the duration of the write so it serializes with Send.
//
// Quit does NOT wait for the process to exit or close the Conversation; call
// Wrapper().Wait and/or Close afterwards. The harness's own session id, which it
// prints as it exits, is captured by the durable line tap (see Open), so a
// History read after the process exits returns the transcript-backed history.
//
// Returns ErrQuitUnsupported when the adapter exposes no quit sequence (does not
// implement turns.Quitter), ErrClosed after Close, or ctx.Err() if the control
// token could not be acquired before ctx is done.
func (c *Conversation) Quit(ctx context.Context) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}

	q, ok := c.adapter.(turns.Quitter)
	if !ok {
		return ErrQuitUnsupported
	}
	keys := q.QuitSequence()
	if len(keys) == 0 {
		return ErrQuitUnsupported
	}

	release, err := c.queue.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()

	return c.write(keys)
}
