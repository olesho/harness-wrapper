package conformance

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

var scenarios = []scenario{
	{"describe", describe},
	{"provision", provision},
	{"open", open},
	{"turn", turnScenario},
	{"send-refused", sendRefused},
	{"interrupt", interrupt},
	{"interrupt-early", interruptEarly},
	{"api-error", apiError},
	{"usage-limit", usageLimit},
	{"observe", observe},
	{"observe-size", observeSize},
	{"record-after-crash", recordAfterCrash},
	{"record-crash-mid-turn", recordCrashMidTurn},
	{"record-no-binary", recordNoBinary},
	{"reopen", reopen},
	{"rescan", rescan},
	{"close-drain", closeDrain},
	{"close-starting", closeStarting},
	{"prompts", prompts},
	{"load", load},
	{"load-missing", loadMissing},
	{"load-refused", loadRefused},
	{"autonomous", autonomous},
	{"load-autonomous", loadAutonomous},
	{"placeholder", placeholderScenario},
}

// describe: the Descriptor is well formed.
func describe(c *check) {
	d := c.desc
	if !contract.Compatible(d.Contract) {
		c.fail("describe.contract", "contract %q is not %s-compatible", d.Contract, contract.Version)
	}
	if d.Harness.Name == "" || d.Harness.Version == "" || d.Harness.Adapter == "" {
		c.fail("describe.harness", "harness %+v: want a name, a version and an adapter", d.Harness)
	}
	valid := map[string]bool{}
	for _, v := range contract.Capability("").Values() {
		valid[v] = true
	}
	for _, cap := range d.Capabilities {
		if !valid[string(cap)] {
			c.fail("describe.capabilities", "unknown capability %q", cap)
		}
	}
	if d.CheckpointFormat < 1 {
		c.fail("describe.checkpoint_format", "checkpoint_format %d", d.CheckpointFormat)
	}
	if d.Limits.MaxInputBytes <= 0 || d.Limits.MaxInputBytes > contract.MaxInputBytes {
		c.fail("describe.limits", "max_input_bytes %d", d.Limits.MaxInputBytes)
	}
	if !reflect.DeepEqual(d, c.f.Adapter.Describe()) {
		c.fail("describe.pure", "two Describe calls differ")
	}
	if err := contract.CheckEgress(d); err != nil {
		c.fail("describe.egress", "%v", err)
	}
	switch l := d.Load; {
	case !c.has(contract.CapSessionLoad):
		if l != nil {
			c.fail("describe.load", "load %+v declared without capability %s", *l, contract.CapSessionLoad)
		}
	case l == nil || len(l.Formats) == 0 || len(l.Sources) == 0:
		c.fail("describe.load", "%s, and no archive format or no source version named", contract.CapSessionLoad)
	case !d.Loads(l.Formats[0], d.Harness.Version):
		c.fail("describe.load", "the sources %v lack the harness's own version %s: it cannot load what it saves", l.Sources, d.Harness.Version)
	}
}

// provision: pure, valid, and it refuses what the Descriptor does not name.
func provision(c *check) {
	a := c.newAgent()
	req := c.request(a, nil)
	r1, err1 := c.f.Adapter.Provision(req)
	r2, err2 := c.f.Adapter.Provision(req)
	if err1 != nil || err2 != nil {
		c.stop("provision.valid", "Provision: %v / %v", err1, err2)
	}
	if !reflect.DeepEqual(r1, r2) {
		c.fail("provision.pure", "the same request rendered two different results")
	}
	if err := r1.Validate(); err != nil {
		c.fail("provision.valid", "the result breaks the Supervisor's rules: %v", err)
	}
	bad := req
	bad.Spec.PermissionPosture = "yolo"
	if _, err := c.f.Adapter.Provision(bad); codeOf(err) != contract.CodeInvalidSpec {
		c.fail("provision.unsupported", "posture %q: %v, want invalid_spec", "yolo", err)
	}
	bad = req
	bad.Spec.Credential = &contract.CredentialRef{Kind: "no_such_credential"}
	var e *contract.Error
	if _, err := c.f.Adapter.Provision(bad); codeOf(err) != contract.CodeUnsupported {
		c.fail("provision.unsupported", "an undeclared credential kind: %v, want unsupported", err)
	} else if asErr(err, &e); e.Field == "" {
		c.fail("provision.unsupported", "unsupported, but naming no field")
	}
}

func asErr(err error, e **contract.Error) bool {
	ce, ok := err.(*contract.Error)
	if ok {
		*e = ce
	}
	return ok
}

// open: the handle's states before and at opening.
func open(c *check) {
	a := c.newAgent()
	bad := c.openRequest(a, contract.OpenFresh, "", &contract.Checkpoint{Format: c.desc.CheckpointFormat, Data: []byte("{}")})
	if _, err := c.f.Adapter.NewSession(bad); codeOf(err) != contract.CodeProtocol {
		c.fail("open.fresh-checkpoint", "a fresh open with a checkpoint: %v, want protocol", err)
	}
	s, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenFresh, "", nil))
	if err != nil {
		c.stop("open.idle", "NewSession: %v", err)
	}
	if p := s.State().Phase; p != contract.PhaseUnopened {
		c.fail("open.unopened", "a new handle is %s, want unopened", p)
	}
	ctx, cancel := c.ctx()
	_, err = s.Send(ctx, contract.Text(newInputID(), "PING 0"))
	cancel()
	if codeOf(err) != contract.CodeUnexpected || certaintyOf(err) != contract.NotSubmitted {
		c.fail("open.send-before-open", "Send before Open: %v, want unexpected, not_submitted", err)
	}
	s, _ = c.openWatch(s, "open.idle")
	if p := c.awaitPhase(s, contract.PhaseIdle); p != contract.PhaseIdle {
		c.fail("open.idle", "after Open the Session is %s, want idle", p)
	}
}

