package wrapper

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

// Snapshot is the most recent state observation for a Session. Snapshot
// is safe to read concurrently with the session running; it always
// reflects a coherent point-in-time view.
type Snapshot struct {
	// Status is the wrapper's current classification. Mid-run, it may
	// be empty (the session is producing output and has not been
	// classified) or one of the actionable mid-run statuses
	// (waiting_for_input). After Wait returns, Status is the terminal
	// status from Result.
	Status Status

	// Reason mirrors the Reason field on the most recent classification.
	Reason string

	// LastOutputAt is the time of the most recent byte received from
	// the harness PTY. Zero if no output has been observed yet.
	LastOutputAt time.Time
}

// SessionEvent is a state transition observed by a Session. Events are
// delivered on Session.Events() in order. Mid-run classifications
// (waiting_for_input, blocked_by_cost, retry_later, api_error) flow as
// Status events. The final event is always Terminated, after which the
// channel is closed.
type SessionEvent struct {
	At         time.Time
	Status     Status
	Reason     string
	Terminated bool

	// Class is the canonical harness-output error taxonomy for this event
	// (ErrNone for non-error transitions). On the terminal event it equals
	// Result.Class. Lets mid-run watchers observe the class per event.
	Class ErrorClass

	// HTTPCode is the upstream API status code when Status is
	// StatusAPIError and the harness surfaced one. Zero otherwise.
	HTTPCode int

	// RetryAfter is the wait duration the harness suggested in its
	// error message. Zero when no hint was parseable.
	RetryAfter time.Duration

	// ResumeAt is the absolute wall-clock time at which the harness
	// expects to be usable again, parsed from session-limit banners
	// (e.g. Claude Code's "resets 6:40pm (Europe/Warsaw)"). Zero when
	// the banner did not include a parseable reset time, or when the
	// event was not raised by a session-limit classification.
	ResumeAt time.Time
}

// Session is a live handle to a supervised harness process. Construct
// one with Start; retrieve the terminal outcome with Wait. Stop
// requests a graceful shutdown without forcing the caller to track
// context cancellation. Concurrent calls to Wait, Stop, Snapshot, and
// Events are safe.
type Session struct {
	cfg       Config
	cmd       *exec.Cmd
	ptmx      *os.File
	pid       int
	startedAt time.Time

	classifier   Classifier
	lastOutput   *atomic.Int64
	recentOutput *recentOutputBuffer
	termState    *terminalState

	events       chan SessionEvent
	stopOnce     sync.Once
	stopRequest  chan struct{}
	classifierCh chan classification
	classifierOn chan struct{}

	doneCh chan struct{}

	fanout  *outputFanout
	stdinMu sync.Mutex

	writerMu   sync.Mutex
	writerHeld bool

	mu       sync.Mutex
	snap     Snapshot
	result   Result
	finalErr error
}

// classification is the internal mid-run handoff between the classifier
// goroutine and the supervisor.
type classification struct {
	status     Status
	class      ErrorClass
	reason     string
	terminal   bool
	httpCode   int
	retryAfter time.Duration
	resumeAt   time.Time
}

// Wait blocks until the Session terminates and returns the final
// Result. Calling Wait more than once is safe; every call returns the
// same value. Errors are returned only when the wrapper itself failed
// during supervision (PTY IO, classifier panic). Harness-level
// outcomes are reported via Result.Status with err == nil.
func (s *Session) Wait() (Result, error) {
	<-s.doneCh
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.finalErr
}

// Stop requests a graceful shutdown. The wrapper sends SIGTERM and
// escalates to SIGKILL after Config.WaitDelay if the process has not
// exited. Stop returns when the session has fully terminated (Wait
// would not block) or when ctx is cancelled. The session's final
// status will be Interrupted unless the harness happened to exit on
// its own before the signal arrived.
//
// Stop is idempotent. The first call wins; subsequent calls block on
// termination just like Wait.
func (s *Session) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() { close(s.stopRequest) })
	select {
	case <-s.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Snapshot returns a coherent point-in-time view of the session's
// state. It never blocks.
func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := s.snap
	if last := s.lastOutput.Load(); last > 0 {
		snap.LastOutputAt = time.Unix(0, last)
	}
	return snap
}

// Events returns the channel of state transitions for this Session.
// The channel is closed after the terminal event has been delivered.
// Slow consumers have events dropped on the floor; events are
// observability, not control flow.
func (s *Session) Events() <-chan SessionEvent { return s.events }

// PID returns the harness process ID, or 0 if the session never
// successfully started.
func (s *Session) PID() int { return s.pid }

