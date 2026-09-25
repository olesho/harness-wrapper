package chat

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/olesho/harness-wrapper/internal/delivery"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Event delivery (ADR-008). Every event a conversation generates goes through
// one bounded queue and one worker, which hands it to Options.OnEvent and then
// offers it to Events(): in order, each once, while the process lives. A
// producer that finds the queue full waits for room outside every lock of the
// conversation; the last event, EventExited, never waits.

// ExitInfo is how the harness process ended.
type ExitInfo struct {
	Status   wrapper.Status
	ExitCode int
	Signal   string
	Reason   string
	Class    wrapper.ErrorClass
	EndedAt  time.Time
}

// State is a conversation's live state, read in one call (ADR-008).
type State struct {
	// Alive reports that the harness process is running; PID is its id.
	Alive bool
	PID   int

	// Turn is the assistant turn in flight, nil when none.
	Turn *Turn
	// Input is the interactive prompt awaiting an answer, nil when none.
	Input *InputRequest
	// Busy reports that the screen shows the harness working
	// (turns.BusyDetector); false for an adapter that cannot tell.
	Busy bool

	// LastOutputAt is when the harness last wrote; zero before it has.
	LastOutputAt time.Time
	// Status, StatusReason and ClassifiedAt are the wrapper's latest
	// classification of the harness's output (wrapper.Snapshot).
	Status       wrapper.Status
	StatusReason string
	ClassifiedAt time.Time

	// HarnessSessionID is the harness's own session id, empty until known.
	HarnessSessionID string

	// Exit is how the process ended, nil while it runs.
	Exit *ExitInfo

	// RateLimit is the account's usage limit as the harness last reported
	// it (EventRateLimit); nil until it has, and always on the TUI
	// transport.
	RateLimit *RateLimit

	// Delivery is the event queue's pressure.
	Delivery DeliveryState
}

// DeliveryState is the event queue's pressure, for diagnostics: a consumer
// that stops taking events shows here before anything else notices.
type DeliveryState struct {
	Limits      wrapper.QueueLimits
	Queued      int
	QueuedBytes int64
	// WaitingSince is when the longest-waiting producer began waiting for
	// room; zero when none waits.
	WaitingSince time.Time
	// DeliveringSince is when the OnEvent call now running began; zero when
	// none is.
	DeliveringSince time.Time
	Delivered       uint64
	// Undelivered counts events generated and never delivered: Close's
	// drain ran out of time.
	Undelivered uint64
	// Oversized counts events delivered without their text for exceeding
	// the byte bound (ErrEventTooLarge).
	Oversized uint64
}

// Done is closed once the harness process has ended. It says nothing about
// delivery: Events() closes after the last event is delivered.
func (c *Conversation) Done() <-chan struct{} { return c.done }

// State returns the conversation's live state.
func (c *Conversation) State() State {
	c.mu.Lock()
	st := State{
		HarnessSessionID: c.session.HarnessID(),
		Exit:             c.exit,
	}
	if c.currentTurn != nil {
		t := *c.currentTurn
		st.Turn = &t
	}
	c.mu.Unlock()
	st.Input = c.PendingInput()
	if c.stream != nil {
		c.streamState(&st)
	}
	if c.sess != nil {
		st.PID = c.sess.PID()
		snap := c.sess.Snapshot()
		st.LastOutputAt = snap.LastOutputAt
		st.Status, st.StatusReason, st.ClassifiedAt = snap.Status, snap.Reason, snap.ClassifiedAt
		st.Alive = st.Exit == nil
	}
	if bd, ok := c.adapter.(turns.BusyDetector); ok && c.screen != nil && st.Alive {
		st.Busy = bd.Busy(c.screen.Snapshot())
	}
	if c.delivery != nil {
		qs := c.delivery.Stats()
		st.Delivery = DeliveryState{
			Limits:          wrapper.QueueLimits(qs.Limits),
			Queued:          qs.Queued,
			QueuedBytes:     qs.QueuedBytes,
			WaitingSince:    qs.WaitingSince,
			DeliveringSince: qs.DeliveringSince,
			Delivered:       qs.Delivered,
			Undelivered:     qs.Undelivered,
			Oversized:       c.oversized.Load(),
		}
	}
	return st
}

// emit queues ev for delivery, waiting for room while the queue is full. It
// must never be called holding c.mu or the submit lock. An event over the
// byte bound is delivered without its turn's text, carrying ErrEventTooLarge.
func (c *Conversation) emit(ev ConversationEvent) {
	if c.delivery == nil {
		// A Conversation assembled without Open (tests): Events() only.
		select {
		case c.eventCh <- ev:
		default:
		}
		return
	}
	err := c.delivery.Push(ev, c.abandoned)
	if errors.Is(err, delivery.ErrTooLarge) {
		c.oversized.Add(1)
		ev.Turn.Text = ""
		ev.Err = fmt.Errorf("%w: its text is in History", ErrEventTooLarge)
		_ = c.delivery.Push(ev, c.abandoned)
	}
}

