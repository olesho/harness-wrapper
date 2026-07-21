package chat

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/opencode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/pi"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Options configures a single Conversation.
type Options struct {
	// Harness names the per-harness adapter ("codex", "claude-code",
	// "opencode", "pi", "generic"). Required.
	Harness string

	// BinaryPath is the harness executable. Required.
	BinaryPath string

	// Args are passed verbatim to the harness.
	Args []string

	// Resume is a harness session id to resume instead of starting a fresh
	// session. When set, the adapter must implement turns.SessionResumer (else
	// Open returns ErrResumeUnsupported); its ResumeArgs fragment is prepended
	// to Args, and Session.HarnessSessionID is seeded with this id. When set,
	// Args must not carry any flag the adapter reserves via
	// turns.SessionControlFlags (else Open returns ErrInvalidOptions).
	Resume string

	// WorkingDir is the harness's working directory. Defaults to the
	// current process's CWD.
	WorkingDir string

	// Env is the harness's environment. Defaults to the current
	// process's environment.
	Env []string

	// Effort and Model are execution-mode knobs forwarded to wrapper.Config; see
	// harness-wrapper wrapper.Config. Empty leaves the harness default.
	Effort string
	Model  string

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
	// blocking startup interstitials that carry no user choice — the
	// model-migration screen and menu-less "Press enter to continue" notices.
	// The zero value keeps their auto-dismiss ENABLED so a stale Codex does not
	// wedge the conversation; set true to instead surface them on Events() for
	// the client/InputPolicy to answer. The "Update available!" menu is NOT
	// governed by this flag — it surfaces by default and is controlled by
	// AutoSkipCodexUpdateNotice. This governs only those startup interstitial
	// kinds — Codex's real approval prompts are never auto-dismissed regardless.
	DisableCodexAutoDismiss bool

	// AutoSkipCodexUpdateNotice re-enables the built-in auto-Skip of Codex's
	// "Update available!" startup menu. The zero value SURFACES that menu on
	// Events() (as a codex_update_notice InputRequest) so a client can choose
	// Update / Skip; set true to instead have the chat layer transparently
	// select "Skip" (never "Update now") without surfacing it — the safe
	// default for headless/no-client callers (the one-shot run CLI, structured
	// runner) that would otherwise wedge on the pending menu. Ignored when
	// DisableCodexAutoDismiss is set (that surfaces every interstitial). An
	// InputPolicy entry for codex_update_notice still takes precedence over
	// this flag, as it is consulted first.
	AutoSkipCodexUpdateNotice bool

	// OnInputRequest is an in-process resolver consulted when InputPolicy
	// did not auto-answer. Returning ok=true answers the prompt with the
	// returned InputAnswer; returning ok=false surfaces the request on
	// Events() for an external client to Answer. It runs on the event pump
	// goroutine, so it must return promptly. Not used by remote transports
	// (they answer over the wire instead).
	OnInputRequest func(InputRequest) (InputAnswer, bool)

	// idleGap, markerGap optionally override the idle-completion windows
	// (idleCompletionGap / markerConfirmGap) for a single Conversation. They
	// are unexported on purpose: only same-package tests set them (the
	// PTY-driven integration suite shrinks them so it runs in ~1s). Zero means
	// "use the package default". Set once at Open and never mutated, so the
	// idleCompletionWatcher goroutine reads them race-free.
	idleGap, markerGap time.Duration
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

	resizeMu sync.Mutex

	closeOnce sync.Once
	closed    chan struct{}
}

// Open starts a fresh harness session, wires the screen + turn watcher, and
// returns a live Conversation. To resume a prior harness session instead, set
// Options.Resume (or use Reopen with a stored chat session id).
func Open(ctx context.Context, opts Options) (*Conversation, error) {
	session := Session{
		ID:         newID(),
		Harness:    opts.Harness,
		WorkingDir: opts.WorkingDir,
		CreatedAt:  time.Now(),
	}
	return openWithSession(ctx, opts, session, true)
}