// RecentOutput returns a snapshot of the last ~64KB of harness PTY
// output, ANSI escapes intact. This is the same buffer the built-in
// classifier inspects on each poll. Safe to call concurrently with the
// session running; the snapshot reflects bytes observed up to the call
// time and may grow on subsequent calls.
//
// Useful for callers that want to run their own post-hoc classification
// (e.g. matching harness-specific error fingerprints) over the same
// bytes the wrapper saw, without maintaining a parallel ring buffer.
func (s *Session) RecentOutput() string { return s.recentOutput.String() }

// startSession is the constructor used by Start. cfg is assumed to
// have been validated and defaulted.
func startSession(ctx context.Context, cfg Config) (*Session, error) {
	cfg.Trace.Emit(trace.Event{
		At:   time.Now(),
		Kind: "wrapper_started",
		Fields: map[string]any{
			"binary_path":   cfg.BinaryPath,
			"args":          cfg.Args,
			"working_dir":   cfg.WorkingDir,
			"idle_quiet":    cfg.IdleQuiet.String(),
			"idle_classify": cfg.IdleClassify.String(),
			"wait_delay":    cfg.WaitDelay.String(),
		},
	})

	cmd := exec.CommandContext(ctx, cfg.BinaryPath, cfg.Args...)
	cmd.Dir = cfg.WorkingDir
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = cfg.WaitDelay

	startedAt := time.Now()
	ptmx, err := pty.Start(cmd)
	if err != nil {
		if isBinaryNotFound(err) {
			return nil, fmt.Errorf("%w: %v", ErrBinaryNotFound, err)
		}
		return nil, fmt.Errorf("%w: %v", ErrPTYAllocation, err)
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	cfg.Trace.Emit(trace.Event{
		At:     time.Now(),
		Kind:   "pty_opened",
		Fields: map[string]any{"pid": pid},
	})

	termState := setupTerminalIfTTY(cfg.Stdin, cfg.Stdout, ptmx, cfg.Trace)

	s := &Session{
		cfg:          cfg,
		cmd:          cmd,
		ptmx:         ptmx,
		pid:          pid,
		startedAt:    startedAt,
		classifier:   resolveClassifier(cfg),
		lastOutput:   &atomic.Int64{},
		recentOutput: newRecentOutput(64 * 1024),
		termState:    termState,
		events:       make(chan SessionEvent, 16),
		stopRequest:  make(chan struct{}),
		classifierCh: make(chan classification, 1),
		classifierOn: make(chan struct{}),
		doneCh:       make(chan struct{}),
		fanout:       newOutputFanout(cfg.Stdout),
	}

	go s.supervise(ctx)
	return s, nil
}

// supervise owns the session's lifecycle. It runs the IO copy
// goroutines, dispatches the classifier, waits for the harness to
// exit (or for Stop / classification / context cancel to force
// termination), assembles the final Result, and closes the Events
// channel.
func (s *Session) supervise(ctx context.Context) {
	defer close(s.doneCh)
	defer close(s.events)
	defer s.termState.cleanup()
	defer s.fanout.closeAll()

	go runSessionClassifier(ctx, s)

	var outWG sync.WaitGroup
	outWG.Add(1)
	go func() {
		defer outWG.Done()
		// newLineSplitter is nil when no durable line tap is configured, and all
		// lineSplitter methods are nil-safe, so the no-tap path is unchanged.
		copyPTYOutput(s.ptmx, s.fanout, s.lastOutput, s.recentOutput, newLineSplitter(s.cfg.OnLine))
	}()

	stdinDone := s.startStdinCopy()

	waitCh := make(chan waitResult, 1)
	go func() {
		waitCh <- waitResult{err: s.cmd.Wait(), endedAt: time.Now()}
	}()

	out := s.awaitTermination(waitCh)

	close(s.classifierOn)
	_ = s.ptmx.Close()
	outWG.Wait()

	s.cfg.Trace.Emit(trace.Event{
		At:     time.Now(),
		Kind:   "pty_closed",
		Fields: map[string]any{"pid": s.pid},
	})
	if stdinDone != nil {
		select {
		case <-stdinDone:
		default:
		}
	}

	res := Result{
		PID:       s.pid,
		StartedAt: s.startedAt,
		EndedAt:   out.endedAt,
	}
	if last := s.lastOutput.Load(); last > 0 {
		res.LastOutputAt = time.Unix(0, last)
	}

	res.Status, res.ExitCode, res.Signal, res.Reason = classifyExit(s.cmd.ProcessState, out.waitErr, ctx.Err())

	actionable := s.resolveActionable(&res, out)

	s.cfg.Trace.Emit(trace.Event{
		At:   time.Now(),
		Kind: "harness_exited",
		Fields: map[string]any{
			"status":      string(res.Status),
			"exit_code":   res.ExitCode,
			"signal":      res.Signal,
			"reason":      res.Reason,
			"pid":         res.PID,
			"started_at":  res.StartedAt,
			"ended_at":    res.EndedAt,
			"duration_ms": res.EndedAt.Sub(res.StartedAt).Milliseconds(),
		},
	})

	s.mu.Lock()
	s.result = res
	s.snap.Status = res.Status
	s.snap.Reason = res.Reason
	s.mu.Unlock()

	final := SessionEvent{
		At:         time.Now(),
		Status:     res.Status,
		Class:      res.Class,
		Reason:     res.Reason,
		Terminated: true,
	}
	if actionable != nil {
		final.HTTPCode = actionable.httpCode
		final.RetryAfter = actionable.retryAfter
		final.ResumeAt = actionable.resumeAt
	}
	s.emitEvent(final)
}

// superviseOutcome captures how a supervised run terminated: the exit
// error and time, an optional terminal classification, whether a Stop
// was requested, and the last non-ErrNone class seen mid-run.
type superviseOutcome struct {
	waitErr           error
	endedAt           time.Time
	terminalClassDone *classification
	stopRequested     bool
	// lastErrClass is the most recent non-ErrNone class observed during
	// the run (terminal or not). It lets Result.Class inherit a mid-run
	// error class — e.g. a non-terminal API error — when the harness then
	// exits Failed without a terminal classification.
	lastErrClass ErrorClass
}

// startStdinCopy pipes cfg.Stdin into the PTY. It returns nil when no
// Stdin is configured, otherwise a channel closed when the copy is done.
func (s *Session) startStdinCopy() chan struct{} {
	if s.cfg.Stdin == nil {
		return nil
	}
	stdinDone := make(chan struct{})
	go func() {
		defer close(stdinDone)
		_, _ = io.Copy(s.ptmx, s.cfg.Stdin)
		// PTYs don't propagate the underlying io.Reader's EOF to the
		// slave automatically. For headless callers (Stdin is not
		// an os.File TTY), send EOT (Ctrl+D, 0x04) twice: the first
		// submits any pending unterminated line to the harness, the
		// second is interpreted by the PTY's canonical-mode line
		// discipline as end-of-file (at start of line, ^D returns
		// 0 bytes from read()). Skip when Stdin is a real TTY so
		// interactive sessions where the user keeps typing aren't
		// corrupted.
		if _, isTTYFile := s.cfg.Stdin.(*os.File); !isTTYFile {
			_, _ = s.ptmx.Write([]byte{0x04, 0x04})
		}
	}()
	return stdinDone
}

// awaitTermination blocks until the harness exits, a terminal
// classification fires, or a Stop is requested, returning how the run
// ended. Non-terminal classifications are recorded as they arrive.
func (s *Session) awaitTermination(waitCh chan waitResult) superviseOutcome {
	var out superviseOutcome
	for {
		select {
		case wr := <-waitCh:
			out.waitErr = wr.err
			out.endedAt = wr.endedAt
			return out
		case c := <-s.classifierCh:
			if c.class != ErrNone {
				out.lastErrClass = c.class
			}
			if !c.terminal {
				s.recordStatusChange(c, false)
				continue
			}
			cc := c
			out.terminalClassDone = &cc
			s.recordStatusChange(c, false)
			out.endedAt, out.waitErr = terminateAndWait(s.cmd, waitCh, s.cfg.WaitDelay)
			return out
		case <-s.stopRequest:
			out.stopRequested = true
			out.endedAt, out.waitErr = terminateAndWait(s.cmd, waitCh, s.cfg.WaitDelay)
			return out
		}
	}
}

// resolveActionable finalizes res.Status/Reason/Class from the run
// outcome and returns the actionable classification (if any) whose
// structured fields flow into the terminal event.
//
// actionable is the mid-run terminal classification when one fired;
// otherwise, for a plain failed exit that was not a stop request, a
// final one-shot pass over recent output so fast-failing transport/API
// errors — which exit before the idle classifier ever polls — still
// upgrade StatusFailed into an actionable, retryable status.
func (s *Session) resolveActionable(res *Result, out superviseOutcome) *classification {
	actionable := out.terminalClassDone
	if actionable == nil && !out.stopRequested && res.Status == StatusFailed {
		actionable = s.classifyOnExit()
	}
	if actionable != nil {
		res.Status = actionable.status
		res.Reason = actionable.reason
	}
	// Error class: prefer the actionable classification's class; otherwise,
	// for a plain Failed exit, inherit the last meaningful class seen mid-run
	// (e.g. a non-terminal API error). Clean/idle/interrupted stay ErrNone.
	switch {
	case actionable != nil && actionable.class != ErrNone:
		res.Class = actionable.class
	case res.Status == StatusFailed:
		res.Class = out.lastErrClass
	}
	if out.stopRequested && out.terminalClassDone == nil {
		res.Status = StatusInterrupted
		if res.Reason == "" {
			res.Reason = "stop requested"
		}
	}
	return actionable
}

// recordStatusChange updates Snapshot and emits a non-terminal event.
// It de-duplicates identical consecutive classifications so the
// classifier can poll freely without flooding subscribers.
func (s *Session) recordStatusChange(c classification, terminated bool) {
	s.mu.Lock()
	if s.snap.Status == c.status && s.snap.Reason == c.reason {
		s.mu.Unlock()
		return
	}
	s.snap.Status = c.status
	s.snap.Reason = c.reason
	s.mu.Unlock()
	s.emitEvent(SessionEvent{
		At:         time.Now(),
		Status:     c.status,
		Class:      c.class,
		Reason:     c.reason,
		Terminated: terminated,
		HTTPCode:   c.httpCode,
		RetryAfter: c.retryAfter,
		ResumeAt:   c.resumeAt,
	})
}

// emitEvent delivers e to subscribers, dropping it if the channel
// buffer is full so a slow consumer cannot stall the supervisor.
func (s *Session) emitEvent(e SessionEvent) {
	select {
	case s.events <- e:
	default:
	}
}

// terminateAndWait sends SIGTERM, waits up to waitDelay for the harness
// to exit, then escalates to SIGKILL.
func terminateAndWait(cmd *exec.Cmd, waitCh <-chan waitResult, waitDelay time.Duration) (time.Time, error) {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case wr := <-waitCh:
		return wr.endedAt, wr.err
	case <-time.After(waitDelay):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		wr := <-waitCh
		return wr.endedAt, wr.err
	}
}

