package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// InterruptResult is what Interrupt did.
type InterruptResult string

const (
	// InterruptStopped: the harness stopped the turn after it had produced
	// output. The turn ended TurnStateInterrupted with its partial reply.
	InterruptStopped InterruptResult = "stopped"

	// InterruptCancelled: the harness cancelled the turn before its first
	// token. The turn ended TurnStateInterrupted with no text, and the prompt
	// the harness put back in its composer was cleared.
	InterruptCancelled InterruptResult = "cancelled"

	// InterruptNoTurn: no turn was in flight. Nothing was written.
	InterruptNoTurn InterruptResult = "no_turn"

	// InterruptTooLate: the turn ended on its own before the harness took the
	// interrupt, and keeps that outcome.
	InterruptTooLate InterruptResult = "too_late"
)

// interruptPoll bounds how long Interrupt waits between looks at the turn: a
// turn can end without a repaint (the idle fallback runs on a timer).
const interruptPoll = 100 * time.Millisecond

// Composer clearing: how many rounds of clear keys before giving up, and how
// long each round waits for the composer to repaint and settle.
const (
	composerClearRounds = 4
	composerRepaintWait = time.Second
	composerSettle      = 100 * time.Millisecond
)

// interruptOp is one Interrupt under way. Its fields other than done are
// guarded by the Conversation's mu.
type interruptOp struct {
	turnID  string
	written bool            // the interrupt keys went out
	outcome InterruptResult // how the turn ended, when an interrupt ended it
	done    chan struct{}   // closed when the op finishes
	result  InterruptResult // valid once done is closed
	err     error           // valid once done is closed
}

// wait returns the op's result once it finishes, or ErrInterruptUnconfirmed
// when ctx ends first.
func (op *interruptOp) wait(ctx context.Context) (InterruptResult, error) {
	select {
	case <-op.done:
		return op.result, op.err
	case <-ctx.Done():
		return "", fmt.Errorf("%w: %w", ErrInterruptUnconfirmed, ctx.Err())
	}
}

// submitLock is a mutex whose Lock gives up when ctx ends. The zero value is
// unlocked.
type submitLock struct {
	once sync.Once
	ch   chan struct{}
}

func (l *submitLock) lock(ctx context.Context, closed <-chan struct{}) error {
	l.once.Do(func() { l.ch = make(chan struct{}, 1) })
	select {
	case l.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-closed:
		return ErrClosed
	}
}

func (l *submitLock) unlock() { <-l.ch }

// Interrupt stops the turn in flight (ADR-007). It needs no control token —
// RunTurn holds that for a whole turn — and never lands inside a submit: it
// waits for a Send's keystrokes to finish, then for the harness to show it has
// taken the prompt, and writes the adapter's interrupt keys once. The turn ends
// only on the harness's acknowledgement, read from the screen:
//
//   - stopped mid-reply or mid-tool: the turn ends TurnStateInterrupted with
//     its partial reply (the transcript's, when it has recorded the interrupt)
//     and Interrupt returns InterruptStopped;
//   - cancelled before its first token: the turn ends TurnStateInterrupted
//     with no text, the prompt the harness put back in its composer is
//     cleared, and Interrupt returns InterruptCancelled — with
//     ErrComposerNotCleared if the composer would not clear;
//   - finished first: the turn keeps its own outcome and Interrupt returns
//     InterruptTooLate;
//   - ctx ends first: ErrInterruptUnconfirmed, and the turn stays in flight.
//
// With no turn in flight it returns InterruptNoTurn and writes nothing. A
// second Interrupt for the same turn joins the first rather than writing again:
// two Escs close together on an idle claude open its Rewind picker. The turn's
// terminal state reaches Events() like any other. Returns
// ErrInterruptUnsupported for an adapter without turns.Interrupter.
func (c *Conversation) Interrupt(ctx context.Context) (InterruptResult, error) {
	select {
	case <-c.closed:
		return "", ErrClosed
	default:
	}
	ir, ok := c.adapter.(turns.Interrupter)
	if !ok {
		return "", ErrInterruptUnsupported
	}
	// With a turn in flight the op is registered before the submit lock is
	// taken, so a concurrent Interrupt joins it rather than queueing behind it.
	// With none, a Send may be recording one under the lock: wait for it.
	op, joined := c.claimInterrupt()
	if joined {
		return op.wait(ctx)
	}
	if err := c.submit.lock(ctx, c.closed); err != nil {
		if !errors.Is(err, ErrClosed) {
			err = fmt.Errorf("%w: %w", ErrInterruptUnconfirmed, err)
		}
		if op != nil {
			c.finishInterruptOp(op, "", err)
		}
		return "", err
	}
	if op == nil {
		if op, joined = c.claimInterrupt(); joined {
			c.submit.unlock()
			return op.wait(ctx)
		}
		if op == nil {
			c.submit.unlock()
			return InterruptNoTurn, nil
		}
	}
	c.mu.Lock()
	prompt := c.currentPrompt
	c.mu.Unlock()
	result, err := c.runInterrupt(ctx, ir, op, prompt)
	c.submit.unlock()
	c.finishInterruptOp(op, result, err)
	return result, err
}