// turnScenario: one turn, as the turn rules require.
func turnScenario(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "PING 1")
	end, ok := h.turnEnded(in)
	if !ok {
		c.stop("turn.started-ended", "no turn_ended for %s", in)
	}
	if end.Outcome != contract.TurnCompleted || !strings.Contains(end.Text, "PONG 1") {
		c.fail("turn.completed", "turn_ended %+v, want completed with PONG 1", end)
	}
	c.awaitPhase(s, contract.PhaseIdle)
	time.Sleep(200 * time.Millisecond) // let late observations arrive
	starts := h.deliveries(func(o contract.Observation) bool { return o.Kind == contract.KindTurnStarted && o.InputID == in })
	if len(ids(starts)) != 1 {
		c.fail("turn.started-ended", "%d turn_started ids for %s, want 1", len(ids(starts)), in)
	}
	ends := h.deliveries(func(o contract.Observation) bool { return o.Kind == contract.KindTurnEnded && o.InputID == in })
	if len(ids(ends)) != 1 {
		c.fail("turn.started-ended", "%d turn_ended ids for %s, want 1: %v", len(ids(ends)), in, ids(ends))
	}
	outcomes := map[contract.TurnOutcome]bool{}
	for _, o := range ends {
		var d contract.TurnEndedData
		_ = o.Decode(&d)
		outcomes[d.Outcome] = true
		if o.ID != contract.ObservationID(contract.KindTurnEnded, in) {
			c.fail("turn.ids", "turn_ended id %q, want %q", o.ID, contract.ObservationID(contract.KindTurnEnded, in))
		}
	}
	if len(outcomes) > 1 {
		c.fail("turn.one-outcome", "input %s reported %d outcomes", in, len(outcomes))
	}
	for id, o := range h.ids() {
		if !strings.HasPrefix(id, string(o.Kind)+":") {
			c.fail("turn.ids", "id %q does not start with its kind %q", id, o.Kind)
		}
		if o.Kind.Capability() != "" && !c.has(o.Kind.Capability()) {
			c.fail("turn.capabilities", "%s observed without capability %s", o.Kind, o.Kind.Capability())
		}
	}
	if o, ok := h.awaitKind(contract.KindUserInput, in); !ok || o.Origin != contract.OriginRecord {
		c.fail("turn.record-origin", "user_input for %s: %+v, want one of origin record", in, o)
	}
	if o, ok := h.awaitKind(contract.KindAssistantText, in); !ok || o.Origin != contract.OriginRecord {
		c.fail("turn.record-origin", "assistant_text for %s: %+v, want one of origin record", in, o)
	}
}

func ids(obs []contract.Observation) []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range obs {
		if !seen[o.ID] {
			seen[o.ID] = true
			out = append(out, o.ID)
		}
	}
	return out
}

// sendRefused: a Send outside idle, or of bad content, is refused and never
// runs.
func sendRefused(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	ctx, cancel := c.ctx()
	defer cancel()
	_, err := s.Send(ctx, contract.Input{InputID: newInputID(), Content: []contract.ContentPart{{Type: contract.ContentText}}})
	if codeOf(err) != contract.CodeInvalidInput || certaintyOf(err) != contract.NotSubmitted {
		c.fail("send.invalid", "empty text: %v, want invalid_input, not_submitted", err)
	}
	slow := c.send(s, "SLOW 20")
	busy := newInputID()
	_, err = s.Send(ctx, contract.Text(busy, "PING 2"))
	if codeOf(err) != contract.CodeBusy || certaintyOf(err) != contract.NotSubmitted {
		c.fail("send.busy", "Send while busy: %v, want busy, not_submitted", err)
	}
	if _, ok := h.turnEnded(slow); !ok {
		c.fail("send.busy", "the running turn did not end")
	}
	time.Sleep(100 * time.Millisecond)
	if got := h.deliveries(func(o contract.Observation) bool { return o.InputID == busy }); len(got) > 0 {
		c.fail("send.busy", "the refused input ran: %d observations", len(got))
	}
}

// interrupt: an interrupt names its input, and reaches no other.
func interrupt(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	ctx, cancel := c.ctx()
	defer cancel()
	if out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: newInputID()}); err != nil || out != contract.InterruptNoTurn {
		c.fail("interrupt.no-turn", "interrupt while idle: %v %v, want no_turn", out, err)
	}
	slow := c.send(s, "SLOW 200")
	if _, ok := h.awaitKind(contract.KindUserInput, slow); !ok {
		c.stop("interrupt.mid-text", "the slow turn never started")
	}
	time.Sleep(150 * time.Millisecond)
	out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: slow, DeadlineMS: 10000})
	if err != nil || out != contract.InterruptStopped {
		c.fail("interrupt.mid-text", "interrupt mid-reply: %v %v, want stopped", out, err)
	}
	end, ok := h.turnEnded(slow)
	if !ok || end.Outcome != contract.TurnInterrupted {
		c.fail("interrupt.mid-text", "turn_ended %+v, want interrupted", end)
	}
	again, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: slow})
	if err != nil || again != out {
		c.fail("interrupt.repeat", "a repeated interrupt: %v %v, want the established %v", again, err, out)
	}
	c.awaitPhase(s, contract.PhaseIdle)
	next := c.send(s, "SLOW 10")
	stale, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: slow})
	if err != nil || stale != contract.InterruptTooLate && stale != out {
		c.fail("interrupt.too-late", "interrupting an ended input while another runs: %v %v, want too_late", stale, err)
	}
	end, ok = h.turnEnded(next)
	if !ok || end.Outcome != contract.TurnCompleted {
		c.fail("interrupt.other-turn", "an interrupt of an older input reached the running turn: %+v", end)
	}
}