// runSessionClassifier polls the configured Classifier on a fixed
// cadence, building a ClassifierInput from the live activity counters
// and forwarding non-empty Classifications to the supervisor. It also
// emits the original output_quiet / output_classify_threshold trace
// events for parity with the original idle classifier.
func runSessionClassifier(ctx context.Context, s *Session) {
	cfg := s.cfg
	tick := max(cfg.IdleQuiet/3, 100*time.Millisecond)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	st := classifierState{lastSeen: -1, staleEnabled: cfg.StaleThreshold > 0}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.classifierOn:
			return
		case <-ticker.C:
			st.onTick(s, cfg)
		}
	}
}

// classifierState carries the per-run bookkeeping the polling classifier
// needs across ticks: the last-seen output timestamp, one-shot trace
// emission latches, and whether a terminal classification has been
// dispatched.
type classifierState struct {
	lastSeen        int64
	quietEmitted    bool
	classifyEmitted bool
	staleEmitted    bool
	dispatched      bool
	staleEnabled    bool
}

// onTick evaluates the activity counters once and, when the output has
// settled, emits threshold traces and dispatches a classification.
func (st *classifierState) onTick(s *Session, cfg Config) {
	last := s.lastOutput.Load()
	if last == 0 {
		return
	}
	outputChanged := last != st.lastSeen
	if outputChanged {
		st.lastSeen = last
		st.quietEmitted = false
		st.classifyEmitted = false
		st.staleEmitted = false
		// Fall through so high-confidence classifiers
		// (api_error) can fire even while output is still
		// streaming. Cost/Retry/Prompt are gated on
		// Quiet/Idle below, which won't be true here, so
		// they stay silent until the output settles.
	}
	sinceLast := time.Since(time.Unix(0, last))
	quiet := !outputChanged && sinceLast >= cfg.IdleQuiet
	idle := !outputChanged && sinceLast >= cfg.IdleClassify
	stale := !outputChanged && st.staleEnabled && sinceLast >= cfg.StaleThreshold

	st.emitThresholdTraces(s, cfg, sinceLast, quiet, idle, stale)

	if st.dispatched {
		return
	}
	st.dispatchClassification(s, cfg, sinceLast, quiet, idle)
}

