package chat

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/gemini"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/opencode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/pi"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Options configures a single Conversation.
type Options struct {
	// Harness names the per-harness adapter ("codex", "claude-code",
	// "gemini", "opencode", "pi", "generic"). Required.
	Harness string

	// BinaryPath is the harness executable. Required.
	BinaryPath string

	// Args are passed verbatim to the harness.
	Args []string

	// WorkingDir is the harness's working directory. Defaults to the
	// current process's CWD.
	WorkingDir string

	// Env is the harness's environment. Defaults to the current
	// process's environment.
	Env []string

	// Cols, Rows configure the virtual PTY size. Defaults: 120x40.
	Cols, Rows int

	// Store backs the chat metadata. Required; pass memstore.New() for
	// the in-process default (the memstore package is kept separate so
	// pkg/chat has no dependency on a specific persistence backend).
	Store Store

	// EventBuffer sizes the Conversation.Events() channel. Defaults to 32.
	EventBuffer int

	// InputPolicy pre-configures how blocking interactive prompts (e.g. the
	// folder-trust dialog) are resolved without a live client. It is
	// consulted first; when it yields "ask" (or is nil), the request falls
	// through to OnInputRequest and then to Events(). JSON-serializable so
	// transports can accept it at open time.
	InputPolicy *InputPolicy

	// DisableCodexAutoDismiss turns off the built-in auto-dismissal of Codex's
	// blocking startup interstitials (the "Update available!" menu, the
	// model-migration screen). The zero value keeps auto-dismiss ENABLED: by
	// default the chat layer clears those interstitials (selecting "Skip" on
	// the update menu, never "Update now") so a stale Codex does not wedge the
	// conversation. Set true to instead surface them on Events() for the
	// client/InputPolicy to answer. This governs only those startup
	// interstitial kinds — Codex's real approval prompts are never
	// auto-dismissed regardless of this flag.
	DisableCodexAutoDismiss bool

	// OnInputRequest is an in-process resolver consulted when InputPolicy
	// did not auto-answer. Returning ok=true answers the prompt with the
	// returned InputAnswer; returning ok=false surfaces the request on
	// Events() for an external client to Answer. It runs on the event pump
	// goroutine, so it must return promptly. Not used by remote transports
	// (they answer over the wire instead).
	OnInputRequest func(InputRequest) (InputAnswer, bool)
}

// Conversation owns one supervised harness process and serves the
// chat-style API on top of it.
type Conversation struct {
	opts    Options
	store   Store
	adapter turns.Adapter

	sess    *wrapper.Session
	screen  *screen.Screen
	watcher *turns.Watcher

	releaseWriter func()

	queue *controlQueue

	session Session // chat-level Session record (also stored in Store)

	eventCh chan ConversationEvent

	mu          sync.Mutex
	currentTurn *Turn // pending/streaming assistant turn, if any

	// endMarkerSeen is set once the adapter has reported an end-of-turn marker
	// for the in-flight turn (claude-code only; see handleTurnsEvent). It does
	// NOT complete the turn on its own — Claude prints a "✻ <verb> for Ns"
	// summary after every thinking block, and the "still working" footer can
	// flicker out for a redraw frame while sub-agents/tools run, so an instant
	// marker-complete cuts the turn off mid-work. Instead the marker shortens the
	// idle-completion gap (markerConfirmGap) so completion still requires the
	// screen to quiesce at a non-busy prompt — robust against intermediate
	// markers, which are always followed by more activity. Reset on each Send.
	endMarkerSeen bool
	// markerArmCh wakes the idle-completion watcher to re-arm on the short gap the
	// moment a marker lands (so a settled end-of-turn confirms promptly, not after
	// the full fallback gap). Buffered (1), non-blocking sender.
	markerArmCh chan struct{}

	// currentInput is the blocking interactive prompt awaiting an answer, or
	// nil. inputSurfaced is true once it has been emitted to the client (no
	// policy/handler resolved it), which makes Send fail fast rather than
	// block. inputStateCh is a buffered (1) wake signal for a Send blocked in
	// waitReadyForSend so it re-checks when input state changes between
	// screen redraws.
	currentInput  *turns.InputRequest
	inputSurfaced bool
	inputStateCh  chan struct{}

	// writeStdin, when non-nil, replaces sess.WriteStdin for interactive
	// answer keystrokes. Production leaves it nil (writes go to the PTY); it
	// exists so the input-resolution path is testable without a live session.
	writeStdin func([]byte) (int, error)

	closeOnce sync.Once
	closed    chan struct{}
}