// interruptEarly: an interrupt before the first token stops the turn, or
// cancels it only on evidence; either way the turn's end agrees.
func interruptEarly(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	ctx, cancel := c.ctx()
	defer cancel()
	in := c.send(s, "STALL 30")
	out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: in, DeadlineMS: 15000})
	if err != nil {
		if codeOf(err) != contract.CodeInterruptUnconfirmed {
			c.fail("interrupt.early", "interrupt before the first token: %v", err)
		}
		return
	}
	end, ok := h.turnEnded(in)
	switch {
	case !ok:
		c.fail("interrupt.early", "no turn_ended after %v", out)
	case out == contract.InterruptStopped && end.Outcome != contract.TurnInterrupted,
		out == contract.InterruptCancelled && end.Outcome != contract.TurnCancelled:
		c.fail("interrupt.early", "interrupt %v, but the turn ended %v", out, end.Outcome)
	case out != contract.InterruptStopped && out != contract.InterruptCancelled:
		c.fail("interrupt.early", "interrupt of the running turn: %v, want stopped or cancelled", out)
	}
}

// apiError: an error the harness retries past completes the turn; one it
// cannot errors it, and leaves the Session usable.
func apiError(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "ERR 529 1")
	end, ok := h.turnEnded(in)
	if !ok || end.Outcome != contract.TurnCompleted || !strings.Contains(end.Text, "RECOVERED") {
		c.fail("api-error.recovers", "turn_ended %+v, want completed with RECOVERED", end)
	}
	if c.has(contract.CapRetryVisible) {
		if _, ok := h.awaitKind(contract.KindRetrying, in); !ok {
			c.fail("api-error.recovers", "retry_visible, but no retrying observation")
		}
	}
	c.awaitPhase(s, contract.PhaseIdle)
	in = c.send(s, "ERR 529 99")
	end, ok = h.turnEnded(in)
	if !ok || end.Outcome != contract.TurnErrored || end.Error == nil || end.Error.Class != contract.ErrorOverloaded {
		c.fail("api-error.exhausted", "turn_ended %+v, want errored with class overloaded", end)
	}
	if p := c.awaitPhase(s, contract.PhaseIdle); p != contract.PhaseIdle {
		c.fail("api-error.exhausted", "after a transient error the Session is %s, want idle: retries must not block", p)
	}
	in = c.send(s, "PING 3")
	if end, ok := h.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted {
		c.fail("api-error.exhausted", "the next turn: %+v", end)
	}
}

// usageLimit: a turn refused at a usage wall closes the admission gate.
func usageLimit(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "LIMIT")
	end, ok := h.turnEnded(in)
	if !ok || end.Outcome != contract.TurnErrored || end.Error == nil || end.Error.Class != contract.ErrorUsageLimit {
		c.stop("gate.blocked", "turn_ended %+v, want errored with class usage_limit", end)
	}
	if p := c.awaitPhase(s, contract.PhaseBlocked); p != contract.PhaseBlocked {
		c.fail("gate.blocked", "after a usage wall the Session is %s, want blocked", p)
	}
	if b := s.State().Block; b == nil || b.Reason != contract.BlockUsageLimited {
		c.fail("gate.blocked", "state.block %+v, want usage_limited", b)
	}
	ctx, cancel := c.ctx()
	defer cancel()
	refused := newInputID()
	_, err := s.Send(ctx, contract.Text(refused, "PING 4"))
	if codeOf(err) != contract.CodeBlocked || certaintyOf(err) != contract.NotSubmitted {
		c.fail("gate.blocked", "Send past the wall: %v, want blocked, not_submitted", err)
	}
	if _, ok := h.awaitKind(contract.KindBlocked, ""); !ok {
		c.fail("gate.blocked", "no blocked observation")
	}
}

// observe: batches replay until acknowledged, and acks are exact.
func observe(c *check) {
	a := c.newAgent()
	s, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenFresh, "", nil))
	if err != nil {
		c.stop("open.idle", "NewSession: %v", err)
	}
	ctx, cancel := c.ctx()
	defer cancel()
	if _, err := s.Open(ctx); err != nil {
		c.stop("open.idle", "Open: %v", err)
	}
	defer func() { _, _ = s.Close(context.Background(), contract.ClosePark, 0) }()

	// Nothing yet: an empty poll needs no ack. Drain what opening produced.
	for i := 0; i < 20; i++ {
		b, err := s.Observe(ctx, 50*time.Millisecond, contract.MaxObserveBytes)
		if err != nil {
			c.stop("observe.empty-poll", "Observe: %v", err)
		}
		if len(b.Items) == 0 && b.Checkpoint == nil && b.Reset == nil && b.Rescan == nil && len(b.Faults) == 0 {
			if b.NeedsAck() {
				c.fail("observe.empty-poll", "an empty poll carries batch %q", b.BatchID)
			}
			break
		}
		_ = s.Ack(b.BatchID)
	}
	in := c.send(s, "PING 5")
	var first contract.Batch
	for {
		b, err := s.Observe(ctx, time.Second, contract.MaxObserveBytes)
		if err != nil {
			c.stop("observe.replay", "Observe: %v", err)
		}
		if b.NeedsAck() {
			first = b
			break
		}
	}
	again, err := s.Observe(ctx, 0, contract.MaxObserveBytes)
	if err != nil {
		c.stop("observe.replay", "Observe: %v", err)
	}
	if again.BatchID != first.BatchID || !reflect.DeepEqual(again.Items, first.Items) {
		c.fail("observe.replay", "an unacknowledged batch came back as %q with %d items, want %q with %d", again.BatchID, len(again.Items), first.BatchID, len(first.Items))
	}
	var wg sync.WaitGroup
	wg.Add(1)
	var second error
	go func() {
		defer wg.Done()
		_, second = s.Observe(ctx, 0, contract.MaxObserveBytes)
	}()
	wg.Wait()
	_ = second // the outstanding batch returns at once, so no overlap is certain
	if err := s.Ack(first.BatchID); err != nil {
		c.fail("observe.ack", "Ack(%s): %v", first.BatchID, err)
	}
	if err := s.Ack(first.BatchID); err != nil {
		c.fail("observe.ack-replay", "acking the last acknowledged batch again: %v, want nil", err)
	}
	if err := s.Ack("no-such-batch"); codeOf(err) != contract.CodeUnexpected {
		c.fail("observe.ack-unknown", "acking a batch never delivered: %v, want unexpected", err)
	}
	// Drain the turn so it does not leak into the next scenario's timing.
	deadline := time.Now().Add(c.f.timeout())
	for time.Now().Before(deadline) {
		b, err := s.Observe(ctx, 200*time.Millisecond, contract.MaxObserveBytes)
		if err != nil {
			break
		}
		done := false
		for _, o := range b.Items {
			done = done || (o.Kind == contract.KindTurnEnded && o.InputID == in)
		}
		if b.NeedsAck() {
			_ = s.Ack(b.BatchID)
		}
		if done {
			break
		}
	}
}

