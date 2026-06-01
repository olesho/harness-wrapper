package harness

import (
	"context"
	"fmt"
	"os"

	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Config configures a single harness.Run. It embeds the low-level wrapper.Config
// (binary/args/env/stdout/classifier/idle knobs) and adds the consumer-facing
// transcript API. This is loom's intended entrypoint (not wrapper.Run directly).
//
// harness.Run OWNS wrapper.Config.OnLine: any value the caller sets in
// Wrapper.OnLine is overwritten by the orchestrator's durable tap.
type Config struct {
	// Wrapper is the low-level supervisor configuration passed through to
	// wrapper.Run. Wrapper.Harness also selects the per-harness Profile (via
	// For); when it names no registered profile, acquisition is Off and the
	// captured session id is empty.
	Wrapper wrapper.Config

	// RunID is the STABLE LOGICAL run id stamped into every emitted envelope so
	// the consumer can dedup / collapse replays across resume (review #2).
	// Optional; empty means envelopes carry no run id.
	RunID string

	// TranscriptMode selects the acquisition strategy (default Off — no events).
	TranscriptMode Mode

	// HookCommand is the loom binary path + subcommand templated into the
	// harness's hook config (e.g. {"/abs/loom","hooks"}); each rendered entry
	// becomes `<HookCommand...> <harness> <arg>`. Required for the Hooks
	// strategy; ignored otherwise.
	HookCommand []string

	// Yield, if set, enables cooperative preemption: the orchestrator wires its
	// file into the harness env (HW_YIELD_FILE) so the yield-guard hook can block
	// the next tool when the caller calls Yield.Request. Only effective with the
	// Hooks strategy (the guard is a hook). The caller owns its lifecycle.
	Yield *YieldControl

	// OnDisplayLine, if set, receives each raw output line on a BEST-EFFORT
	// basis for the caller's usage/display needs (e.g. loom's stream UI). It is
	// delivered via an internal bounded drop-oldest queue on a separate
	// goroutine and NEVER blocks the harness — under sustained back-pressure the
	// oldest lines are dropped (counted in Result.DisplayLinesDropped). For the
	// transcript/session-id path, which must not drop, use OnEvent (durable).
	OnDisplayLine func(line string)

	// OnEvent is the durable, idempotent sink for normalized transcript events.
	// It is invoked SYNCHRONOUSLY from the PTY read loop, so it back-pressures
	// the harness (a slow sink slows the harness) rather than dropping an event,
	// and it MAY return an error: on a delivery failure the orchestrator
	// terminates the harness (graceful ctx-cancel → wait → kill) and fails the
	// run, because losing transcript truth is worse than stopping the agent. A
	// panic in OnEvent is converted to an error, never swallowed. nil ⇒ no
	// delivery (and no stream parsing is performed).
	OnEvent func(transcript.EventEnvelope) error
}

// Result wraps wrapper.Result with the transcript outcome.
type Result struct {
	wrapper.Result

	// TranscriptStrategy is the parent acquisition strategy actually used this
	// run: "stream", "hooks", or "none".
	TranscriptStrategy string

	// HarnessSessionID is the native session id captured from the live stream
	// (for resume / lock persistence), or empty if none was observed.
	HarnessSessionID string

	// DisplayLinesDropped counts the OnDisplayLine lines dropped under
	// back-pressure (0 when OnDisplayLine is unset or kept up). The durable
	// transcript path never drops; this is only the best-effort display queue.
	DisplayLinesDropped uint64
}

// Run resolves the per-harness Profile, composes wrapper.Run with the durable
// line tap, and delivers normalized transcript events to cfg.OnEvent according
// to cfg.TranscriptMode.
//
// Concurrency: the tap runs in wrapper's single PTY-copy goroutine, which
// wrapper joins (and flushes) before Run returns. So the session id / delivery
// error the tap records are read here AFTER wrapper.Run returns with an
// established happens-before — no locking needed.
func Run(ctx context.Context, cfg Config) (Result, error) {
	rp := resolveProfile(cfg)
	plan := planAcquisition(cfg.TranscriptMode, rp, cfg.OnEvent != nil)

	tap := &streamTap{
		harness:    cfg.Wrapper.Harness,
		runID:      cfg.RunID,
		stream:     rp.Stream,
		sessions:   rp.SessionID,
		onEvent:    cfg.OnEvent,
		emitLive:   plan.useStream,
		bufferLive: plan.shadow,
		display:    newDisplaySink(cfg.OnDisplayLine),
	}

	wc := cfg.Wrapper
	runCtx := ctx

	// Hooks: ensure the per-worktree config + a spool dir before launch; the
	// harness propagates HW_EVENT_SPOOL to its hook subprocesses, which write
	// parsed events there for the post-exit drain.
	var spoolDir string
	if plan.useHooks {
		dir, err := os.MkdirTemp("", "hw-spool-")
		if err != nil {
			return Result{TranscriptStrategy: "none"}, fmt.Errorf("harness: create spool dir: %w", err)
		}
		spoolDir = dir
		defer func() { _ = os.RemoveAll(spoolDir) }()
		if err := rp.Hooks.EnsureConfig(cfg.Wrapper.WorkingDir, cfg.HookCommand); err != nil {
			return Result{TranscriptStrategy: "none"}, fmt.Errorf("harness: ensure hooks: %w", err)
		}
		wc.Env = hookEnv(cfg.Wrapper.Env, spoolDir, cfg.Wrapper.WorkingDir, cfg.Yield)
	}

	// Install the tap when there is something to observe: live events to parse
	// or buffer, or a session id to capture for resume. With none (e.g. Off mode
	// and no SessionID extractor), the wrapper path is byte-for-byte unchanged.
	if tap.installs() {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithCancel(ctx)
		defer cancel()
		tap.cancel = cancel
		wc.OnLine = tap.onLine
	}

	wres, werr := wrapper.Run(runCtx, wc)

	// The tap goroutine has joined (wrapper joins it before returning), so it is
	// now safe to close the display sink (no concurrent push) and drain the spool.
	dropped := tap.display.close()

	// Post-exit (grace window): in hooks mode the SessionEnd/Stop hooks fire
	// around exit, so the spool is drained AFTER the process is gone.
	strategy := plan.label
	if plan.useHooks {
		strategy = tap.drainHooks(spoolDir)
	}

	res := Result{Result: wres, TranscriptStrategy: strategy, HarnessSessionID: tap.sessionID, DisplayLinesDropped: dropped}
	if tap.deliverErr != nil {
		// Surface the delivery failure as the run error: a fast-fail beats a
		// silently truncated transcript. (The harness was already terminated via
		// ctx cancel when the failure was recorded.)
		return res, fmt.Errorf("harness: transcript delivery failed: %w", tap.deliverErr)
	}
	return res, werr
}

// resolveProfile looks up and resolves the per-harness Profile for the run, or
// returns the zero ResolvedProfile (all capabilities nil) when no profile is
// registered for cfg.Wrapper.Harness.
func resolveProfile(cfg Config) ResolvedProfile {
	p, ok := For(cfg.Wrapper.Harness)
	if !ok {
		return ResolvedProfile{}
	}
	return p.Resolve(ResolveContext{
		BinaryPath: cfg.Wrapper.BinaryPath,
		Args:       cfg.Wrapper.Args,
		Env:        cfg.Wrapper.Env,
		Cwd:        cfg.Wrapper.WorkingDir,
	})
}

// acqPlan is the resolved acquisition plan for a run.
type acqPlan struct {
	label     string // initial strategy label ("hooks"|"stream"|"none"); drainHooks may downgrade hooks→stream
	useHooks  bool   // ensure config + spool + post-exit drain
	useStream bool   // emit live stream events during the run
	shadow    bool   // buffer live stream events as a hooks fallback
}

// planAcquisition maps the requested mode + resolved capabilities (+ whether a
// sink exists) to the acquisition plan. Hooks/Auto latch to hooks when a
// HookProvider resolved, with the live stream BUFFERED as a fallback so the
// transcript isn't lost if no hook fires (a post-exit latch; the mid-run
// two-stage confirmation is a later refinement). Either degrades to the
// StreamParse floor when only a stream parser is available.
func planAcquisition(mode Mode, rp ResolvedProfile, haveSink bool) acqPlan {
	if !haveSink {
		return acqPlan{label: "none"} // session id may still be captured (see installs)
	}
	switch mode {
	case TranscriptHooks, TranscriptAuto:
		if rp.Hooks != nil {
			return acqPlan{label: "hooks", useHooks: true, shadow: rp.Stream != nil}
		}
		if rp.Stream != nil {
			return acqPlan{label: "stream", useStream: true}
		}
	case TranscriptStreamParse:
		if rp.Stream != nil {
			return acqPlan{label: "stream", useStream: true}
		}
	case TranscriptOff:
	}
	return acqPlan{label: "none"}
}

// hookEnv augments the harness launch env with the HW_* hook variables. It must
// APPEND to the inherited env (defaulting to os.Environ when base is nil), not
// replace it, so the harness keeps its normal environment. The yield var is
// added only when a YieldControl was supplied.
func hookEnv(base []string, spoolDir, cwd string, yield *YieldControl) []string {
	if base == nil {
		base = os.Environ()
	}
	home, _ := os.UserHomeDir()
	out := make([]string, 0, len(base)+4)
	out = append(out, base...)
	out = append(out, EnvSpool+"="+spoolDir, EnvHookCwd+"="+cwd, EnvHome+"="+home)
	if yield != nil {
		out = append(out, EnvYieldFile+"="+yield.path)
	}
	return out
}

// streamTap is the per-run, goroutine-confined consumer of wrapper's durable
// line tap. wrapper invokes onLine serially from one copy goroutine and joins
// it before Run returns, so the mutable fields below need no synchronization:
// the tap goroutine is the sole writer DURING the run, and Run (the same
// goroutine that called wrapper.Run) is the sole reader/writer AFTER the join
// (the post-exit drainHooks).
type streamTap struct {
	harness    string
	runID      string
	stream     StreamParser
	sessions   SessionIDExtractor
	onEvent    func(transcript.EventEnvelope) error
	emitLive   bool               // emit live stream events (StreamParse)
	bufferLive bool               // buffer live stream events (hooks fallback)
	display    *displaySink       // best-effort display callback (nil ⇒ none)
	cancel     context.CancelFunc // terminate the harness on a delivery failure

	// --- mutated in the PTY copy goroutine during the run, then by Run after ---
	sessionID  string
	seq        int
	deliverErr error
	shadow     []transcript.ParsedEvent // buffered live events (flushed iff no hook fires)
}

// installs reports whether the durable line tap must be attached.
func (o *streamTap) installs() bool {
	return o.sessions != nil || o.emitLive || o.bufferLive || o.display != nil
}

// onLine is wrapper.Config.OnLine: the durable per-line callback. It captures
// the session id (first occurrence wins) and, depending on mode, emits live
// stream events or buffers them for the hooks fallback. Once a delivery has
// failed it becomes inert (the run is already being torn down).
func (o *streamTap) onLine(line string) {
	// Best-effort display first: every line, independent of the transcript path,
	// and never blocking (the sink drops under back-pressure).
	o.display.push(line)
	if o.deliverErr != nil {
		return
	}
	if o.sessions != nil && o.sessionID == "" {
		if id, ok := o.sessions.ExtractSessionID(line); ok {
			o.sessionID = id
		}
	}
	if o.stream == nil || (!o.emitLive && !o.bufferLive) {
		return
	}
	for _, pe := range o.stream.ParseStreamLine(line) {
		switch {
		case o.bufferLive:
			o.shadow = append(o.shadow, pe)
		case o.emitLive:
			if !o.emit(pe, TranscriptStreamParse) {
				return
			}
		}
	}
}

// drainHooks consumes the post-exit spool, emits the hook (file) events under
// Hooks authority, and — if no PARENT-conversation event was captured (no hook
// fired) — flushes the buffered live stream under StreamParse authority so the
// transcript is never lost. Returns the resulting strategy label.
func (o *streamTap) drainHooks(spoolDir string) string {
	drained, _ := DrainSpool(spoolDir) // best-effort; malformed files are skipped+reported
	parents := 0
	for _, pe := range drained {
		if isParentConversationKind(pe.Event.Type) {
			parents++
		}
		if !o.emit(pe, TranscriptHooks) {
			return "hooks"
		}
	}
	if parents > 0 {
		return "hooks" // hooks captured the conversation
	}
	// Fallback: no parent captured via hooks → flush the shadow buffer.
	for _, pe := range o.shadow {
		if !o.emit(pe, TranscriptStreamParse) {
			return "stream"
		}
	}
	if len(o.shadow) > 0 {
		return "stream"
	}
	return "hooks" // hooks attempted; nothing captured (empty transcript)
}

// emit shapes one ParsedEvent into an envelope, applies the central authority
// filter for the given (latched) mode, and delivers it via onEvent. It returns
// false only when delivery failed (the caller stops and the run aborts); a
// filtered-out event returns true (not an error).
func (o *streamTap) emit(pe transcript.ParsedEvent, mode Mode) bool {
	if !admitParent(mode, pe.Event.Source, pe.Event.Type, pe.ParentSessionID != "") {
		return true
	}
	ev := pe.Event
	ev.Seq = o.seq
	ev.SchemaVersion = transcript.SchemaVersion
	o.seq++

	hsid := pe.HarnessSessionID
	if hsid == "" {
		hsid = o.sessionID
	}
	if o.onEvent == nil {
		return true
	}
	if err := o.deliver(transcript.EventEnvelope{
		RunID:            o.runID,
		Harness:          o.harness,
		HarnessSessionID: hsid,
		ParentSessionID:  pe.ParentSessionID,
		Event:            ev,
	}); err != nil {
		o.deliverErr = err
		if o.cancel != nil {
			o.cancel() // terminate the harness; losing transcript truth is worse
		}
		return false
	}
	return true
}

// deliver calls onEvent, converting a panic into an error so a misbehaving sink
// fails the run cleanly rather than crashing the copy goroutine.
func (o *streamTap) deliver(env transcript.EventEnvelope) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("OnEvent panicked: %v", r)
		}
	}()
	return o.onEvent(env)
}