// ReopenOptions configures Reopen. It is the Options knobs that make sense when
// re-attaching to an already-stored session: the harness, working dir, and
// resume id come from the stored record (looked up by SessionID), so they are
// intentionally omitted here (mirrors the TS Omit<Options,"harness"|"workingDir"|"resume">).
type ReopenOptions struct {
	// SessionID is the chat-level session id to reopen. The stored record
	// supplies Harness, WorkingDir, and the harness session id to resume.
	SessionID string

	// The remaining fields mirror the identically-named Options knobs; see
	// Options for their semantics.
	BinaryPath              string
	Args                    []string
	Env                     []string
	Effort                  string
	Model                   string
	Cols, Rows              int
	Store                   Store
	EventBuffer             int
	InputPolicy             *InputPolicy
	DisableCodexAutoDismiss bool
	OnInputRequest          func(InputRequest) (InputAnswer, bool)

	// idleGap, markerGap mirror the unexported Options test knobs; only
	// same-package tests set them. See Options.idleGap / Options.markerGap.
	idleGap, markerGap time.Duration
}

// Reopen resumes a previously-stored chat session against its harness's own
// persisted session, re-attaching a fresh live Conversation. It looks up the
// stored record by SessionID, requires it to carry a harness session id, and
// launches the harness with the adapter's resume args spliced in. Unlike Open
// it does NOT create a new store record — the record already exists.
func Reopen(ctx context.Context, opts ReopenOptions) (*Conversation, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("%w: Store is required (pass memstore.New() for the default)", ErrInvalidOptions)
	}
	rec, err := opts.Store.GetSession(ctx, opts.SessionID)
	if err != nil {
		return nil, err
	}
	if rec.HarnessSessionID == "" {
		return nil, fmt.Errorf("chat: session %s has no harness session id: %w", opts.SessionID, ErrNoHarnessSession)
	}

	launch := Options{
		Harness:                 rec.Harness,
		BinaryPath:              opts.BinaryPath,
		Args:                    opts.Args,
		WorkingDir:              rec.WorkingDir,
		Env:                     opts.Env,
		Resume:                  rec.HarnessSessionID,
		Effort:                  opts.Effort,
		Model:                   opts.Model,
		Cols:                    opts.Cols,
		Rows:                    opts.Rows,
		Store:                   opts.Store,
		EventBuffer:             opts.EventBuffer,
		InputPolicy:             opts.InputPolicy,
		DisableCodexAutoDismiss: opts.DisableCodexAutoDismiss,
		OnInputRequest:          opts.OnInputRequest,
		idleGap:                 opts.idleGap,
		markerGap:               opts.markerGap,
	}
	return openWithSession(ctx, launch, *rec, false)
}