// observeSize: an observation larger than the bound is refused with the bound
// it needs, never skipped; within it, it is delivered, cut to its bound.
func observeSize(c *check) {
	a := c.newAgent()
	s, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenFresh, "", nil))
	if err != nil {
		c.stop("open.idle", "NewSession: %v", err)
	}
	ctx, cancel := c.ctx()
	defer cancel()
	if _, err := s.Open(ctx); err != nil {
		c.stop("open.idle", "Open: %v", err)
	}
	defer func() { _, _ = s.Close(context.Background(), contract.ClosePark, 0) }()
	in := c.send(s, "BIG 100")
	var tooLarge *contract.Error
	deadline := time.Now().Add(c.f.timeout())
	for time.Now().Before(deadline) {
		b, err := s.Observe(ctx, 200*time.Millisecond, contract.MinObserveBytes)
		if err != nil {
			if asErr(err, &tooLarge) && tooLarge.Code == contract.CodeBatchTooLarge {
				break
			}
			c.stop("observe.too-large", "Observe: %v", err)
		}
		for _, o := range b.Items {
			if o.Kind == contract.KindAssistantText && o.InputID == in {
				c.stop("observe.too-large", "a 100 KiB reply fit a %d-byte bound", contract.MinObserveBytes)
			}
			if o.Kind == contract.KindTurnEnded && o.InputID == in {
				c.stop("observe.too-large", "the turn ended with its 100 KiB reply never delivered: skipped, not refused")
			}
		}
		if b.NeedsAck() {
			_ = s.Ack(b.BatchID)
		}
	}
	if tooLarge == nil {
		c.stop("observe.too-large", "no batch_too_large for a reply over the bound")
	}
	// Each refusal names a bound that delivers the item it refused. The
	// reply's turn_ended, whose text is the same reply cut to its bound, may
	// come first: follow the refusals until the reply itself arrives.
	for found := false; !found; {
		if tooLarge.RequiredBytes <= contract.MinObserveBytes {
			c.fail("observe.too-large", "required_bytes %d does not exceed the bound", tooLarge.RequiredBytes)
		}
		b, err := s.Observe(ctx, time.Second, min(max(tooLarge.RequiredBytes, contract.MinObserveBytes), contract.MaxObserveBytes))
		if err != nil {
			c.stop("observe.too-large", "Observe with the required bound: %v", err)
		}
		if len(b.Items) == 0 {
			c.stop("observe.too-large", "the required bound delivered nothing")
		}
		for _, o := range b.Items {
			if o.Kind == contract.KindAssistantText && o.InputID == in {
				found = true
				var d contract.AssistantTextData
				_ = o.Decode(&d)
				if len(d.Text) > contract.MaxObservationText || !o.Truncated {
					c.fail("observe.truncated", "a %d-byte text, truncated=%v: want it cut to %d and marked", len(d.Text), o.Truncated, contract.MaxObservationText)
				}
			}
		}
		if b.NeedsAck() {
			_ = s.Ack(b.BatchID)
		}
		for !found {
			b, err = s.Observe(ctx, 200*time.Millisecond, contract.MinObserveBytes)
			if asErr(err, &tooLarge) && tooLarge.Code == contract.CodeBatchTooLarge {
				break
			}
			if err != nil {
				c.stop("observe.too-large", "Observe: %v", err)
			}
			for _, o := range b.Items {
				if o.Kind == contract.KindAssistantText && o.InputID == in {
					c.stop("observe.too-large", "a 100 KiB reply fit a %d-byte bound", contract.MinObserveBytes)
				}
			}
			if !b.NeedsAck() && time.Now().After(deadline) {
				c.stop("observe.too-large", "the reply was never delivered")
			}
			if b.NeedsAck() {
				_ = s.Ack(b.BatchID)
			}
		}
	}
}