// Open starts a harness, wires the screen + turn watcher, and returns
// a live Conversation.
func Open(ctx context.Context, opts Options) (*Conversation, error) {
	if opts.Harness == "" || opts.BinaryPath == "" {
		return nil, fmt.Errorf("%w: Harness and BinaryPath are required", ErrInvalidOptions)
	}
	if opts.Store == nil {
		return nil, fmt.Errorf("%w: Store is required (pass memstore.New() for the default)", ErrInvalidOptions)
	}
	if opts.Cols <= 0 {
		opts.Cols = 120
	}
	if opts.Rows <= 0 {
		opts.Rows = 40
	}
	if opts.EventBuffer <= 0 {
		opts.EventBuffer = 32
	}

	adapter, err := resolveAdapter(opts.Harness)
	if err != nil {
		return nil, err
	}

	scr := screen.New(opts.Cols, opts.Rows)

	sess, err := wrapper.Start(ctx, wrapper.Config{
		BinaryPath: opts.BinaryPath,
		Args:       opts.Args,
		WorkingDir: opts.WorkingDir,
		Env:        opts.Env,
		Stdin:      nil,
		Stdout:     scr,
		Harness:    opts.Harness,
	})
	if err != nil {
		return nil, fmt.Errorf("chat: wrapper start: %w", err)
	}

	releaseWriter, ok := sess.AcquireWriter()
	if !ok {
		// Should be impossible immediately after Start; treat as fatal.
		_ = sess.Stop(context.Background())
		return nil, fmt.Errorf("chat: failed to acquire wrapper writer lock")
	}

	// Match the PTY size to the virtual screen size so the harness's
	// re-renders target the same dimensions our emulator is tracking.
	_ = sess.Resize(uint16(opts.Cols), uint16(opts.Rows))

	sessionRec := Session{
		ID:         newID(),
		Harness:    opts.Harness,
		WorkingDir: opts.WorkingDir,
		CreatedAt:  time.Now(),
	}
	if err := opts.Store.CreateSession(ctx, &sessionRec); err != nil {
		releaseWriter()
		_ = sess.Stop(context.Background())
		return nil, fmt.Errorf("chat: store CreateSession: %w", err)
	}

	c := &Conversation{
		opts:          opts,
		store:         opts.Store,
		adapter:       adapter,
		sess:          sess,
		screen:        scr,
		releaseWriter: releaseWriter,
		queue:         newControlQueue(),
		session:       sessionRec,
		eventCh:       make(chan ConversationEvent, opts.EventBuffer),
		inputStateCh:  make(chan struct{}, 1),
		markerArmCh:   make(chan struct{}, 1),
		closed:        make(chan struct{}),
	}
	c.watcher = turns.Watch(sess, scr, adapter)

	go c.consumeWatcher()
	go c.idleCompletionWatcher()

	return c, nil
}

// SessionID returns the chat-level session ID. Distinct from the
// underlying harness's session ID (which is Session.HarnessSessionID
// and is empty in v1 until adapter-level extraction lands).
func (c *Conversation) SessionID() string { return c.session.ID }

// Adapter returns the per-harness turns adapter backing this conversation, so
// callers can probe its optional capabilities (e.g. turns.Quitter for a
// graceful-exit sequence).
func (c *Conversation) Adapter() turns.Adapter { return c.adapter }

// ScreenSnapshot returns a coherent point-in-time view of the conversation's
// rendered terminal — the vt100-emulated screen the turn detector reads from.
// Safe to call concurrently with the conversation running; the snapshot
// reflects the screen as of the call and the underlying terminal keeps
// mutating independently. This is a pure read: it needs no control token, so
// any number of observers can inspect a live (e.g. stuck) harness without
// disturbing it.
func (c *Conversation) ScreenSnapshot() screen.Snapshot { return c.screen.Snapshot() }

// Events returns the channel of turn-state transitions. Closed after
// Close has completed and the watcher has drained.
func (c *Conversation) Events() <-chan ConversationEvent { return c.eventCh }