// deliver is the delivery worker's callback: OnEvent first, then Events(),
// which never waits — it is the best-effort view.
func (c *Conversation) deliver(ev ConversationEvent) {
	if c.opts.OnEvent != nil {
		c.opts.OnEvent(ev)
	}
	select {
	case c.eventCh <- ev:
	default:
	}
}

// eventSize is an event's payload bytes, for the queue's byte bound.
func eventSize(ev ConversationEvent) int64 {
	n := int64(256 + len(ev.Turn.Text) + len(ev.Turn.Reason))
	if rl := ev.RateLimit; rl != nil {
		n += int64(len(rl.Window))
		for k := range rl.Windows {
			n += int64(64 + len(k))
		}
	}
	if in := ev.Input; in != nil {
		n += int64(len(in.Prompt) + len(in.Header))
		for _, o := range in.Options {
			n += int64(len(o.Label) + len(o.Description) + len(o.ID) + len(o.Alias))
		}
	}
	return n
}

// statusItem is one wrapper event on its way to the event loop: the turn
// events it maps to, and the event itself when it is the last.
type statusItem struct {
	events []turns.Event
	final  *wrapper.SessionEvent
}

// onWrapperEvent is the wrapper's OnEvent: every session event, in order,
// hands its turn events to the event loop, so none is dropped however busy
// the loop is.
func (c *Conversation) onWrapperEvent(ev wrapper.SessionEvent) {
	it := statusItem{events: turns.StatusEvents(c.adapter, ev)}
	if ev.Terminated {
		final := ev
		it.final = &final
	}
	select {
	case c.statusCh <- it:
	case <-c.abandoned:
	}
}

// handleExit runs on the event loop once the wrapper's last event has been
// handled — its turn events first, so the turn in flight has ended. It ends a
// turn the harness left in flight, waits for every turn ending already under
// way to queue its event, and queues EventExited last.
func (c *Conversation) handleExit(final wrapper.SessionEvent) {
	info := ExitInfo{Status: final.Status, Reason: final.Reason, Class: final.Class, EndedAt: final.At}
	if c.sess != nil {
		// Returns at once: the wrapper queues its last event as its last act.
		if res, _ := c.sess.Wait(); res.Status != "" {
			info = ExitInfo{
				Status: res.Status, ExitCode: res.ExitCode, Signal: res.Signal,
				Reason: res.Reason, Class: res.Class, EndedAt: res.EndedAt,
			}
		}
	}
	c.exitWith(info)
}

// exitWith records how the harness ended and ends the conversation's stream:
// the turn it left in flight ends errored, every turn ending already under
// way queues its event, and EventExited goes last.
func (c *Conversation) exitWith(info ExitInfo) {
	c.mu.Lock()
	c.exit = &info
	c.mu.Unlock()
	close(c.done)

	// A Send that had begun recording its turn finishes doing so, so its
	// turn is the one ended here, before EventExited.
	c.sending.Wait()
	c.mu.Lock()
	turn := c.claimTurnLocked()
	c.mu.Unlock()
	if turn != nil {
		turn.State = TurnStateErrored
		turn.CompletedAt = time.Now()
		turn.Reason = c.opts.Harness + ": harness exited (" + string(info.Status) + ")"
		c.finishTurn(turn, nil)
	}
	c.ending.Wait()
	c.delivery.PushLast(ConversationEvent{Type: EventExited, Exit: &info})
}

// claimTurnLocked takes the turn in flight to end it, holding EventExited
// until its terminal event is queued (finishTurn). nil when none is in
// flight. Caller holds c.mu.
func (c *Conversation) claimTurnLocked() *Turn {
	turn := c.currentTurn
	if turn == nil {
		return nil
	}
	c.currentTurn = nil
	c.ending.Add(1)
	return turn
}

// finishTurn publishes a turn claimed by claimTurnLocked, then releases
// EventExited.
func (c *Conversation) finishTurn(turn *Turn, err error) {
	defer c.ending.Done()
	c.publishTurn(turn, err)
}

// publishTurn stores turn and emits it; err, or a store failure, rides on
// the event.
func (c *Conversation) publishTurn(turn *Turn, err error) {
	if uerr := c.store.UpdateTurn(context.Background(), turn); uerr != nil && err == nil {
		err = uerr
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: *turn, Err: err})
}

// drain waits for every event to be delivered, until ctx ends: then the rest
// is dropped and counted, producers waiting for room give up, and Close
// reports ErrUndelivered.
func (c *Conversation) drain(ctx context.Context) error {
	if c.delivery == nil {
		return nil
	}
	select {
	case <-c.delivery.Drained():
		return nil
	case <-ctx.Done():
	}
	c.abandonOnce.Do(func() { close(c.abandoned) })
	n := c.delivery.Stats().Queued
	c.delivery.Abandon()
	return fmt.Errorf("%w: %d still queued: %w", ErrUndelivered, n, ctx.Err())
}