// recordAfterCrash: a turn that ended but was never committed is delivered
// again from the record, with the same ids, after a crash.
func recordAfterCrash(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	warm := c.send(s, "PING 6")
	if _, ok := h.turnEnded(warm); !ok {
		c.stop("record.redeliver", "no turn_ended for %s", warm)
	}
	c.awaitPhase(s, contract.PhaseIdle)
	time.Sleep(200 * time.Millisecond)
	committed := h.checkpoint()
	h.pause()
	in := c.send(s, "PING 7")
	// The turn ends in the harness, but nothing about it is committed: the
	// Host acknowledges no batch now.
	if p := c.awaitPhase(s, contract.PhaseIdle); p != contract.PhaseIdle {
		c.stop("record.redeliver", "the turn never ended: %s", p)
	}
	time.Sleep(200 * time.Millisecond)
	delivered := h.deliveries(func(o contract.Observation) bool { return o.InputID == in && o.Origin == contract.OriginRecord })
	c.f.Kill(c.t, s)
	h.halt()

	ctx, cancel := c.ctx()
	defer cancel()
	r, err := c.f.Adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: h.id, OpenConfig: a.result.OpenConfig, Layout: a.layout, Checkpoint: committed})
	if err != nil {
		c.stop("record.redeliver", "OpenRecord: %v", err)
	}
	defer func() { _ = r.Close() }()
	rh := c.watch(r)
	for _, o := range delivered {
		if _, ok := rh.await(o.ID, func(x contract.Observation) bool { return x.ID == o.ID }); !ok {
			c.fail("record.redeliver", "record item %s, delivered before the crash and never committed, is not delivered again", o.ID)
		}
	}
	if o, ok := rh.awaitKind(contract.KindTurnEnded, in); !ok || o.Origin != contract.OriginRecord {
		c.fail("record.turn-end", "the record proves the turn ended, but delivers no record-origin turn_ended")
	}
	got, err := r.Recover(ctx, in)
	if err != nil || got.Outcome != contract.RecoveredCompleted {
		c.fail("record.recover", "Recover(%s) = %+v %v, want completed", in, got, err)
	}
	if got, err := r.Recover(ctx, newInputID()); err != nil || got.Outcome != contract.RecoveredNotFound {
		c.fail("record.not-found", "Recover of an input never sent = %+v %v, want not_found", got, err)
	}
}

// recordCrashMidTurn: after a crash mid-turn, Recover never claims the input
// never ran, nor that it completed.
func recordCrashMidTurn(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "STALL 30")
	if _, ok := h.awaitKind(contract.KindUserInput, in); !ok {
		c.stop("record.crash-mid-turn", "the input never reached the record")
	}
	c.f.Kill(c.t, s)
	h.halt()
	ctx, cancel := c.ctx()
	defer cancel()
	r, err := c.f.Adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: h.id, OpenConfig: a.result.OpenConfig, Layout: a.layout, Checkpoint: h.checkpoint()})
	if err != nil {
		c.stop("record.crash-mid-turn", "OpenRecord: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := r.Recover(ctx, in)
	if err != nil {
		c.stop("record.crash-mid-turn", "Recover: %v", err)
	}
	if got.Outcome != contract.RecoveredInterrupted && got.Outcome != contract.RecoveredUnknown {
		c.fail("record.crash-mid-turn", "Recover of a turn cut by a crash = %s, want interrupted or unknown", got.Outcome)
	}
}

// recordNoBinary: the record is read with no harness binary and no
// credential.
func recordNoBinary(c *check) {
	if c.f.HideBinary == nil {
		c.t.Logf("no HideBinary: skipped")
		return
	}
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "PING 8")
	if _, ok := h.turnEnded(in); !ok {
		c.stop("record.no-binary", "no turn_ended")
	}
	ctx, cancel := c.ctx()
	defer cancel()
	_, _ = s.Close(ctx, contract.ClosePark, contract.DefaultDrain)
	h.halt()
	restore := c.f.HideBinary(c.t)
	defer restore()
	if a.cred != nil {
		_ = removeFile(a.cred.File)
	}
	r, err := c.f.Adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: h.id, OpenConfig: a.result.OpenConfig, Layout: a.layout})
	if err != nil {
		c.stop("record.no-binary", "OpenRecord with no binary and no credential: %v", err)
	}
	defer func() { _ = r.Close() }()
	if got, err := r.Recover(ctx, in); err != nil || got.Outcome != contract.RecoveredCompleted {
		c.fail("record.no-binary", "Recover = %+v %v, want completed", got, err)
	}
}

// reopen: a reopened Session continues from its checkpoint, redelivering
// nothing acknowledged.
func reopen(c *check) {
	if !c.has(contract.CapResume) {
		c.t.Logf("no resume: skipped")
		return
	}
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "PING 9")
	if _, ok := h.turnEnded(in); !ok {
		c.stop("reopen.continues", "no turn_ended")
	}
	ctx, cancel := c.ctx()
	defer cancel()
	res, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain)
	if err != nil || !res.Drained {
		c.fail("reopen.continues", "Close = %+v %v, want drained", res, err)
	}
	h.halt()
	acked := h.ids()
	s2, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenReopen, h.id, h.checkpoint()))
	if err != nil {
		c.stop("reopen.continues", "NewSession(reopen): %v", err)
	}
	s2, h2 := c.openWatch(s2, "reopen.continues")
	in2 := c.send(s2, "PING 10")
	if end, ok := h2.turnEnded(in2); !ok || end.Outcome != contract.TurnCompleted {
		c.fail("reopen.continues", "a turn after reopening: %+v", end)
	}
	time.Sleep(200 * time.Millisecond)
	for id, o := range h2.ids() {
		if _, before := acked[id]; before && o.Origin == contract.OriginRecord {
			c.fail("reopen.no-redelivery", "record item %s, acknowledged before, delivered again", id)
		}
	}
}

// rescan: a checkpoint the adapter cannot read makes it read the record from
// its start — with the same ids, so nothing is stored twice.
func rescan(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "PING 11")
	if _, ok := h.turnEnded(in); !ok {
		c.stop("rescan", "no turn_ended")
	}
	ctx, cancel := c.ctx()
	defer cancel()
	_, _ = s.Close(ctx, contract.ClosePark, contract.DefaultDrain)
	h.halt()
	stored := h.ids()
	r, err := c.f.Adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: h.id, OpenConfig: a.result.OpenConfig, Layout: a.layout, Checkpoint: &contract.Checkpoint{Format: 9999, Data: []byte("unreadable")}})
	if err != nil {
		c.stop("rescan", "OpenRecord with an unreadable checkpoint: %v, want a rescan", err)
	}
	defer func() { _ = r.Close() }()
	b, err := r.Observe(ctx, time.Second, contract.MaxObserveBytes)
	if err != nil {
		c.stop("rescan", "Observe: %v", err)
	}
	if b.Rescan == nil {
		c.fail("rescan", "the first batch after an unreadable checkpoint has no rescan")
	}
	for _, o := range b.Items {
		if _, ok := stored[o.ID]; !ok && o.Origin == contract.OriginRecord {
			c.fail("rescan", "a rescan produced %s, an id the Session never delivered: a duplicate fact", o.ID)
		}
	}
}