// AcquireControl blocks until this caller is granted the exclusive
// control token. Holders are queued FIFO. The returned release function
// passes the token to the next waiter (or leaves it free); call it
// (typically with defer) when done sending messages.
func (c *Conversation) AcquireControl(ctx context.Context) (release func(), err error) {
	return c.queue.Acquire(ctx)
}

// Close terminates the harness process, releases the wrapper writer
// lock, stops the watcher, and closes the events channel. Safe to call
// multiple times.
func (c *Conversation) Close(ctx context.Context) error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.queue.Close()
		if c.releaseWriter != nil {
			c.releaseWriter()
		}
		if c.sess != nil {
			_ = c.sess.Stop(ctx)
		}
		if c.watcher != nil {
			_ = c.watcher.Close()
		}
	})
	return nil
}

// consumeWatcher pumps turns.Event from the watcher into Conversation
// state and emits TurnEvent on c.eventCh.
func (c *Conversation) consumeWatcher() {
	defer close(c.eventCh)
	for ev := range c.watcher.Events() {
		c.handleTurnsEvent(ev)
	}
}

// handleTurnsEvent translates a low-level turns.Event into a
// chat-level Turn state transition. If there is no current assistant
// turn (e.g. an adapter signal fired before the first Send), the event
// is ignored as a stale heuristic firing.
//
// On every TurnComplete the watcher loop also opportunistically tries
// to extract the harness's own session ID — most harnesses print it as
// part of their end-of-turn footer, so this is the natural moment to
// grab it.
func (c *Conversation) handleTurnsEvent(ev turns.Event) {
	// Interactive input prompts are independent of turn state — handle them
	// before the current-turn machinery and return.
	switch ev.Kind {
	case turns.InputRequested:
		c.handleInputRequested(ev.Input)
		return
	case turns.InputResolved:
		c.handleInputResolved(ev.Input)
		return
	}

	if ev.Kind == turns.TurnComplete {
		c.maybeExtractSessionID()

		// Claude Code: a marker does NOT complete the turn outright. It prints a
		// "✻ <verb> for Ns" summary after EVERY thinking block, and the "esc to
		// interrupt" footer can flicker out for a redraw frame while sub-agents or
		// tools run — so an instant complete on a non-busy frame cuts the turn off
		// mid-work (the captured reply is then a pre-final preamble). Record the
		// marker and let the idle-completion watcher confirm it once the screen
		// quiesces at a non-busy prompt (markerConfirmGap). An intermediate marker
		// is always followed by more activity, so it never confirms; the genuine
		// end-of-turn marker, followed by a settled prompt, confirms in ~2s.
		// Other harnesses (codex) keep the instant marker path below.
		if c.opts.Harness == "claude-code" {
			c.mu.Lock()
			pending := c.currentTurn != nil
			if pending {
				c.endMarkerSeen = true
			}
			c.mu.Unlock()
			if pending {
				select {
				case c.markerArmCh <- struct{}{}:
				default:
				}
				return
			}
		}
	}

	c.mu.Lock()
	turn := c.currentTurn
	c.currentTurn = nil
	c.mu.Unlock()

	if turn == nil {
		return
	}

	switch ev.Kind {
	case turns.TurnComplete:
		turn.State = TurnStateComplete
		turn.CompletedAt = ev.At
		turn.Reason = ev.Reason
		if ev.Snap != nil {
			turn.Text = c.assistantText(*ev.Snap)
		}
	case turns.Blocked, turns.Errored:
		turn.State = TurnStateErrored
		turn.CompletedAt = ev.At
		turn.Reason = ev.Reason
		turn.HTTPCode = ev.HTTPCode
		turn.RetryAfter = ev.RetryAfter
	case turns.ToolCall:
		// ToolCall is informational mid-turn; restore the current-turn
		// pointer so the next event can complete the turn.
		c.mu.Lock()
		c.currentTurn = turn
		c.mu.Unlock()
		return
	default:
		// Unknown kind — leave turn as-is, restore pointer.
		c.mu.Lock()
		c.currentTurn = turn
		c.mu.Unlock()
		return
	}

	if err := c.store.UpdateTurn(context.Background(), turn); err != nil {
		c.emit(ConversationEvent{Type: EventTurn, Turn: *turn, Err: err})
		return
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: *turn})
}