// claimInterrupt returns the Interrupt under way for the in-flight turn
// (joined), or registers a new one for it; nil when no turn is in flight.
func (c *Conversation) claimInterrupt() (op *interruptOp, joined bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	turn := c.currentTurn
	if turn == nil {
		return nil, false
	}
	if op := c.interrupt; op != nil && op.turnID == turn.ID {
		return op, true
	}
	op = &interruptOp{turnID: turn.ID, done: make(chan struct{})}
	c.interrupt = op
	return op, false
}

// finishInterruptOp records op's result for the callers that joined it.
func (c *Conversation) finishInterruptOp(op *interruptOp, result InterruptResult, err error) {
	c.mu.Lock()
	if c.interrupt == op {
		c.interrupt = nil
	}
	c.mu.Unlock()
	op.result, op.err = result, err
	close(op.done)
}

// runInterrupt drives one interrupt with the submit lock held: it writes the
// keys once the harness has taken the prompt and the screen shows the turn
// still running, then waits for the turn to end, and clears the composer after
// a cancel.
func (c *Conversation) runInterrupt(ctx context.Context, ir turns.Interrupter, op *interruptOp, prompt string) (InterruptResult, error) {
	notifyCh, unsubscribe := c.screen.Subscribe()
	defer unsubscribe()
	poll := time.NewTicker(interruptPoll)
	defer poll.Stop()
	for {
		if outcome, ended := c.interruptEnded(op); ended {
			if outcome == InterruptCancelled {
				if err := c.clearComposer(ctx, ir, prompt); err != nil {
					if !errors.Is(err, ErrComposerNotCleared) {
						err = fmt.Errorf("%w: %w", ErrComposerNotCleared, err)
					}
					return InterruptCancelled, err
				}
			}
			return outcome, nil
		}
		snap := c.screen.Snapshot()
		c.observeBusy(snap)
		if c.turnAccepted() {
			switch outcome, partial := ir.InterruptOutcome(prompt, snap); outcome {
			case turns.InterruptStopped, turns.InterruptCancelled:
				c.finishInterrupted(op.turnID, outcome, partial)
				continue
			case turns.InterruptPending:
				if err := c.writeInterrupt(ir, op); err != nil {
					return "", err
				}
			case turns.InterruptFinished:
				// Ending on its own: no keys, the completion path ends it.
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %w", ErrInterruptUnconfirmed, ctx.Err())
		case <-c.closed:
			return "", ErrClosed
		case _, ok := <-notifyCh:
			if !ok {
				return "", ErrClosed
			}
		case <-poll.C:
		}
	}
}

// writeInterrupt writes the interrupt keys, once per op.
func (c *Conversation) writeInterrupt(ir turns.Interrupter, op *interruptOp) error {
	c.mu.Lock()
	if op.written {
		c.mu.Unlock()
		return nil
	}
	op.written = true
	c.interruptedBy = op.turnID
	c.mu.Unlock()
	return c.write(ir.InterruptSequence())
}

// interruptEnded reports whether op's turn has ended, and how: the interrupt
// outcome when an interrupt ended it, InterruptTooLate when it ended its own
// way.
func (c *Conversation) interruptEnded(op *interruptOp) (InterruptResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.currentTurn != nil && c.currentTurn.ID == op.turnID {
		return "", false
	}
	if op.outcome != "" {
		return op.outcome, true
	}
	return InterruptTooLate, true
}

// interruptInFlight reports whether an Interrupt has written its keys for the
// turn and awaits the harness's answer. The idle fallback holds off meanwhile:
// the screen it would complete from shows a reply cut short.
func (c *Conversation) interruptInFlight(turnID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interrupt != nil && c.interrupt.turnID == turnID && c.interrupt.written
}

// turnAccepted reports whether the harness has taken the in-flight prompt: the
// screen showed it working after the submit began.
func (c *Conversation) turnAccepted() bool {
	after := c.acceptAfter.Load()
	return after != 0 && c.lastBusyAt.Load() >= after
}

// observeInterrupt ends the in-flight turn when the screen shows the harness
// interrupted it — an Interrupt's keys, or a person pressing Esc at the
// terminal; the reading is the same. Called for every repaint.
func (c *Conversation) observeInterrupt(snap screen.Snapshot) {
	ir, ok := c.adapter.(turns.Interrupter)
	if !ok {
		return
	}
	c.mu.Lock()
	turn := c.currentTurn
	prompt := c.currentPrompt
	c.mu.Unlock()
	if turn == nil || !c.turnAccepted() {
		return
	}
	switch outcome, partial := ir.InterruptOutcome(prompt, snap); outcome {
	case turns.InterruptStopped, turns.InterruptCancelled:
		c.finishInterrupted(turn.ID, outcome, partial)
	}
}

// finishInterrupted ends turn turnID TurnStateInterrupted, unless it has
// already ended. partial is the reply the screen showed above the interrupt
// marker; the transcript's text replaces it once the harness has recorded the
// interrupt.
func (c *Conversation) finishInterrupted(turnID string, outcome turns.InterruptOutcome, partial string) {
	result := InterruptStopped
	if outcome == turns.InterruptCancelled {
		result = InterruptCancelled
	}
	c.mu.Lock()
	turn := c.currentTurn
	if turn == nil || turn.ID != turnID {
		c.mu.Unlock()
		return
	}
	c.currentTurn = nil
	c.endMarkerSeen = false
	c.heldReason = ""
	byCall := c.interruptedBy == turnID
	if op := c.interrupt; op != nil && op.turnID == turnID {
		op.outcome = result
	}
	c.mu.Unlock()

	turn.State = TurnStateInterrupted
	turn.CompletedAt = time.Now()
	turn.Reason = interruptReason(c.opts.Harness, result, byCall)
	turn.Text = ""
	if result == InterruptStopped {
		turn.Text = c.interruptedText(partial)
	}
	if err := c.store.UpdateTurn(context.Background(), turn); err != nil {
		c.emit(ConversationEvent{Type: EventTurn, Turn: *turn, Err: err})
		return
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: *turn})
}

