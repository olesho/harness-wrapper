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
	switch {
	case c.currentTurn != nil:
		c.mu.Unlock()
		return "", ErrTurnInFlight
	case c.exit != nil:
		c.mu.Unlock()
		return "", ErrExited
	}
	c.mu.Unlock()

	if c.stream != nil {
		return c.streamSend(ctx, text)
	}

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

	// Type into an empty composer only: whatever it holds — a prompt a cancel
	// put back, a draft typed at the terminal — would become part of this one.
	// Checked before anything is recorded, under the lock an Interrupt's keys
	// take too (ADR-007).
	if ir, ok := c.adapter.(turns.Interrupter); ok {
		if err := c.submit.lock(ctx, c.closed); err != nil {
			return "", err
		}
		err := c.clearComposer(ctx, ir, "")
		c.submit.unlock()
		if err != nil {
			return "", err
		}
	}

	assistantTurn, err := c.beginTurn(ctx, text)
	if err != nil {
		return "", err
	}
	// From here, the screen showing the harness at work means it took this
	// prompt: it sat idle through the busy gate before anything was typed.
	c.acceptAfter.Store(time.Now().UnixNano())

	// The prompt and its submit key must not interleave with an Interrupt's
	// keys (ADR-007).
	if err := c.submit.lock(ctx, c.closed); err != nil {
		return c.failSubmit(assistantTurn.ID, err)
	}
	// Record the screen the prompt is being submitted on: a swallow detector
	// answers "nothing changed at all" by comparing the settled screen to this.
	sentScreen := c.screen.Snapshot().Text
	// ...and how far the harness's own transcript already extends, so that
	// nothing written BEFORE this prompt can later be read as a verdict about
	// it. Taken before the submit for the obvious reason: afterwards the
	// harness is already writing.
	watermark := c.captureTranscriptWatermark()
	c.mu.Lock()
	c.sentScreenText = sentScreen
	c.sentTranscriptWatermark = watermark
	c.mu.Unlock()

	submitKey := submitKeyForHarness(c.opts.Harness, sentScreen)
	// The prompt and the submit key go out as SEPARATE writes, with the composer
	// echo awaited in between — see submit.go for the paste-collapse failure a
	// single combined write loses to.
	err = c.writeMessageAndSubmit(ctx, text, sentScreen, submitKey)
	c.submit.unlock()
	if err != nil {
		return c.failSubmit(assistantTurn.ID, err)
	}
	// The turn may already have ended — a fast reply, the harness exiting —
	// and its terminal event been delivered before Send returns.
	return assistantTurn.ID, nil
}

// beginTurn records the turn a Send is about to submit and announces it — the
// user's turn, then the assistant's, pending — and makes the assistant's the
// turn in flight. It runs before the prompt reaches the harness: nothing can
// end a turn that has not been submitted, so no terminal event can precede
// its pending one (ADR-008). The announcements wait for room in the event
// queue, so they go out holding no lock; EventExited waits for them.
func (c *Conversation) beginTurn(ctx context.Context, text string) (Turn, error) {
	c.mu.Lock()
	switch {
	case c.currentTurn != nil:
		c.mu.Unlock()
		return Turn{}, ErrTurnInFlight
	case c.exit != nil:
		c.mu.Unlock()
		return Turn{}, ErrExited
	}
	c.sending.Add(1)
	c.mu.Unlock()
	defer c.sending.Done()

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
		return Turn{}, fmt.Errorf("chat: append user turn: %w", err)
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
		return Turn{}, fmt.Errorf("chat: append assistant turn: %w", err)
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: assistantTurn})

	c.mu.Lock()
	turnCopy := assistantTurn
	c.currentTurn = &turnCopy
	c.currentPrompt = text
	c.endMarkerSeen = false // fresh turn: no end-of-turn marker seen yet
	c.heldReason = ""       // nor a Blocked to hold it on
	c.mu.Unlock()
	return assistantTurn, nil
}

// failSubmit ends a turn whose prompt could not be submitted, once: if
// something else — the harness's exit — ended it first, that stands.
func (c *Conversation) failSubmit(turnID string, err error) (string, error) {
	c.mu.Lock()
	var turn *Turn
	if c.currentTurn != nil && c.currentTurn.ID == turnID {
		turn = c.claimTurnLocked()
	}
	c.mu.Unlock()
	if turn != nil {
		turn.State = TurnStateErrored
		turn.Reason = "submit: " + err.Error()
		turn.CompletedAt = time.Now()
		c.finishTurn(turn, err)
	}
	return turnID, fmt.Errorf("chat: submit prompt: %w", err)
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

	// The prompt is deliberately NOT written to the harness here, so nothing it
	// writes afterwards belongs to this turn. Clear the watermark rather than
	// leaving a previous turn's, which would let a stale tag speak for a turn
	// that never reached the harness at all.
	c.mu.Lock()
	c.sentTranscriptWatermark = watermarkUnknown
	c.mu.Unlock()

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
		Code:        CodeAuthRequired,
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

	if c.stream != nil {
		return c.streamQuit(ctx)
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