// emitThresholdTraces emits the output_quiet / output_classify_threshold /
// harness_stale trace events (and the stale status change) at most once
// per settle window.
func (st *classifierState) emitThresholdTraces(s *Session, cfg Config, sinceLast time.Duration, quiet, idle, stale bool) {
	if quiet && !st.quietEmitted {
		cfg.Trace.Emit(trace.Event{
			At:   time.Now(),
			Kind: "output_quiet",
			Fields: map[string]any{
				"since_last_output_ms": sinceLast.Milliseconds(),
				"threshold_ms":         cfg.IdleQuiet.Milliseconds(),
			},
		})
		st.quietEmitted = true
	}
	if idle && !st.classifyEmitted {
		cfg.Trace.Emit(trace.Event{
			At:   time.Now(),
			Kind: "output_classify_threshold",
			Fields: map[string]any{
				"since_last_output_ms": sinceLast.Milliseconds(),
				"threshold_ms":         cfg.IdleClassify.Milliseconds(),
			},
		})
		st.classifyEmitted = true
	}
	if stale && !st.staleEmitted {
		cfg.Trace.Emit(trace.Event{
			At:   time.Now(),
			Kind: "harness_stale",
			Fields: map[string]any{
				"since_last_output_ms": sinceLast.Milliseconds(),
				"threshold_ms":         cfg.StaleThreshold.Milliseconds(),
			},
		})
		s.recordStatusChange(classification{
			status:   StatusStale,
			reason:   fmt.Sprintf("no output for %s", sinceLast.Round(time.Second)),
			terminal: false,
		}, false)
		st.staleEmitted = true
	}
}