// interruptReason is the Reason of an interrupted turn: what the harness did,
// and whether Interrupt or someone at the terminal asked it to.
func interruptReason(harness string, result InterruptResult, byCall bool) string {
	reason := harness + ": interrupted: the harness stopped the turn"
	if result == InterruptCancelled {
		reason = harness + ": interrupted: the harness cancelled the turn before it replied"
	}
	if !byCall {
		reason += " (at the terminal)"
	}
	return reason
}

// interruptedText returns the partial reply of a stopped turn: the last reply
// the harness recorded for it, once its transcript shows the interrupt — an
// entry of the user's after the turn's replies — and the screen's partial
// otherwise. One pause for a transcript that has not caught up yet.
func (c *Conversation) interruptedText(partial string) string {
	text, ok := c.interruptedTranscriptText()
	if !ok && c.waitForTranscriptFlush() {
		text, ok = c.interruptedTranscriptText()
	}
	if ok {
		return text
	}
	return partial
}

// interruptedTranscriptText reads the in-flight turn's entries in the
// harness's transcript: they run from the turn's prompt, past the watermark
// Send took, and end on the harness's record of the interrupt when it has
// written one. ok is false when there is no transcript to read or it has not
// recorded the interrupt yet.
func (c *Conversation) interruptedTranscriptText() (string, bool) {
	reader, ok := c.adapter.(turns.TranscriptReader)
	if !ok {
		return "", false
	}
	c.mu.Lock()
	sessionID := c.session.HarnessID()
	watermark := c.sentTranscriptWatermark
	c.mu.Unlock()
	if sessionID == "" || watermark == watermarkUnknown {
		return "", false
	}
	tturns, err := reader.ReadTranscript(sessionID, c.transcriptDir())
	if err != nil || len(tturns) < watermark+2 || Role(tturns[len(tturns)-1].Role) != RoleUser {
		return "", false
	}
	for i := len(tturns) - 2; i > watermark; i-- {
		t := tturns[i]
		if Role(t.Role) == RoleAssistant && t.APIError == "" {
			return strings.TrimSpace(t.Text), true
		}
	}
	return "", true
}

// clearComposer empties the harness's composer with the adapter's clear keys,
// re-reading it after each round until it reads empty. hint stands in for a
// composer that cannot be read before the first round (the prompt a cancel put
// back); with no hint, an unreadable composer is left alone — there is nothing
// to confirm, and Send types as it always has. The caller holds the submit
// lock.
func (c *Conversation) clearComposer(ctx context.Context, ir turns.Interrupter, hint string) error {
	notifyCh, unsubscribe := c.screen.Subscribe()
	defer unsubscribe()
	for round := 0; ; round++ {
		text, ok := ir.ComposerText(c.screen.Snapshot())
		switch {
		case ok && text == "":
			return nil
		case round == composerClearRounds:
			return fmt.Errorf("%w: it still holds %q", ErrComposerNotCleared, oneLineCapped(text, 80))
		case !ok && round > 0:
			return fmt.Errorf("%w: the composer is no longer on screen", ErrComposerNotCleared)
		case !ok && hint == "":
			return nil
		case !ok:
			text = hint
		}
		drain(notifyCh)
		if err := c.write(ir.ClearComposerSequence(text)); err != nil {
			return err
		}
		if err := c.awaitRepaint(ctx, notifyCh); err != nil {
			return err
		}
	}
}

// awaitRepaint waits for the screen to change and then settle, bounded by
// composerRepaintWait.
func (c *Conversation) awaitRepaint(ctx context.Context, notifyCh <-chan struct{}) error {
	bound := time.NewTimer(composerRepaintWait)
	defer bound.Stop()
	var settle <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.closed:
			return ErrClosed
		case <-bound.C:
			return nil
		case <-settle:
			return nil
		case _, ok := <-notifyCh:
			if !ok {
				return ErrClosed
			}
			settle = time.After(composerSettle)
		}
	}
}

// drain empties a notification channel without blocking.
func drain(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