// closeDrain: Close keeps delivering until the final batches are
// acknowledged, and says so; without acks it says it did not drain.
func closeDrain(c *check) {
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "PING 12")
	if _, ok := h.turnEnded(in); !ok {
		c.stop("close.drained", "no turn_ended")
	}
	ctx, cancel := c.ctx()
	defer cancel()
	res, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain)
	if err != nil || !res.Drained || !res.Stopped {
		c.fail("close.drained", "Close with the Host acking = %+v %v, want stopped and drained", res, err)
	}
	if _, ok := h.awaitKind(contract.KindSessionExited, ""); !ok {
		c.fail("close.drained", "no session_exited before the drain completed")
	}
	again, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain)
	if err != nil || again != res {
		c.fail("close.repeat", "a repeated Close = %+v %v, want %+v", again, err, res)
	}
	if _, err := s.Send(ctx, contract.Text(newInputID(), "PING 13")); certaintyOf(err) != contract.NotSubmitted || (codeOf(err) != contract.CodeClosed && codeOf(err) != contract.CodeExited) {
		c.fail("close.send-after", "Send after Close: %v, want closed or exited, not_submitted", err)
	}

	b := c.newAgent()
	s2, h2 := c.openSession(b)
	h2.pause()
	c.send(s2, "PING 14")
	if p := c.awaitPhase(s2, contract.PhaseIdle); p != contract.PhaseIdle {
		c.stop("close.drained", "the turn never ended: %s", p)
	}
	res, err = s2.Close(ctx, contract.ClosePark, 200*time.Millisecond)
	if err != nil || res.Drained {
		c.fail("close.drained", "Close with nothing acknowledged = %+v %v, want not drained", res, err)
	}
}

// closeStarting: Close during Open ends both.
func closeStarting(c *check) {
	a := c.newAgent()
	s, err := c.f.Adapter.NewSession(c.openRequest(a, contract.OpenFresh, "", nil))
	if err != nil {
		c.stop("close.starting", "NewSession: %v", err)
	}
	ctx, cancel := c.ctx()
	defer cancel()
	opened := make(chan error, 1)
	go func() {
		_, err := s.Open(ctx)
		opened <- err
	}()
	res, err := s.Close(ctx, contract.ClosePark, time.Second)
	if err != nil {
		c.fail("close.starting", "Close during Open: %v", err)
	}
	select {
	case <-opened:
	case <-time.After(c.f.timeout()):
		c.fail("close.starting", "Open never returned after Close: %s", stacks())
	}
	if p := s.State().Phase; p != contract.PhaseExited {
		c.fail("close.starting", "after Close the Session is %s, want exited", p)
	}
	_ = res
}

// prompts: a prompt is answered once, by id.
func prompts(c *check) {
	if !c.has(contract.CapPrompts) {
		c.t.Logf("no prompts: skipped")
		return
	}
	a := c.newAgent()
	s, h := c.openSession(a)
	in := c.send(s, "ASK")
	o, ok := h.awaitKind(contract.KindPromptRaised, in)
	if !ok {
		c.stop("prompt.raised", "no prompt_raised")
	}
	var p contract.PromptInfo
	_ = o.Decode(&p)
	if ph := c.awaitPhase(s, contract.PhaseAwaitingAnswer); ph != contract.PhaseAwaitingAnswer {
		c.fail("prompt.raised", "while a prompt waits the Session is %s, want awaiting_answer", ph)
	}
	ctx, cancel := c.ctx()
	defer cancel()
	if _, err := s.Send(ctx, contract.Text(newInputID(), "PING 15")); codeOf(err) != contract.CodePromptPending || certaintyOf(err) != contract.NotSubmitted {
		c.fail("prompt.pending", "Send while a prompt waits: %v, want prompt_pending, not_submitted", err)
	}
	if err := s.Answer(ctx, p.PromptID, contract.Choice{OptionID: "maybe"}); codeOf(err) != contract.CodeInvalidChoice {
		c.fail("prompt.answer", "an option the prompt lacks: %v, want invalid_choice", err)
	}
	if err := s.Answer(ctx, p.PromptID, contract.Choice{OptionID: "yes"}); err != nil {
		c.fail("prompt.answer", "Answer: %v", err)
	}
	if err := s.Answer(ctx, p.PromptID, contract.Choice{OptionID: "yes"}); err != nil {
		c.fail("prompt.repeat", "the same answer again: %v, want nil", err)
	}
	if err := s.Answer(ctx, p.PromptID, contract.Choice{OptionID: "no"}); codeOf(err) != contract.CodePromptGone {
		c.fail("prompt.gone", "a different answer after one was taken: %v, want prompt_gone", err)
	}
	if end, ok := h.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted {
		c.fail("prompt.answer", "the answered turn: %+v", end)
	}
	if _, ok := h.awaitKind(contract.KindPromptResolved, in); !ok {
		c.fail("prompt.answer", "no prompt_resolved")
	}
}

func removeFile(p string) error { return removeFunc(p) }

// own reports whether o is of a turn the harness started itself: a turn, and
// of no input the scenario sent.
func (c *check) own(o contract.Observation) bool {
	return (o.TurnID != "" || o.Kind == contract.KindTurnStarted || o.Kind == contract.KindTurnEnded) && !c.sent[o.InputID]
}

// ownStarted waits for the start of a turn the harness started itself, other
// than those named.
func (h *host) ownStarted(skip ...string) (contract.Observation, bool) {
	return h.await("a turn of the harness's own", func(o contract.Observation) bool {
		if o.Kind != contract.KindTurnStarted || !h.c.own(o) {
			return false
		}
		for _, id := range skip {
			if o.TurnID == id {
				return false
			}
		}
		return true
	})
}