// dispatchClassification runs the configured Classifier and forwards a
// non-empty result to the supervisor, latching dispatched on a terminal
// classification.
func (st *classifierState) dispatchClassification(s *Session, cfg Config, sinceLast time.Duration, quiet, idle bool) {
	classification := s.classifier.Classify(ClassifierInput{
		RecentOutput:    s.recentOutput.String(),
		SinceLastOutput: sinceLast,
		Quiet:           quiet,
		Idle:            idle,
	})
	if classification.Status == "" {
		return
	}

	emitClassifierTrace(cfg, classification)
	select {
	case s.classifierCh <- toInternalClassification(classification):
		if classification.Terminal {
			st.dispatched = true
		}
	default:
	}
}

// classifyOnExit runs a final one-shot classification over the harness's
// recent output after a plain failed exit. A harness that fails fast — e.g.
// prints "connection refused" and exits before the idle classifier polls —
// never produces a mid-run classification, so without this pass its terminal
// status stays StatusFailed and the retry layer can't act on it. Returns nil
// when the output yields no actionable (or only a waiting-for-input)
// classification. Uses the session's resolved classifier so a custom
// cfg.Classifier is honored.
func (s *Session) classifyOnExit() *classification {
	c := s.classifier.Classify(ClassifierInput{
		RecentOutput: s.recentOutput.String(),
		Idle:         true,
		Quiet:        false,
	})
	if c.Status == "" || c.Status == StatusWaitingForInput {
		return nil
	}
	ic := toInternalClassification(c)
	return &ic
}

func toInternalClassification(c Classification) classification {
	return classification{
		status:     c.Status,
		class:      c.Class,
		reason:     c.Reason,
		terminal:   c.Terminal,
		httpCode:   c.HTTPCode,
		retryAfter: c.RetryAfter,
		resumeAt:   c.ResumeAt,
	}
}

func emitClassifierTrace(cfg Config, c Classification) {
	kind := "harness_classified"
	switch c.Status {
	case StatusBlockedByCost:
		kind = "harness_blocked_by_cost"
	case StatusRetryLater:
		kind = "harness_retry_later"
	case StatusWaitingForInput:
		kind = "harness_waiting_for_input"
	case StatusAPIError:
		kind = "harness_api_error"
	}
	fields := map[string]any{
		"status":   string(c.Status),
		"reason":   c.Reason,
		"terminal": c.Terminal,
	}
	if c.HTTPCode != 0 {
		fields["http_code"] = c.HTTPCode
	}
	if c.RetryAfter > 0 {
		fields["retry_after_ms"] = c.RetryAfter.Milliseconds()
	}
	if !c.ResumeAt.IsZero() {
		fields["resume_at"] = c.ResumeAt.Format(time.RFC3339)
	}
	cfg.Trace.Emit(trace.Event{
		At:     time.Now(),
		Kind:   kind,
		Fields: fields,
	})
}
