package harness

import (
	"context"
	"fmt"

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
	effective, strategy := resolveStrategy(cfg.TranscriptMode, rp)

	tap := &streamTap{
		harness:  cfg.Wrapper.Harness,
		runID:    cfg.RunID,
		mode:     effective,
		stream:   rp.Stream,
		sessions: rp.SessionID,
		onEvent:  cfg.OnEvent,
		active:   strategy == "stream" && cfg.OnEvent != nil,
	}

	wc := cfg.Wrapper
	runCtx := ctx
	// Install the tap when there is something to observe: live events to parse,
	// or a session id to capture for resume. With neither (e.g. Off mode and no
	// SessionID extractor), the wrapper path is byte-for-byte unchanged.
	if tap.active || tap.sessions != nil {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithCancel(ctx)
		defer cancel()
		tap.cancel = cancel
		wc.OnLine = tap.onLine
	}

	wres, werr := wrapper.Run(runCtx, wc)

	res := Result{Result: wres, TranscriptStrategy: strategy, HarnessSessionID: tap.sessionID}
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

// resolveStrategy maps the requested mode + resolved capabilities to the
// effective (latched) parent strategy and its label. Hooks is not yet
// implemented (P3c), so Hooks/Auto degrade to the StreamParse floor when a
// stream parser is available.
func resolveStrategy(mode Mode, rp ResolvedProfile) (effective Mode, label string) {
	switch mode {
	case TranscriptStreamParse, TranscriptHooks, TranscriptAuto:
		if rp.Stream != nil {
			return TranscriptStreamParse, "stream"
		}
	}
	return TranscriptOff, "none"
}

// streamTap is the per-run, goroutine-confined consumer of wrapper's durable
// line tap. wrapper invokes onLine serially from one copy goroutine and joins
// it before Run returns, so the mutable fields below need no synchronization:
// the tap goroutine is the sole writer, and Run reads them only after the join.
type streamTap struct {
	harness  string
	runID    string
	mode     Mode
	stream   StreamParser
	sessions SessionIDExtractor
	onEvent  func(transcript.EventEnvelope) error
	active   bool               // stream parsing on (mode==stream and a sink exists)
	cancel   context.CancelFunc // terminate the harness on a delivery failure

	// --- mutated only in the PTY copy goroutine ---
	sessionID  string
	seq        int
	deliverErr error
}

// onLine is wrapper.Config.OnLine: the durable per-line callback. It captures
// the session id (first occurrence wins) and, in stream mode, parses the line
// into events. Once a delivery has failed it becomes inert (the run is already
// being torn down).
func (o *streamTap) onLine(line string) {
	if o.deliverErr != nil {
		return
	}
	if o.sessions != nil && o.sessionID == "" {
		if id, ok := o.sessions.ExtractSessionID(line); ok {
			o.sessionID = id
		}
	}
	if !o.active || o.stream == nil {
		return
	}
	for _, pe := range o.stream.ParseStreamLine(line) {
		if !o.emit(pe) {
			return
		}
	}
}

// emit shapes one ParsedEvent into an envelope, applies the central authority
// filter, and delivers it via onEvent. It returns false only when delivery
// failed (the caller stops and the run aborts); a filtered-out event returns
// true (not an error).
func (o *streamTap) emit(pe transcript.ParsedEvent) bool {
	if !admitParent(o.mode, pe.Event.Source, pe.Event.Type, pe.ParentSessionID != "") {
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