// idleCompletionGap is how long the rendered screen must sit completely
// unchanged, at the ready prompt, before an in-flight turn is treated as
// complete by the idle fallback. Claude Code animates its working spinner
// (and a per-second elapsed counter) while a turn runs, so any screen update
// resets the timer — it only elapses once Claude has stopped and returned to
// the prompt.
const idleCompletionGap = 8 * time.Second

// markerConfirmGap is the (shorter) quiet window used once an end-of-turn marker
// has been seen for the in-flight turn. The marker is strong evidence the turn
// ended; we only need to confirm the screen then SETTLED at a non-busy prompt
// (rather than continuing into the next tool call), which distinguishes a genuine
// end-of-turn marker from an intermediate one. Must exceed Claude's working-frame
// cadence (its spinner repaints ~1×/s, resetting the timer) so it never elapses
// mid-work; 2s clears that with margin while keeping per-turn latency low.
const markerConfirmGap = 2 * time.Second

// idleCompletionWatcher is a fallback end-of-turn detector. The primary
// detector is the adapter's screen marker (Claude Code's "✻ <verb> for Ns"
// summary); when that marker is missed — it can scroll off before a snapshot
// captures it — the assistant turn's currentTurn pointer would otherwise stay
// set forever and 409 (turn_in_flight) every subsequent Send. This watcher
// closes that gap: if a turn is in flight and the screen has been idle at the
// ready prompt for idleCompletionGap, the turn is completed from the settled
// screen. No-op for harnesses without prompt-readiness semantics.
func (c *Conversation) idleCompletionWatcher() {
	if !requiresPromptReadiness(c.opts.Harness) {
		return
	}
	notifyCh, unsubscribe := c.screen.Subscribe()
	defer unsubscribe()
	timer := time.NewTimer(idleCompletionGap)
	defer timer.Stop()
	// gap is the quiet window the screen must hold before we try to complete: the
	// short markerConfirmGap once an end-of-turn marker has been seen, else the
	// long fallback. Recomputed on every re-arm so a mid-turn marker promptly
	// switches the watcher to the fast confirmation.
	reset := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		c.mu.Lock()
		marker := c.endMarkerSeen
		c.mu.Unlock()
		gap := idleCompletionGap
		if marker {
			gap = markerConfirmGap
		}
		timer.Reset(gap)
	}
	for {
		select {
		case <-c.closed:
			return
		case _, ok := <-notifyCh:
			if !ok {
				return
			}
			reset()
		case <-c.markerArmCh:
			// A marker just landed — re-arm on the short gap even if the screen
			// has already gone quiet (no further notify would otherwise come).
			reset()
		case <-timer.C:
			c.maybeIdleComplete()
			reset()
		}
	}
}

// maybeIdleComplete completes the in-flight turn if (and only if) the screen
// has settled at the ready prompt — Claude finished and the end-of-turn marker
// was not observed. Guards: no pending input dialog, the prompt is actually
// ready, and the turn has been in flight at least idleCompletionGap (so a
// just-sent turn is never closed on residual pre-send idle). A real marker
// event that fires first clears currentTurn and makes this a no-op.
func (c *Conversation) maybeIdleComplete() {
	c.mu.Lock()
	turn := c.currentTurn
	c.mu.Unlock()
	if turn == nil {
		return
	}
	if c.inputAwaitingClient() {
		return
	}
	c.mu.Lock()
	marker := c.endMarkerSeen
	c.mu.Unlock()
	snap := c.screen.Snapshot()
	// Two completion modes share this settled-screen check:
	//   - marker-confirmed (claude-code): an end-of-turn marker was reported and
	//     the screen then held quiet for markerConfirmGap. The marker is the
	//     authoritative end signal, so we do NOT also require readyForInput (its
	//     header/prompt heuristic can lag a frame); !Busy + the quiet window are
	//     enough, and avoid hanging a finished turn on a missed header.
	//   - fallback (no marker, or non-claude harness): the marker was missed, so
	//     prompt-readiness is the only end signal — require it.
	if !marker && !readyForInput(c.opts.Harness, snap.Text) {
		return
	}
	// The harness's input prompt is often painted even while it works, so
	// prompt-readiness alone can't tell "done" from "thinking/running a tool".
	// If the adapter can report that the harness is still busy, honor it — never
	// idle-complete a turn that's still in flight (that would cut work short).
	if bd, ok := c.adapter.(turns.BusyDetector); ok && bd.Busy(snap) {
		return
	}
	gap := idleCompletionGap
	if marker {
		gap = markerConfirmGap
	}
	if time.Since(turn.StartedAt) < gap {
		return
	}

	c.mu.Lock()
	if c.currentTurn == nil || c.currentTurn.ID != turn.ID {
		c.mu.Unlock()
		return
	}
	c.currentTurn = nil
	c.endMarkerSeen = false
	c.mu.Unlock()

	turn.State = TurnStateComplete
	turn.CompletedAt = time.Now()
	if marker {
		turn.Reason = "claude-code: end-of-turn marker confirmed at a settled prompt"
	} else {
		turn.Reason = "claude-code: idle-completion fallback (end-of-turn marker not observed)"
	}
	turn.Text = snap.Text
	if err := c.store.UpdateTurn(context.Background(), turn); err != nil {
		c.emit(ConversationEvent{Type: EventTurn, Turn: *turn, Err: err})
		return
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: *turn})
}

