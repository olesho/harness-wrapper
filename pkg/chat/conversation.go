package chat

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olesho/harness-wrapper/internal/resettime"
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

	// HarnessSessionID is the harness's own id for the FRESH session Open
	// starts. When the adapter can take one (turns.SessionAssigner: claude-code
	// and pi, launched with --session-id <uuid>), every fresh Open assigns an id
	// at launch — this one, or a minted UUID when it is empty — and seeds
	// Session.HarnessSessionID with it before the harness starts, so the
	// transcript verdicts and History work from the first turn. Set it when the
	// id must be known before launch, e.g. to record it first: it names the
	// session's transcript.
	//
	// Open refuses it, with ErrInvalidOptions, when the adapter cannot take an
	// id (codex, opencode, generic), when it is not in the harness's form (a
	// UUID), when Resume is also set, and when the harness already has a
	// transcript for it (also ErrHarnessSessionInUse): resume that session
	// instead. An assigned id makes session control chat's, so Args may not
	// carry any flag the adapter reserves via turns.SessionControlFlags
	// (--session-id, --resume, --continue, …), exactly as on a resume.
	HarnessSessionID string

	// WorkingDir is the harness's working directory. Defaults to the
	// current process's CWD.
	WorkingDir string

	// Env is the harness's environment. Defaults to the current
	// process's environment.
	Env []string

	// Effort, Model and PermissionMode are execution-mode knobs forwarded to
	// wrapper.Config; see harness-wrapper wrapper.Config. Empty leaves the
	// harness default.
	Effort string
	Model  string

	// PermissionMode is the launch-time permission rung forwarded to
	// wrapper.Config, which translates it into the harness's native flag
	// (claude --permission-mode, codex -s/-a). The canonical rungs are "plan",
	// "manual", "ask", "auto" and "bypass"; per-harness native spellings are
	// also accepted. Empty leaves the harness default. All value validation
	// lives in wrapper.Config — this layer only carries the string.
	//
	// Restrictive rungs (`plan`, `manual`, `ask`) are fully enforced only when
	// a human is at the TUI (passthrough, or `run` from a terminal for codex).
	// Under `structured-run` and unattended `run`, claude's permission dialogs
	// are not detected (the turn stalls to the deadline) and codex's approval
	// prompts are auto-approved (only the `-s` sandbox axis still binds).
	PermissionMode string

	// Containment requests Landlock containment for the harness (Linux; see
	// wrapper.Config.Containment). nil — the default — opens an uncontained
	// conversation exactly as before.
	//
	// A contained conversation is selected at creation and stays contained:
	// its containment record — the normalized policy, profile and private
	// state — is persisted to Store BEFORE the harness starts (the Store must
	// implement ContainmentStore), the conversation needs cgroup supervision,
	// and Reopen inherits the record. Resume it through Reopen; read its
	// harness session id with Session.HarnessID.
	Containment *wrapper.Containment

	// KeepAliveOnClassification keeps the harness running through every
	// classification of its output (wrapper.Config.KeepAliveOnClassification,
	// ADR-006). Set it for a conversation whose lifetime the caller owns: one
	// kept open between messages, where silence is the resting state. Without
	// it the wrapper ends the harness when a quiet stretch follows output that
	// merely mentions a rate limit or a retry, and on a real usage-limit wall.
	// The zero value keeps that run-to-completion supervision, which
	// harness.RunTurn, oneshot, structured-run and the run CLI rely on.
	KeepAliveOnClassification bool

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

	// wrapperQuiet, wrapperClassify optionally override the wrapper's idle
	// thresholds (wrapper.Config.IdleQuiet / IdleClassify), so a same-package
	// test can reach the wrapper's idle classification in a fraction of a
	// second. Unexported for the reason idleGap is; zero keeps the defaults.
	wrapperQuiet, wrapperClassify time.Duration

	// permModeRenderTimeout optionally overrides the per-press repaint budget
	// SetPermissionMode waits on (defaultPermissionModeRenderTimeout). Same
	// rationale as idleGap/markerGap and unexported for the same reason: only
	// same-package tests set it, so a deliberately-stuck fake exhausts the press
	// bound in milliseconds instead of tens of seconds. Zero means "use the
	// package default".
	permModeRenderTimeout time.Duration
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

	// sentScreenText is the rendered screen as it looked when the current
	// prompt was submitted. A swallow detector compares against it to answer
	// "nothing changed at all" — see swallowed.go.
	sentScreenText string

	// sentTranscriptWatermark is how far the harness's own transcript already
	// extended at the moment the in-flight prompt was submitted, or -1 when
	// that could not be established. Only entries at or beyond it can speak for
	// THIS turn.
	//
	// It is the same rule loom applies one layer down with
	// LogFileStartOffset, and for the same reason: these files are append-only
	// and survive resume, so a previous turn's verdict sitting in one would
	// otherwise condemn every later turn on the same session. Without a
	// watermark there is no verdict — never a guessed lower bound. Ported from
	// meta-harness's sentTranscriptWatermark, which keeps the same field for
	// the same purpose. Reset on each Send.
	sentTranscriptWatermark int
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

	// inputUnresolved is set when an auto-answer was written but the dialog
	// never acted on it, after the bounded retries in answerAndConfirm. It is
	// what turns a permanent stall into a fast, diagnosable failure: a prompt
	// nothing can clear can never reach the ready state, so waitReadyForSend
	// returns this instead of blocking to the caller's run deadline. Cleared
	// when a new request arrives and when one resolves.
	inputUnresolved *InputUnresolvedError

	// lastBusyAt is when the screen last showed the harness working
	// (turns.BusyDetector), in Unix nanoseconds; zero when it never has. Send
	// types only after the harness has been idle for the confirmation window.
	lastBusyAt atomic.Int64

	// heldReason is set while the in-flight turn is HELD: a keep-alive
	// conversation reported a Blocked for it — an API error, a usage wall —
	// that the harness may yet retry past. It is the Blocked reason, which the
	// turn ends with if the harness's own record cannot settle it. See
	// holdsTurns.
	heldReason string

	// harnessDir is the directory the harness runs in: Options.WorkingDir, or
	// this process's working directory when that is empty, which the harness
	// inherits. Every read of the harness's transcript keys on it — the harness
	// files its transcript under its working directory — so an empty WorkingDir
	// does not turn those reads off. Set once at Open.
	harnessDir string

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
	BinaryPath                string
	Args                      []string
	Env                       []string
	Effort                    string
	Model                     string
	PermissionMode            string
	KeepAliveOnClassification bool
	Cols, Rows                int
	Store                     Store
	EventBuffer               int
	InputPolicy               *InputPolicy
	DisableCodexAutoDismiss   bool
	OnInputRequest            func(InputRequest) (InputAnswer, bool)

	// Containment, for a contained session, may restate its policy: nil
	// inherits the stored record, and an explicit request must normalize to
	// the same policy. For an uncontained session a non-nil value is refused:
	// containment is chosen when a conversation is created, never added later.
	Containment *wrapper.Containment

	// idleGap, markerGap, wrapperQuiet and wrapperClassify mirror the
	// unexported Options test knobs; only same-package tests set them.
	idleGap, markerGap            time.Duration
	wrapperQuiet, wrapperClassify time.Duration
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
	// No in-place conversion: containment is chosen when a conversation is
	// created. Refused before any prompt reaches a harness or any process
	// starts; the stored record is not touched.
	if rec.Containment == nil && opts.Containment != nil {
		return nil, fmt.Errorf("%w: session %s is uncontained, and containment cannot be added to an existing conversation; open a new contained conversation", ErrInvalidOptions, opts.SessionID)
	}
	if rec.Containment != nil {
		if err := validContainmentRecord(rec); err != nil {
			return nil, err
		}
	}
	if rec.HarnessID() == "" {
		return nil, fmt.Errorf("chat: session %s has no harness session id: %w", opts.SessionID, ErrNoHarnessSession)
	}

	launch := Options{
		Harness:                   rec.Harness,
		BinaryPath:                opts.BinaryPath,
		Args:                      opts.Args,
		WorkingDir:                rec.WorkingDir,
		Env:                       opts.Env,
		Resume:                    rec.HarnessID(),
		Effort:                    opts.Effort,
		Model:                     opts.Model,
		PermissionMode:            opts.PermissionMode,
		KeepAliveOnClassification: opts.KeepAliveOnClassification,
		Containment:               opts.Containment,
		Cols:                      opts.Cols,
		Rows:                      opts.Rows,
		Store:                     opts.Store,
		EventBuffer:               opts.EventBuffer,
		InputPolicy:               opts.InputPolicy,
		DisableCodexAutoDismiss:   opts.DisableCodexAutoDismiss,
		OnInputRequest:            opts.OnInputRequest,
		idleGap:                   opts.idleGap,
		markerGap:                 opts.markerGap,
		wrapperQuiet:              opts.wrapperQuiet,
		wrapperClassify:           opts.wrapperClassify,
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
	harnessDir := opts.WorkingDir
	if harnessDir == "" {
		if wd, err := os.Getwd(); err == nil {
			harnessDir = wd
		}
	}

	// A contained conversation — requested now, or recorded — is prepared
	// before anything else: its record validated or created, and private
	// state allocated. Its adapter reads the private layout, configured once
	// the launch exists (below); never the caller's environment.
	contained := opts.Containment != nil || session.Containment != nil
	var cl *containedLaunch
	if contained {
		if cl, err = prepareContainment(ctx, opts, &session, persist); err != nil {
			return nil, err
		}
		opts.Containment = session.Containment.Required.Clone()
		// Until the launch is running, every failure gives back what
		// prepareContainment took: state allocated for a new conversation is
		// removed (and a record already persisted is marked so), state of a
		// reopened one is left intact.
		defer func() {
			if !cl.launched {
				cl.abandon(context.Background(), opts.Store, session)
			}
		}()
	} else {
		configureAdapterEnv(adapter, opts.Env)
	}

	// Resolve the session args up front — a resume, or the id assigned to a
	// fresh session — so an unsupported request fails before launch.
	sessionArgs, harnessID, err := sessionLaunch(adapter, opts, harnessDir, contained)
	if err != nil {
		return nil, err
	}
	if len(sessionArgs) > 0 {
		// Whenever chat injects a session prefix the caller must NOT also pass raw
		// session-control flags in Options.Args — they would diverge the real
		// transcript from the persisted harness session id. Reject before launch.
		// Adapters that declare no reserved flags (e.g. codex) accept anything.
		if scf, ok := adapter.(turns.SessionControlFlags); ok {
			if bad := firstSessionControlConflict(opts.Args, scf.SessionControlFlags()); bad != "" {
				return nil, fmt.Errorf("%w: argument %s conflicts with chat-managed session control; use Options.HarnessSessionID, Options.Resume or Reopen", ErrInvalidOptions, bad)
			}
		}

		// Seed the session's harness id before launch, so History and the
		// transcript verdicts read the right session from the start. This composes
		// with the first-write-wins guards (maybeExtractSessionID /
		// captureRawSessionID both short-circuit on a non-empty id), so nothing the
		// harness prints later replaces it. A contained session keeps it in its
		// record, never in the legacy field.
		session = session.withHarnessID(harnessID)
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
		harnessDir:   harnessDir,
		eventCh:      make(chan ConversationEvent, opts.EventBuffer),
		inputStateCh: make(chan struct{}, 1),
		markerArmCh:  make(chan struct{}, 1),
		closed:       make(chan struct{}),
		// No prompt has been sent, so nothing in a transcript can speak for a
		// turn of ours yet. Send replaces this; watermarkUnknown until it does.
		sentTranscriptWatermark: watermarkUnknown,
	}

	// Prepend the session fragment AHEAD of the caller's args so the resume verb
	// or the assigned id leads the argv; empty when the adapter takes neither.
	launchArgs := opts.Args
	if len(sessionArgs) > 0 {
		launchArgs = append(append([]string{}, sessionArgs...), opts.Args...)
	}

	cfg := wrapper.Config{
		BinaryPath:     opts.BinaryPath,
		Args:           launchArgs,
		WorkingDir:     opts.WorkingDir,
		Env:            opts.Env,
		Stdin:          nil,
		Stdout:         scr,
		Harness:        opts.Harness,
		Effort:         opts.Effort,
		Model:          opts.Model,
		PermissionMode: opts.PermissionMode,
		Containment:    opts.Containment,
		IdleQuiet:      opts.wrapperQuiet,
		IdleClassify:   opts.wrapperClassify,

		KeepAliveOnClassification: opts.KeepAliveOnClassification,
	}
	// When the adapter can recover the harness's own session id from a raw
	// output line and the id is not already known, tap the wrapper's durable,
	// no-drop line stream to capture it. Claude Code prints "claude --resume
	// <uuid>" only to the normal screen as the TUI tears down on exit, where it
	// never reaches the rendered snapshot a turns.SessionIDExtractor scrapes — so
	// the raw line is the only surface that carries it. A resumed or assigned id
	// is known from launch and nothing may replace it, so the tap is not wired
	// then; nor for adapters without the capability, which pay no per-line cost.
	if _, ok := adapter.(turns.RawSessionIDExtractor); ok && session.HarnessID() == "" {
		cfg.OnLine = c.captureRawSessionID
	}

	// A contained conversation's record is persisted BEFORE its first launch:
	// failure to persist prevents the launch. (Uncontained conversations keep
	// the old order, below.)
	startCtx := ctx
	if contained {
		if persist {
			sessionRec := session.clone()
			if err := opts.Store.CreateSession(ctx, &sessionRec); err != nil {
				return nil, fmt.Errorf("chat: store CreateSession: %w", err)
			}
		}
		startCtx = cl.ctx
	}

	sess, err := wrapper.Start(startCtx, cfg)
	if err != nil {
		// An invalid wrapper.Config reaching Start from here means a caller-supplied
		// option (in practice Effort — see the reachability note in Options) failed
		// validation, so surface it as ErrInvalidOptions and let transports map it to
		// a 4xx. The multi-%w keeps wrapper.ErrInvalidConfig matchable for consumers
		// that discriminate on it, and both arms carry the same breadcrumb.
		if errors.Is(err, wrapper.ErrInvalidConfig) {
			return nil, fmt.Errorf("%w: chat: wrapper start: %w", ErrInvalidOptions, err)
		}
		return nil, fmt.Errorf("chat: wrapper start: %w", err)
	}
	c.sess = sess
	if contained {
		if err := c.recordLaunch(ctx, cl); err != nil {
			_ = sess.Stop(context.Background())
			return nil, err
		}
	}

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
	// skips this — the record already exists. Pass a copy taken under c.mu: the
	// PTY read loop is already live, so the tap may write c.session (under c.mu)
	// concurrently — the read must be synchronized and the store must not alias it.
	if persist && !contained {
		c.mu.Lock()
		sessionRec := c.session
		c.mu.Unlock()
		if err := opts.Store.CreateSession(ctx, &sessionRec); err != nil {
			releaseWriter()
			_ = sess.Stop(context.Background())
			return nil, fmt.Errorf("chat: store CreateSession: %w", err)
		}
	}

	c.watcher = turns.Watch(sess, scr, adapter)

	go c.consumeWatcher()
	go c.idleCompletionWatcher()

	if contained {
		cl.launched = true
		cl.keepUntilEnd(sess)
	}
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

// sessionLaunch returns the argv fragment chat prepends to identify the
// harness session, and the harness session id it names: the resume fragment
// for Options.Resume; for a fresh Open with a turns.SessionAssigner, the
// assigned id's fragment — Options.HarnessSessionID, or a minted id — and
// nil otherwise. Every refusal is decided here, before anything launches.
func sessionLaunch(adapter turns.Adapter, opts Options, harnessDir string, contained bool) ([]string, string, error) {
	if opts.Resume != "" {
		if opts.HarnessSessionID != "" {
			return nil, "", fmt.Errorf("%w: HarnessSessionID names a fresh session and Resume resumes one; set one of them", ErrInvalidOptions)
		}
		resumer, ok := adapter.(turns.SessionResumer)
		if !ok {
			return nil, "", fmt.Errorf("chat: harness %s cannot resume: %w", opts.Harness, ErrResumeUnsupported)
		}
		return resumer.ResumeArgs(opts.Resume), opts.Resume, nil
	}
	assigner, ok := adapter.(turns.SessionAssigner)
	if !ok {
		if opts.HarnessSessionID != "" {
			return nil, "", fmt.Errorf("%w: harness %s cannot start a session under an assigned id", ErrInvalidOptions, opts.Harness)
		}
		return nil, "", nil
	}
	id := opts.HarnessSessionID
	if id == "" {
		// A minted id cannot collide, so it needs no in-use check.
		id = assigner.NewSessionID()
		return assigner.SessionIDArgs(id), id, nil
	}
	if err := assigner.ValidSessionID(id); err != nil {
		return nil, "", fmt.Errorf("%w: HarnessSessionID: %w", ErrInvalidOptions, err)
	}
	// A contained Open starts in private state allocated for it, whose root the
	// adapter learns only once the launch exists; a caller-managed StateDir that
	// already holds the session is refused by claude itself.
	if !contained {
		if err := harnessSessionInUse(adapter, id, harnessDir); err != nil {
			return nil, "", err
		}
	}
	return assigner.SessionIDArgs(id), id, nil
}

// harnessSessionInUse refuses id when the adapter can already read a transcript
// for it: the session exists, so launching a fresh one under the same id would
// be refused by the harness (claude: "Session ID … is already in use") or would
// silently continue it (pi reuses an existing session). A read that fails for
// any reason but a missing file cannot establish that the id is free, so it
// refuses too. Adapters without a transcript reader cannot tell, and pass.
func harnessSessionInUse(adapter turns.Adapter, id, workingDir string) error {
	reader, ok := adapter.(turns.TranscriptReader)
	if !ok {
		return nil
	}
	_, err := reader.ReadTranscript(id, workingDir)
	switch {
	case err == nil:
		return fmt.Errorf("%w: %w: harness session %s already has a transcript; resume it instead", ErrInvalidOptions, ErrHarnessSessionInUse, id)
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("%w: %w: cannot establish that harness session %s is unused: %w", ErrInvalidOptions, ErrHarnessSessionInUse, id, err)
	}
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
			// A turn whose "reply" is in fact the usage-limit wall is not a success
			// either; that one is a NON-empty extraction, so it must be checked first
			// (authRelabel's empty-gate would let it through). And before BOTH, the
			// harness's own recorded verdict, which is a statement rather than a
			// reading of the screen — see apierror.go.
			c.relabelTerminal(turn, *ev.Snap)
		}
	case turns.Blocked:
		turn.HTTPCode = ev.HTTPCode
		turn.RetryAfter = ev.RetryAfter
		if c.holdsTurns() {
			// Held (ADR-006): a keep-alive harness may yet retry past what the
			// output showed, so the turn stays pending with the Blocked recorded
			// on it, and ends when the harness ends it — the harness's own record
			// deciding the outcome then.
			c.mu.Lock()
			c.heldReason = ev.Reason
			c.currentTurn = turn
			c.mu.Unlock()
			return
		}
		turn.State = TurnStateErrored
		turn.CompletedAt = ev.At
		turn.Reason = ev.Reason
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
		// The harness's own tag outranks the screen for the same reason it does
		// at the completion sites: it is a verdict the harness recorded about
		// its own API call, not a reading of rendered pixels. It also names the
		// two failures no banner regex can — a billing wall, and an org policy
		// refusal — which would otherwise arrive here as "harness exited".
		if !c.apiErrorRelabel(turn) && authRequired(c.opts.Harness, screenText) {
			turn.Reason = ReasonAuthRequired
			turn.Code = CodeAuthRequired
		}
		// A held turn keeps the code and hint its Blocked recorded when the
		// ending event carries none.
		if ev.HTTPCode != 0 || ev.RetryAfter != 0 {
			turn.HTTPCode = ev.HTTPCode
			turn.RetryAfter = ev.RetryAfter
		}
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
			c.observeBusy(c.screen.Snapshot())
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

// observeBusy records when the screen shows the harness working, for Send's
// busy gate. A no-op for an adapter that cannot tell (no turns.BusyDetector).
func (c *Conversation) observeBusy(snap screen.Snapshot) bool {
	bd, ok := c.adapter.(turns.BusyDetector)
	if !ok || !bd.Busy(snap) {
		return false
	}
	c.lastBusyAt.Store(time.Now().UnixNano())
	return true
}

// busyQuietRemaining is how much longer the harness must stay idle before Send
// may type: the confirmation window (markerGapDur) less the time since the
// screen last showed it working. Zero when it has been idle long enough, or
// has never been seen working.
func (c *Conversation) busyQuietRemaining() time.Duration {
	last := c.lastBusyAt.Load()
	if last == 0 {
		return 0
	}
	return max(0, c.markerGapDur()-time.Since(time.Unix(0, last)))
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

	// A settled screen the adapter says was never accepted is not a completed
	// turn — it is a prompt the harness swallowed. Only the non-marker path can
	// be swallowed: an end-of-turn marker is itself evidence the harness ran,
	// and so is a Blocked the turn is held on — the harness made the API call
	// that failed.
	c.mu.Lock()
	held := c.heldReason != ""
	c.mu.Unlock()
	if !marker && !held && c.promptWasSwallowed(snap) {
		c.applySwallowedPromptVerdict(turn, snap)
		return
	}

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
	// as its reply. Relabel it ReasonAuthRequired when no real reply was extracted —
	// or ReasonUsageLimited when the "reply" is a usage-limit wall, which (being a
	// non-empty extraction) would otherwise slip past authRelabel's empty-gate —
	// or, ahead of both, whatever the harness itself recorded about the turn.
	c.relabelTerminal(turn, snap)
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
	if c.session.HarnessID() != "" {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	id, ok := c.extractSessionID()
	if !ok {
		return
	}

	c.mu.Lock()
	c.session.setHarnessID(id)
	updated := c.session.clone()
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
	already := c.session.HarnessID() != ""
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
	if c.session.HarnessID() != "" {
		c.mu.Unlock()
		return
	}
	c.session.setHarnessID(id)
	updated := c.session.clone()
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
// When transcript reading isn't possible (adapter has no reader, the
// harness session ID is not known, or the harness has not written its
// transcript yet), History falls back to the Store's recorded turns. The fallback only contains the
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
	turn.Code = CodeAuthRequired
	turn.Text = ""
	return true
}

// relabelTerminal applies the relabels in strength order to a turn that
// reached a terminal point looking like a success, and reports whether any of
// them took it.
//
// The order is the whole point:
//
//  1. The harness's last word — what the HARNESS recorded about its own API
//     call. A categorical statement, correlated to this turn by the pre-send
//     watermark, and the only one that can name a billing wall.
//  2. usageLimitRelabel — the quota wall, which claude paints as an assistant
//     bubble. Deliberately NOT gated on an empty extraction, because the wall
//     IS the extraction, which is why it must precede the auth check.
//  3. authRelabel — a logged-out / onboarding screen with no real reply.
//  4. heldRelabel — a held turn the harness's record did not settle.
//
// Each declines cleanly when it has nothing to say, so a turn that really did
// complete passes through all of them untouched.
func (c *Conversation) relabelTerminal(turn *Turn, snap screen.Snapshot) bool {
	word := c.lastWordOfCurrentTurn()
	return word.apply(turn, c.opts.Harness) ||
		c.usageLimitRelabel(turn, snap) ||
		c.authRelabel(turn, snap) ||
		c.heldRelabel(turn, word)
}

// holdsTurns reports whether a Blocked leaves the in-flight turn pending rather
// than ending it: in a keep-alive conversation (ADR-006) the harness ends its
// turns, and one whose adapter reads transcripts can have the outcome decided
// by the harness's own record when it does. An adapter without a transcript
// reader, and every default-mode conversation — whose run-to-completion callers
// rely on a Blocked ending the turn — keep that behaviour.
func (c *Conversation) holdsTurns() bool {
	if !c.opts.KeepAliveOnClassification {
		return false
	}
	_, reads := c.adapter.(turns.TranscriptReader)
	return reads
}

// heldRelabel ends a held turn errored with the Blocked it was held on, when
// the harness's own record did not settle it: its transcript could not be
// read, or holds no word on this turn. A success nobody can confirm is a wrong
// verdict (principle 2). A turn whose transcript shows a reply after the error
// recovered, and is left complete.
func (c *Conversation) heldRelabel(turn *Turn, word transcriptWord) bool {
	c.mu.Lock()
	reason := c.heldReason
	c.mu.Unlock()
	if reason == "" || word.replied {
		return false
	}
	turn.State = TurnStateErrored
	turn.Reason = reason
	turn.Text = ""
	return true
}

// usageLimitRelabel converts a turn that "completed" while the harness was out of
// subscription quota into the canonical ReasonUsageLimited failure. claude-code
// paints its usage/session-limit wall as an assistant bubble, so the turn ends on
// a normal end-of-turn marker and the wall itself is what ExtractMessage returns —
// persisting it would be a false SUCCESS whose "reply" is the wall, which a
// downstream validator then rejects as a bogus answer, retrying until a TRANSIENT
// quota outage blocks the task.
//
// Unlike authRelabel this is deliberately NOT gated on an empty extraction — the
// wall IS the extraction — so it must run FIRST at each completion site (an
// empty-gated authRelabel would decline anyway, but the ordering is what makes the
// intent explicit). The probe prefers the clean extracted reply and falls back to
// the whole screen, so it fires whether the CLI renders the wall as a "⏺" bubble
// (captured) or only as a "⎿" decoration (not captured, still on screen).
// Returns true if it relabeled the turn.
func (c *Conversation) usageLimitRelabel(turn *Turn, snap screen.Snapshot) bool {
	probe := c.cleanAssistantText(snap)
	if strings.TrimSpace(probe) == "" {
		probe = snap.Text
	}
	msg, ok := usageLimitMessage(c.opts.Harness, probe)
	if !ok {
		return false
	}
	turn.State = TurnStateErrored
	turn.Reason = ReasonUsageLimited + " (" + msg + ")"
	turn.Code = CodeUsageLimited
	turn.ResumeAt = resumeAtFrom(msg)
	turn.Text = ""
	return true
}

// resumeAtFrom is the reset time a usage wall names, zero when it names none —
// read with the parser the wrapper's session-limit matcher uses, so a wall
// reports one reset time whichever layer saw it.
func resumeAtFrom(wall string) time.Time {
	at, _ := resettime.Parse(wall, time.Now())
	return at
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

// configureAdapterEnv hands the harness's LAUNCH environment to an adapter whose
// on-disk lookups depend on it (Claude Code's CLAUDE_CONFIG_DIR, Codex's
// CODEX_HOME). Without it a profiled agent — one launched with its own config
// root — reads the OPERATOR's transcripts or none at all, and History silently
// degrades to the screen-scraped store fallback.
//
// opts.Env is the authority, not os.Environ(): one process can drive several
// conversations with different config roots, so the wrapper process's own
// environment is not a sound proxy. nil falls back to os.Environ(), matching
// the documented Options.Env default (an exec with a nil Env inherits).
//
// Called from openWithSession on a FRESH adapter, before the watcher goroutine
// exists, so the write happens-before every read of the configured field.
func configureAdapterEnv(adapter turns.Adapter, env []string) {
	ec, ok := adapter.(turns.EnvConfigurable)
	if !ok {
		return
	}
	if env == nil {
		env = os.Environ()
	}
	ec.ConfigureFromEnv(env)
}

// ErrInputUnresolved is the sentinel behind *InputUnresolvedError. An
// auto-answer was written to a blocking prompt and the prompt did not act on
// it — the harness is wedged on a dialog no keystroke this layer knows how to
// send will clear.
//
// It exists so a caller can classify the failure without depending on the
// concrete type:
//
//	if errors.Is(err, chat.ErrInputUnresolved) { ... }
//
// and recover the evidence with errors.As when it wants the screen.
var ErrInputUnresolved = errors.New("chat: interactive prompt did not accept its answer")

// InputUnresolvedError is the concrete error behind ErrInputUnresolved. Like
// PermissionModeBlockedError it carries the client-facing chat.InputRequest
// (the value PendingInput returns and Answer accepts) plus the screen as it
// looked when the driver gave up, so a failed run is diagnosable from the error
// alone rather than from a 43-minute silence.
type InputUnresolvedError struct {
	// Request is the prompt that would not take its answer.
	Request InputRequest
	// Observed is the rendered screen at the last attempt.
	Observed string
	// Attempts is how many answers were written before giving up.
	Attempts int
}

func (e *InputUnresolvedError) Error() string {
	return fmt.Sprintf("%s: %s (kind %q, id %q, %d attempts)",
		ErrInputUnresolved.Error(), e.Request.Prompt, e.Request.Kind, e.Request.ID, e.Attempts)
}

// Unwrap makes errors.Is(err, ErrInputUnresolved) match.
func (e *InputUnresolvedError) Unwrap() error { return ErrInputUnresolved }

// recordUnresolvedInput latches a stall so waitReadyForSend can fail fast on
// it. Only the typed error latches: a cancelled context or a closed
// conversation is the caller's own doing, not a wedged dialog, and latching it
// would poison a conversation that is otherwise fine.
func (c *Conversation) recordUnresolvedInput(err error) {
	var ue *InputUnresolvedError
	if !errors.As(err, &ue) {
		return
	}
	c.mu.Lock()
	c.inputUnresolved = ue
	c.mu.Unlock()
}

// inputBlocked reports the error that makes this conversation unsendable
// because of an interactive prompt, or nil. The unresolved stall is checked
// FIRST: it is strictly more informative than ErrInputPending (it carries the
// screen and the attempt count) and both are true at once, because a stall
// surfaces the request to the client on its way out.
func (c *Conversation) inputBlocked() error {
	c.mu.Lock()
	unresolved := c.inputUnresolved
	awaiting := c.currentInput != nil && c.inputSurfaced
	c.mu.Unlock()
	if unresolved != nil {
		return unresolved
	}
	if awaiting {
		return ErrInputPending
	}
	return nil
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
