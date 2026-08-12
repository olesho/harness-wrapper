package chat

import (
	"context"
	"errors"
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
		// The harness is stuck on a logged-out / onboarding screen and will never
		// reach a ready prompt. Record a terminal assistant turn carrying the
		// canonical ReasonAuthRequired instead of hanging to the deadline, so the
		// onboarding case surfaces through the same Events()/turn.Reason channel as
		// the completion- and error-path cases. Returns (id, nil) so the RunTurn
		// driver observes the emitted terminal turn rather than a bare error.
		if errors.Is(err, ErrAuthRequired) {
			return c.emitAuthRequiredTurn(ctx, text)
		}
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

	// Record the screen the prompt is being submitted on: a swallow detector
	// answers "nothing changed at all" by comparing the settled screen to this.
	sentScreen := c.screen.Snapshot().Text
	c.mu.Lock()
	c.sentScreenText = sentScreen
	c.mu.Unlock()

	submitKey := submitKeyForHarness(c.opts.Harness, sentScreen)
	// The prompt and the submit key go out as SEPARATE writes, with the composer
	// echo awaited in between — see submit.go for the paste-collapse failure a
	// single combined write loses to.
	if err := c.writeMessageAndSubmit(ctx, text, sentScreen, submitKey); err != nil {
		// Roll back the in-flight pointer and mark the turn errored.
		c.mu.Lock()
		c.currentTurn = nil
		c.mu.Unlock()
		assistantTurn.State = TurnStateErrored
		assistantTurn.Reason = "submit: " + err.Error()
		assistantTurn.CompletedAt = time.Now()
		if uerr := c.store.UpdateTurn(ctx, &assistantTurn); uerr != nil {
			return "", fmt.Errorf("chat: submit prompt + update turn: submit=%v update=%w", err, uerr)
		}
		c.emit(ConversationEvent{Type: EventTurn, Turn: assistantTurn, Err: err})
		return assistantTurn.ID, fmt.Errorf("chat: submit prompt: %w", err)
	}

	c.emit(ConversationEvent{Type: EventTurn, Turn: assistantTurn})
	return assistantTurn.ID, nil
}

// emitAuthRequiredTurn records and emits a terminal assistant turn carrying
// ReasonAuthRequired, for the case where the harness never reaches a ready prompt
// because it is sitting in a logged-out / onboarding screen (detected by
// waitReadyForSend). It mirrors the normal Send bookkeeping — a completed user
// turn, then a terminal assistant turn — so consumers observe the auth signal
// through the same Events()/turn.Reason channel as the completion- and error-path
// cases. The prompt is NOT written to the harness (it would land in the sign-in
// menu). Returns (assistantTurnID, nil); the RunTurn driver reads the emitted
// Errored turn and surfaces its Reason.
func (c *Conversation) emitAuthRequiredTurn(ctx context.Context, text string) (string, error) {
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
		ID:          newID(),
		SessionID:   c.session.ID,
		Role:        RoleAssistant,
		State:       TurnStateErrored,
		Reason:      ReasonAuthRequired,
		StartedAt:   now,
		CompletedAt: now,
	}
	if err := c.store.AppendTurn(ctx, &assistantTurn); err != nil {
		return "", fmt.Errorf("chat: append assistant turn: %w", err)
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: assistantTurn})
	return assistantTurn.ID, nil
}

// Wrapper returns the underlying wrapper.Session for callers that need
// to reach past the chat API — e.g. to AttachOutput or read the raw
// RecentOutput buffer. Use Conversation.Resize instead of resizing the
// wrapper directly so the private terminal emulator stays synchronized.
// Use with care: writing directly to stdin bypasses the control-token guard.
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