// maybeExtractSessionID opportunistically scrapes the harness's own
// session ID from the rendered screen. Once we've persisted one, we
// don't probe again. No-op for adapters that don't implement
// turns.SessionIDExtractor.
func (c *Conversation) maybeExtractSessionID() {
	c.mu.Lock()
	if c.session.HarnessSessionID != "" {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	ext, ok := c.adapter.(turns.SessionIDExtractor)
	if !ok {
		return
	}
	id, ok := ext.ExtractSessionID(c.screen.Snapshot())
	if !ok {
		return
	}

	c.mu.Lock()
	c.session.HarnessSessionID = id
	updated := c.session
	c.mu.Unlock()
	_ = c.store.UpdateSession(context.Background(), &updated)
}

// History returns the conversation history for this Conversation.
//
// When the adapter supports turns.TranscriptReader and the harness
// session ID is known, History reads the harness's own JSONL log and
// returns its parsed contents — this is the higher-fidelity source
// because the harness records exactly what the model said, not what
// the TUI rendered.
//
// When transcript reading isn't possible (adapter has no reader, or
// the harness session ID has not yet been extracted), History falls
// back to the Store's recorded turns. The fallback only contains the
// user-side text and any screen-derived assistant text the watcher
// captured at TurnComplete.
// assistantText returns the clean assistant reply for a completed turn: when
// the adapter implements turns.MessageExtractor (e.g. claude-code) and can
// isolate the message from the rendered TUI, that cleaned text is used;
// otherwise we fall back to the raw screen snapshot. This keeps screen-derived
// Turn.Text parseable (a one-shot reply, not a full-screen dump) without
// depending on a persisted transcript.
func (c *Conversation) assistantText(snap screen.Snapshot) string {
	if ex, ok := c.adapter.(turns.MessageExtractor); ok {
		if msg, ok := ex.ExtractMessage(snap); ok {
			return msg
		}
	}
	return snap.Text
}

func (c *Conversation) History(ctx context.Context) ([]Turn, error) {
	c.mu.Lock()
	sessionCopy := c.session
	c.mu.Unlock()

	reader, hasReader := c.adapter.(turns.TranscriptReader)
	if !hasReader || sessionCopy.HarnessSessionID == "" {
		return c.store.ListTurns(ctx, sessionCopy.ID)
	}

	tturns, err := reader.ReadTranscript(sessionCopy.HarnessSessionID, c.opts.WorkingDir)
	if err != nil {
		return nil, fmt.Errorf("chat: read transcript: %w", err)
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
	return out, nil
}

// emit pushes an event onto the chan. Drops if the buffer is full
// rather than blocking the watcher pump.
func (c *Conversation) emit(ev ConversationEvent) {
	select {
	case c.eventCh <- ev:
	case <-c.closed:
	default:
		// Buffer full — drop. Slow consumers lose events; this matches
		// the wrapper's own slow-consumer policy.
	}
}

// resolveAdapter maps Options.Harness to a concrete turns.Adapter.
func resolveAdapter(name string) (turns.Adapter, error) {
	switch name {
	case "codex":
		return codex.New(), nil
	case "claude-code":
		return claudecode.New(), nil
	case "gemini":
		return gemini.New(), nil
	case "opencode":
		return opencode.New(), nil
	case "pi":
		return pi.New(), nil
	case "generic", "":
		return generic.New(), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownHarness, name)
	}
}