// openWithSession is the shared launch/wiring body behind Open and Reopen. It
// attaches the supplied chat Session (Open mints a fresh one; Reopen reuses the
// stored record) and, when persist is set, inserts it via Store.CreateSession
// after launch. Reopen passes persist=false because the record already exists.
func openWithSession(ctx context.Context, opts Options, session Session, persist bool) (*Conversation, error) {
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
	if opts.Cols > math.MaxUint16 || opts.Rows > math.MaxUint16 {
		return nil, fmt.Errorf("%w: Cols and Rows must not exceed %d", ErrInvalidOptions, math.MaxUint16)
	}
	if opts.EventBuffer <= 0 {
		opts.EventBuffer = 32
	}

	adapter, err := resolveAdapter(opts.Harness)
	if err != nil {
		return nil, err
	}

	// Resolve resume args up front so an unsupported harness fails before launch.
	var resumeArgs []string
	if opts.Resume != "" {
		resumer, ok := adapter.(turns.SessionResumer)
		if !ok {
			return nil, fmt.Errorf("chat: harness %s cannot resume: %w", opts.Harness, ErrResumeUnsupported)
		}
		resumeArgs = resumer.ResumeArgs(opts.Resume)

		// Whenever chat injects a resume prefix the caller must NOT also pass raw
		// session-control flags in Options.Args — they would diverge the real
		// transcript from the persisted harness session id. Reject before launch.
		// Adapters that declare no reserved flags (e.g. codex) accept anything.
		if scf, ok := adapter.(turns.SessionControlFlags); ok {
			if bad := firstSessionControlConflict(opts.Args, scf.SessionControlFlags()); bad != "" {
				return nil, fmt.Errorf("%w: argument %s conflicts with chat-managed session control; use Options.Resume / Reopen", ErrInvalidOptions, bad)
			}
		}

		// Seed the session's harness id with the resume id so History and
		// session-id capture reflect the resumed session immediately. This composes
		// with the existing first-write-wins guards (maybeExtractSessionID /
		// captureRawSessionID both short-circuit on a non-empty id).
		session.HarnessSessionID = opts.Resume
	}

	scr := screen.New(opts.Cols, opts.Rows)

	// Build the Conversation BEFORE starting the wrapper so the durable line
	// tap (below) can target c.captureRawSessionID. Every field the tap reads
	// (mu, session, store, adapter) is initialized here; sess/releaseWriter are
	// filled in right after Start and are not touched by the tap.
	c := &Conversation{
		opts:         opts,
		store:        opts.Store,
		adapter:      adapter,
		screen:       scr,
		queue:        newControlQueue(),
		session:      session,
		eventCh:      make(chan ConversationEvent, opts.EventBuffer),
		inputStateCh: make(chan struct{}, 1),
		markerArmCh:  make(chan struct{}, 1),
		closed:       make(chan struct{}),
	}

	// Prepend the resume fragment AHEAD of the caller's args so the resume verb
	// leads the argv; empty for a fresh launch.
	launchArgs := opts.Args
	if len(resumeArgs) > 0 {
		launchArgs = append(append([]string{}, resumeArgs...), opts.Args...)
	}

	cfg := wrapper.Config{
		BinaryPath: opts.BinaryPath,
		Args:       launchArgs,
		WorkingDir: opts.WorkingDir,
		Env:        opts.Env,
		Stdin:      nil,
		Stdout:     scr,
		Harness:    opts.Harness,
		Effort:     opts.Effort,
		Model:      opts.Model,
	}
	// When the adapter can recover the harness's own session id from a raw
	// output line, tap the wrapper's durable, no-drop line stream to capture
	// it. Claude Code prints "claude --resume <uuid>" only to the normal screen
	// as the TUI tears down on exit, where it never reaches the rendered
	// snapshot a turns.SessionIDExtractor scrapes — so the raw line is the only
	// surface that carries it. Wired only when the capability is present, so
	// other harnesses pay no per-line tap cost.
	if _, ok := adapter.(turns.RawSessionIDExtractor); ok {
		cfg.OnLine = c.captureRawSessionID
	}

	sess, err := wrapper.Start(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("chat: wrapper start: %w", err)
	}
	c.sess = sess

	releaseWriter, ok := sess.AcquireWriter()
	if !ok {
		// Should be impossible immediately after Start; treat as fatal.
		_ = sess.Stop(context.Background())
		return nil, fmt.Errorf("chat: failed to acquire wrapper writer lock")
	}
	c.releaseWriter = releaseWriter

	// Match the PTY size to the virtual screen size so the harness's
	// re-renders target the same dimensions our emulator is tracking.
	if err := scr.ResizeWithPeer(opts.Cols, opts.Rows, func() error {
		return sess.Resize(uint16(opts.Cols), uint16(opts.Rows))
	}); err != nil {
		releaseWriter()
		_ = sess.Stop(context.Background())
		return nil, fmt.Errorf("chat: initial resize: %w", err)
	}

	// Persist the session record on the create path only. Reopen (persist=false)
	// skips this — the record already exists. Pass a copy: the PTY read loop is
	// already live, so the tap may touch c.session (under c.mu) concurrently —
	// the store must not alias it.
	if persist {
		sessionRec := c.session
		if err := opts.Store.CreateSession(ctx, &sessionRec); err != nil {
			releaseWriter()
			_ = sess.Stop(context.Background())
			return nil, fmt.Errorf("chat: store CreateSession: %w", err)
		}
	}

	c.watcher = turns.Watch(sess, scr, adapter)

	go c.consumeWatcher()
	go c.idleCompletionWatcher()

	return c, nil
}