// ownEnded waits for the end of the turn named, which the harness started
// itself.
func (h *host) ownEnded(turnID string) (contract.TurnEndedData, bool) {
	o, ok := h.await("the end of "+turnID, func(o contract.Observation) bool {
		return o.Kind == contract.KindTurnEnded && o.TurnID == turnID && h.c.own(o)
	})
	var d contract.TurnEndedData
	if !ok || o.Decode(&d) != nil {
		return contract.TurnEndedData{}, false
	}
	return d, true
}

// index is where the first delivery pred holds of came, among all; -1 when
// none did.
func (h *host) index(pred func(contract.Observation) bool) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, o := range h.all {
		if pred(o) {
			return i
		}
	}
	return -1
}

// autonomous: a harness that works by itself — on a goal it was given — has
// its turns reported as turns of no input; an input stops the one that runs
// and is answered by a turn of its own; an interrupt that names one stops it,
// and the harness rests until an input's turn ends.
func autonomous(c *check) {
	if !c.has(contract.CapAutonomousTurns) {
		c.t.Logf("no autonomous_turns: skipped")
		return
	}
	a := c.newAgent()
	s, h := c.openSession(a)
	ctx, cancel := c.ctx()
	defer cancel()

	// The model gives the Session a goal: two turns of work, then done.
	mk := c.send(s, "MKGOAL GOAL 2 40")
	if end, ok := h.turnEnded(mk); !ok || end.Outcome != contract.TurnCompleted {
		c.stop("auto.reported", "the turn that sets the goal: %+v", end)
	}
	first, ok := h.ownStarted()
	if !ok {
		c.stop("auto.reported", "the harness has a goal, and no turn of its own was reported started")
	}
	if st := s.State(); st.Phase != contract.PhaseBusy || st.TurnID != first.TurnID || st.InputID != "" {
		c.fail("auto.reported", "while the harness is on turn %q of its own the state is %+v, want busy, naming that turn and no input", first.TurnID, st)
	}

	// An input does not wait for the harness's own work: that turn yields.
	in := newInputID()
	c.sent[in] = true
	if _, err := s.Send(ctx, contract.Text(in, "PING 31")); err != nil {
		c.stop("auto.send", "Send while the harness is on a turn of its own: %v, want it taken", err)
	}
	if end, ok := h.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted || !strings.Contains(end.Text, "PONG 31") {
		c.fail("auto.send", "the input's turn: %+v, want completed with PONG 31: a turn of its own, not a part of the harness's", end)
	}
	if end, ok := h.ownEnded(first.TurnID); !ok || end.Outcome != contract.TurnInterrupted {
		c.fail("auto.send", "the harness's turn the input stopped ended %+v, want interrupted", end)
	}
	ended := h.index(func(o contract.Observation) bool { return o.Kind == contract.KindTurnEnded && o.TurnID == first.TurnID })
	began := h.index(func(o contract.Observation) bool { return o.Kind == contract.KindTurnStarted && o.InputID == in })
	if ended < 0 || began < 0 || ended > began {
		c.fail("auto.send", "the input's turn started (delivery %d) before the harness's own ended (delivery %d)", began, ended)
	}

	// Its turn over, the harness takes its work up again; an interrupt that
	// names that turn stops it, and the harness rests.
	second, ok := h.ownStarted(first.TurnID)
	if !ok {
		c.stop("auto.reported", "after the input's turn the harness did not take its work up again")
	}
	if out, err := s.Interrupt(ctx, contract.InterruptRequest{InputID: in}); err != nil || out != contract.InterruptTooLate {
		c.fail("auto.interrupt", "interrupting an ended input while the harness is on a turn of its own: %v %v, want too_late", out, err)
	}
	out, err := s.Interrupt(ctx, contract.InterruptRequest{TurnID: second.TurnID, DeadlineMS: 15000})
	if err != nil || out != contract.InterruptStopped {
		c.fail("auto.interrupt", "interrupting turn %s of the harness's own: %v %v, want stopped", second.TurnID, out, err)
	}
	if end, ok := h.ownEnded(second.TurnID); !ok || end.Outcome != contract.TurnInterrupted {
		c.fail("auto.interrupt", "the interrupted turn ended %+v, want interrupted", end)
	}
	if again, err := s.Interrupt(ctx, contract.InterruptRequest{TurnID: second.TurnID}); err != nil || again != out {
		c.fail("auto.interrupt", "a repeated interrupt: %v %v, want the established %v", again, err, out)
	}
	if p := c.awaitPhase(s, contract.PhaseIdle); p != contract.PhaseIdle {
		c.fail("auto.rests", "after the interrupt the Session is %s, want idle", p)
	}
	time.Sleep(c.f.quiet())
	if o, ok := h.ownStartedNow(first.TurnID, second.TurnID); ok {
		c.fail("auto.rests", "the harness started turn %s by itself after an interrupt stopped its work", o.TurnID)
	}
	if p := s.State().Phase; p != contract.PhaseIdle {
		c.fail("auto.rests", "a harness that rests is %s, want idle", p)
	}

	// The next input's turn wakes it, and it finishes its goal.
	in = c.send(s, "PING 32")
	if end, ok := h.turnEnded(in); !ok || end.Outcome != contract.TurnCompleted || !strings.Contains(end.Text, "PONG 32") {
		c.fail("auto.send", "a turn while the harness rests: %+v", end)
	}
	third, ok := h.ownStarted(first.TurnID, second.TurnID)
	if !ok {
		c.stop("auto.reported", "after an input's turn the harness that rested did not take its work up again")
	}
	if end, ok := h.ownEnded(third.TurnID); !ok || end.Outcome != contract.TurnCompleted {
		c.fail("auto.reported", "the harness's last turn ended %+v, want completed", end)
	}
	if p := c.awaitPhase(s, contract.PhaseIdle); p != contract.PhaseIdle {
		c.fail("auto.reported", "its goal complete, the Session is %s, want idle", p)
	}
	time.Sleep(200 * time.Millisecond)

	turns := []string{first.TurnID, second.TurnID, third.TurnID}
	for _, o := range h.deliveries(c.own) {
		switch {
		case o.InputID != "":
			c.fail("auto.ids", "%s of a turn the harness started names input %q", o.Kind, o.InputID)
		case o.TurnID == "":
			c.fail("auto.ids", "%s of a turn the harness started names no turn", o.Kind)
		case (o.Kind == contract.KindTurnStarted || o.Kind == contract.KindTurnEnded) && o.ID != contract.ObservationID(o.Kind, o.TurnID):
			c.fail("auto.ids", "%s id %q, want %q: the turn's id is its key", o.Kind, o.ID, contract.ObservationID(o.Kind, o.TurnID))
		}
	}
	for _, id := range turns {
		if n := len(ids(h.deliveries(func(o contract.Observation) bool { return o.Kind == contract.KindTurnStarted && o.TurnID == id }))); n != 1 {
			c.fail("auto.ids", "%d turn_started ids for turn %s, want 1", n, id)
		}
		outcomes := map[contract.TurnOutcome]bool{}
		for _, o := range h.deliveries(func(o contract.Observation) bool { return o.Kind == contract.KindTurnEnded && o.TurnID == id }) {
			var d contract.TurnEndedData
			_ = o.Decode(&d)
			outcomes[d.Outcome] = true
		}
		if len(outcomes) != 1 {
			c.fail("auto.ids", "turn %s reported %d outcomes", id, len(outcomes))
		}
	}

	// The record proves those turns ended, under the ids their ends had.
	if res, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
		c.fail("auto.record", "Close = %+v %v, want drained", res, err)
	}
	h.halt()
	r, err := c.f.Adapter.OpenRecord(ctx, contract.RecordRequest{SessionID: h.id, OpenConfig: a.result.OpenConfig, Layout: a.layout})
	if err != nil {
		c.stop("auto.record", "OpenRecord: %v", err)
	}
	defer func() { _ = r.Close() }()
	rh := c.watch(r)
	for _, id := range turns {
		want := contract.ObservationID(contract.KindTurnEnded, id)
		if o, ok := rh.await(want, func(o contract.Observation) bool { return o.ID == want }); !ok || o.Origin != contract.OriginRecord || o.InputID != "" {
			c.fail("auto.record", "the record, read alone, does not deliver %s: the end of a turn the harness started", want)
		}
	}
}

// ownStartedNow is a turn the harness started itself, other than those
// named, if one was delivered already.
func (h *host) ownStartedNow(skip ...string) (contract.Observation, bool) {
	i := h.index(func(o contract.Observation) bool {
		if o.Kind != contract.KindTurnStarted || !h.c.own(o) {
			return false
		}
		for _, id := range skip {
			if o.TurnID == id {
				return false
			}
		}
		return true
	})
	if i < 0 {
		return contract.Observation{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.all[i], true
}

// loadAutonomous: work the harness had of its own is saved with the Session,
// and the loaded Session takes it up.
func loadAutonomous(c *check) {
	if !c.has(contract.CapSessionLoad) || !c.has(contract.CapAutonomousTurns) {
		c.t.Logf("no session_load with autonomous_turns: skipped")
		return
	}
	src := c.newAgent()
	s, h := c.openSession(src)
	ctx, cancel := c.ctx()
	defer cancel()
	mk := c.send(s, "MKGOAL GOAL 3 30")
	if end, ok := h.turnEnded(mk); !ok || end.Outcome != contract.TurnCompleted {
		c.stop("load.own-work", "the turn that sets the goal: %+v", end)
	}
	first, ok := h.ownStarted()
	if !ok {
		c.stop("load.own-work", "the harness has a goal, and no turn of its own was reported started")
	}
	// Stopped, the harness rests with its goal unfinished: the state to save.
	if out, err := s.Interrupt(ctx, contract.InterruptRequest{TurnID: first.TurnID, DeadlineMS: 15000}); err != nil || out != contract.InterruptStopped {
		c.stop("load.own-work", "interrupting the harness's own turn: %v %v, want stopped", out, err)
	}
	if p := c.awaitPhase(s, contract.PhaseIdle); p != contract.PhaseIdle {
		c.stop("load.own-work", "after the interrupt the Session is %s, want idle", p)
	}
	if res, err := s.Close(ctx, contract.ClosePark, contract.DefaultDrain); err != nil || !res.Drained {
		c.stop("load.own-work", "Close = %+v %v, want drained", res, err)
	}
	h.halt()
	saved := c.save(src, "load.own-work")
	dst := c.loadAgent(saved, true, "load.provision")
	_, cp := c.seed(dst, h.id, "load.record")

	req := c.openRequest(dst, contract.OpenReopen, h.id, cp)
	req.Loaded = true
	s2, err := c.f.Adapter.NewSession(req)
	if err != nil {
		c.stop("load.open", "NewSession of a loaded Session: %v", err)
	}
	s2, h2 := c.openWatch(s2, "load.open")
	next, ok := h2.ownStarted()
	if !ok {
		c.stop("load.own-work", "the saved Session had a goal it had not finished, and the loaded one starts no turn for it")
	}
	if next.TurnID == first.TurnID {
		c.fail("load.own-work", "the loaded Session's turn has the id of one the saved Session ran: %s", next.TurnID)
	}
	if out, err := s2.Interrupt(ctx, contract.InterruptRequest{TurnID: next.TurnID, DeadlineMS: 15000}); err != nil || out != contract.InterruptStopped {
		c.fail("load.own-work", "interrupting the loaded Session's own turn: %v %v, want stopped", out, err)
	}
}