// firstSessionControlConflict scans args (up to a bare "--" terminator) for the
// first token that conflicts with a chat-managed session-control flag: an exact
// token match (covering short flags and bare long flags), or, for a LONG flag,
// the attached "--flag=value" form. Returns the offending token, or "" when
// there is no conflict. Mirrors the TS firstSessionControlConflict.
func firstSessionControlConflict(args, banned []string) string {
	set := make(map[string]struct{}, len(banned))
	var longFlags []string
	for _, f := range banned {
		set[f] = struct{}{}
		if strings.HasPrefix(f, "--") {
			longFlags = append(longFlags, f)
		}
	}
	for _, tok := range args {
		if tok == "--" {
			break // positionals follow; never flags
		}
		if _, ok := set[tok]; ok {
			return tok
		}
		for _, f := range longFlags {
			if strings.HasPrefix(tok, f+"=") {
				return tok
			}
		}
	}
	return ""
}

// SessionID returns the chat-level session ID. Distinct from the
// underlying harness's session ID (Session.HarnessSessionID), which the
// adapter surfaces from the harness's own output when available.
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
		c.resizeMu.Lock()
		defer c.resizeMu.Unlock()

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

// Resize updates both the harness PTY and the private terminal emulator.
// Calls are serialized so concurrent resizes cannot leave the two at
// different final dimensions. Screen reads and writes are paused while the PTY
// is resized, then the emulator is updated before queued output can be
// interpreted. If the PTY resize fails, the screen remains untouched. Zero
// dimensions are ignored, matching wrapper.Session.Resize.
func (c *Conversation) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return nil
	}

	c.resizeMu.Lock()
	defer c.resizeMu.Unlock()

	select {
	case <-c.closed:
		return ErrClosed
	default:
	}

	return c.screen.ResizeWithPeer(int(cols), int(rows), func() error {
		return c.sess.Resize(cols, rows)
	})
}

// consumeWatcher pumps turns.Event from the watcher into Conversation
// state and emits ConversationEvent on c.eventCh.
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
// grab it. The idle-completion fallback (maybeIdleComplete) makes the
// same attempt, so a turn that completes there is not left without an id.
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
			// A "completed" turn that yielded no real reply on a logged-out /
			// not-onboarded screen is not a success — relabel it ReasonAuthRequired.
			c.authRelabel(turn, *ev.Snap)
		}
	case turns.Blocked:
		turn.State = TurnStateErrored
		turn.CompletedAt = ev.At
		turn.Reason = ev.Reason
		turn.HTTPCode = ev.HTTPCode
		turn.RetryAfter = ev.RetryAfter
	case turns.Errored:
		turn.State = TurnStateErrored
		turn.CompletedAt = ev.At
		// A terminal error whose screen shows a logged-out / re-auth banner is not
		// a task failure — the harness CLI is logged out. Prefer the canonical,
		// machine-matchable auth reason over the generic one (e.g. "harness
		// exited"). A status-derived Errored event carries no snapshot (the
		// wrapper-status watcher pump stamps none), so fall back to the live
		// screen, which still shows the banner after the harness exits.
		turn.Reason = ev.Reason
		screenText := ""
		if ev.Snap != nil {
			screenText = ev.Snap.Text
		} else if c.screen != nil {
			screenText = c.screen.Snapshot().Text
		}
		if authRequired(c.opts.Harness, screenText) {
			turn.Reason = ReasonAuthRequired
		}
		turn.HTTPCode = ev.HTTPCode
		turn.RetryAfter = ev.RetryAfter
	case turns.ToolCall:
		// ToolCall is informational mid-turn; fall through to the shared
		// restore-pointer path so the next event can complete the turn.
		fallthrough
	default:
		// ToolCall (mid-turn) or an unknown kind: leave turn as-is and restore
		// the current-turn pointer so the next event can complete it.
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
// the prompt. This is the default; a Conversation may shrink it via the
// unexported Options.idleGap (see idleGapDur) — the integration suite does this
// to keep PTY-driven tests fast.
const idleCompletionGap = 8 * time.Second

// markerConfirmGap is the (shorter) quiet window used once an end-of-turn marker
// has been seen for the in-flight turn. The marker is strong evidence the turn
// ended; we only need to confirm the screen then SETTLED at a non-busy prompt
// (rather than continuing into the next tool call), which distinguishes a genuine
// end-of-turn marker from an intermediate one. Must exceed Claude's working-frame
// cadence (its spinner repaints ~1×/s, resetting the timer) so it never elapses
// mid-work; 2s clears that with margin while keeping per-turn latency low.
// Default; per-Conversation override via Options.markerGap (see markerGapDur).
const markerConfirmGap = 2 * time.Second

// idleGapDur / markerGapDur return this Conversation's completion windows: the
// unexported Options override when set (tests), else the package default. opts
// is fixed at Open and never mutated, so these are safe to call from the
// idleCompletionWatcher goroutine without synchronization.
func (c *Conversation) idleGapDur() time.Duration {
	if c.opts.idleGap > 0 {
		return c.opts.idleGap
	}
	return idleCompletionGap
}

func (c *Conversation) markerGapDur() time.Duration {
	if c.opts.markerGap > 0 {
		return c.opts.markerGap
	}
	return markerConfirmGap
}

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
	timer := time.NewTimer(c.idleGapDur())
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
		gap := c.idleGapDur()
		if marker {
			gap = c.markerGapDur()
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
// event that fires first clears currentTurn and makes this a no-op. When it does
// complete, it recovers the harness session id (maybeExtractSessionID) just like
// the TurnComplete-event path, so completing here never loses the id.
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
	gap := c.idleGapDur()
	if marker {
		gap = c.markerGapDur()
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

	// The turn is now committed to completing here. Recover the harness's own
	// session id, exactly as the TurnComplete-event path does in handleTurnsEvent
	// — otherwise a turn that completes via THIS idle fallback never captures it.
	// That gap is real for Codex 0.142+: it renders neither the Token-usage footer
	// that fires a TurnComplete event nor the "resume <uuid>" hint, so its turns
	// land here with no id, and History silently degrades to the lossy screen
	// scrape instead of the on-disk transcript. Done before the completion event
	// is emitted so any consumer reacting to it sees the id already persisted.
	// No-op for adapters without a screen/disk extractor (e.g. claude-code, whose
	// id arrives via the raw line tap), so this never disturbs their path.
	c.maybeExtractSessionID()

	turn.State = TurnStateComplete
	turn.CompletedAt = time.Now()
	if marker {
		// The marker path is only reached for adapters that emit an end-of-turn
		// marker (claude-code today).
		turn.Reason = c.opts.Harness + ": end-of-turn marker confirmed at a settled prompt"
	} else {
		// The fallback path is harness-agnostic (pi, opencode, …); name the harness
		// that actually completed rather than hardcoding claude-code.
		turn.Reason = c.opts.Harness + ": idle-completion fallback (end-of-turn marker not observed)"
	}
	// Use the adapter's message extractor (when available) so Turn.Text is the
	// clean assistant reply rather than a full-screen dump — matching the
	// marker-event completion path in onTurnEvent.
	turn.Text = c.assistantText(snap)
	// The claude-code false-success lands HERE: a logged-out turn ends on a
	// "✻ … for 0s" marker and would otherwise complete with the raw banner screen
	// as its reply. Relabel it ReasonAuthRequired when no real reply was extracted.
	c.authRelabel(turn, snap)
	if err := c.store.UpdateTurn(context.Background(), turn); err != nil {
		c.emit(ConversationEvent{Type: EventTurn, Turn: *turn, Err: err})
		return
	}
	c.emit(ConversationEvent{Type: EventTurn, Turn: *turn})
}

// maybeExtractSessionID opportunistically recovers the harness's own session
// ID, preferring a cheap screen scrape (turns.SessionIDExtractor) and falling
// back to an on-disk lookup keyed on the working directory
// (turns.SessionIDLocator). The disk fallback exists because some harnesses
// (Codex 0.142+) stopped printing the "resume <uuid>" hint to the screen, so
// the scrape returns nothing and the only remaining anchor is the persisted
// session log. Once we've persisted an ID we don't probe again. No-op for
// adapters that implement neither capability.
func (c *Conversation) maybeExtractSessionID() {
	c.mu.Lock()
	if c.session.HarnessSessionID != "" {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	id, ok := c.extractSessionID()
	if !ok {
		return
	}

	c.mu.Lock()
	c.session.HarnessSessionID = id
	updated := c.session
	c.mu.Unlock()
	_ = c.store.UpdateSession(context.Background(), &updated)
}

// extractSessionID tries the screen scrape first, then the on-disk locator.
// Returns ("", false) when neither yields an ID.
func (c *Conversation) extractSessionID() (string, bool) {
	if ext, ok := c.adapter.(turns.SessionIDExtractor); ok {
		if id, ok := ext.ExtractSessionID(c.screen.Snapshot()); ok {
			return id, true
		}
	}
	if loc, ok := c.adapter.(turns.SessionIDLocator); ok {
		if id, ok := loc.LocateSessionID(c.opts.WorkingDir); ok {
			return id, true
		}
	}
	return "", false
}

// captureRawSessionID is the wrapper's durable line-tap callback (wired in Open
// only when the adapter implements turns.RawSessionIDExtractor). It runs
// synchronously in the PTY read goroutine — one call per raw output line, in
// order — and records the harness's own session ID the moment it appears in the
// stream (e.g. Claude Code's "claude --resume <uuid>" exit hint). Once captured
// we stop probing. Kept cheap so it does not back-pressure the read loop:
// a string compare short-circuits after capture, and the regex only runs while
// the ID is still unknown.
func (c *Conversation) captureRawSessionID(line string) {
	c.mu.Lock()
	already := c.session.HarnessSessionID != ""
	c.mu.Unlock()
	if already {
		return
	}

	ext, ok := c.adapter.(turns.RawSessionIDExtractor)
	if !ok {
		return
	}
	id, ok := ext.ExtractSessionIDFromLine(line)
	if !ok {
		return
	}

	c.mu.Lock()
	if c.session.HarnessSessionID != "" {
		c.mu.Unlock()
		return
	}
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

// cleanAssistantText is the adapter's extracted assistant reply with NO
// whole-screen fallback: "" when the adapter has no extractor or finds no reply
// (unlike assistantText, which returns the whole screen in that case). It is the
// "did this turn actually produce a reply?" signal used by authRelabel.
func (c *Conversation) cleanAssistantText(snap screen.Snapshot) string {
	if ex, ok := c.adapter.(turns.MessageExtractor); ok {
		if msg, ok := ex.ExtractMessage(snap); ok {
			return strings.TrimSpace(msg)
		}
	}
	return ""
}

// authRelabel converts a turn that "completed" but produced NO real assistant
// reply, on a settled screen showing a logged-out / not-onboarded banner, into
// the canonical ReasonAuthRequired failure. Without it a logged-out claude-code
// turn — which ends on a "✻ … for 0s" end-of-turn thinking marker, not an error
// — is persisted as a SUCCESS with the raw banner screen as its "reply" (the
// false-success bug). Gated on an EMPTY clean extraction, so a genuine reply
// (which produces a "⏺" bullet) is never touched even if it mentions "/login".
// Returns true if it relabeled the turn.
func (c *Conversation) authRelabel(turn *Turn, snap screen.Snapshot) bool {
	if c.cleanAssistantText(snap) != "" {
		return false
	}
	if !authRequired(c.opts.Harness, snap.Text) {
		return false
	}
	turn.State = TurnStateErrored
	turn.Reason = ReasonAuthRequired
	turn.Text = ""
	return true
}

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
	// captured. Used when the adapter can't read transcripts or the harness
	// session id was never captured.
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
	if !hasReader || sessionCopy.HarnessSessionID == "" {
		out, err := c.store.ListTurns(ctx, sessionCopy.ID)
		return out, HistorySourceStore, err
	}

	tturns, err := reader.ReadTranscript(sessionCopy.HarnessSessionID, c.opts.WorkingDir)
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
