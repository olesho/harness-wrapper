package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// TurnID is the turn id of an input's turn: the same in the live Session and
// in every read of its record.
func TurnID(inputID string) string {
	if len(inputID) <= 126 {
		return "t_" + inputID
	}
	sum := sha256.Sum256([]byte(inputID))
	return "t_" + hex.EncodeToString(sum[:16])
}

// AutoTurnID is the turn id of a turn the harness started with no input, from
// the harness's own id for it: the same in the live Session and in every read
// of its record, and never an input's turn id.
func AutoTurnID(native string) string {
	if len(native) <= 126 && contract.ValidID(native) {
		return "a_" + native
	}
	sum := sha256.Sum256([]byte(native))
	return "a_" + hex.EncodeToString(sum[:16])
}

// Truncate cuts a text field to contract.MaxObservationText, at a rune
// boundary, and reports whether it cut.
func Truncate(s string) (string, bool) {
	if len(s) <= contract.MaxObservationText {
		return s, false
	}
	cut := contract.MaxObservationText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

type session struct {
	a   *harnessAdapter
	req contract.OpenRequest
	cur *cursor

	mu        sync.Mutex
	phase     contract.Phase
	t         Transport
	markers   *Markers
	sessionID string
	// lock is the Session's lock while its harness runs, nil once it exited.
	lock     *sessionLock
	instance string
	counter  int
	turns    map[string]*turn // by input id
	byNative map[string]*turn
	current  *turn // the input's turn the harness is on, nil when none
	// auto is the turn the harness started itself and is on, nil when none;
	// autos holds such turns by native id.
	auto       *autoTurn
	autos      map[string]*autoTurn
	sending    bool
	sendDone   chan struct{}
	gate       *contract.Block
	gateTimer  *time.Timer
	resumeHint *time.Time // when a refused account can work again, from its last usage report
	prompt     *contract.PromptInfo
	answered   map[string]contract.Choice
	retry      *contract.RetryInfo
	// background is the work the harness runs in the background now.
	background []contract.BackgroundTask
	exited     chan struct{} // closed once the harness exited
	exitOnce   sync.Once
	closing    bool
	closeDone  chan struct{}
	closeRes   contract.CloseResult
	openCancel context.CancelFunc
	opened     chan struct{} // closed once Open returned, or never started
}

type turn struct {
	inputID, native, turnID string
	started, ended          bool
	outcome                 contract.TurnOutcome
	endCh                   chan struct{}
	interrupt               *interruptOp
	// retries is how many retry notices the turn has published: each takes
	// the next number for its id, since a turn's later model calls retry
	// from attempt 1 again.
	retries int
}

// autoTurn is a turn the harness started with no input.
type autoTurn struct {
	native, turnID string
	started, ended bool
	endCh          chan struct{}
	interrupt      *interruptOp
}

// interruptOp is a turn's one interrupt: repeats join it, and its outcome,
// once established, is every repeat's answer.
type interruptOp struct {
	done    chan struct{}
	outcome contract.InterruptOutcome
}

func newSession(a *harnessAdapter, req contract.OpenRequest) *session {
	return &session{
		a: a, req: req, cur: newCursor(nil, false),
		phase:    contract.PhaseUnopened,
		instance: randomHex(6),
		turns:    map[string]*turn{}, byNative: map[string]*turn{},
		autos:    map[string]*autoTurn{},
		answered: map[string]contract.Choice{},
		exited:   make(chan struct{}),
		opened:   make(chan struct{}),
	}
}

func (s *session) has(c contract.Capability) bool { return s.a.desc.Has(c) }

// ---- Open

func (s *session) Open(ctx context.Context) (contract.OpenResult, error) {
	s.mu.Lock()
	switch {
	case s.closing:
		s.mu.Unlock()
		return contract.OpenResult{}, contract.Errorf(contract.CodeClosed, "the session is closed")
	case s.phase != contract.PhaseUnopened:
		s.mu.Unlock()
		return contract.OpenResult{}, contract.Errorf(contract.CodeUnexpected, "open in %s", s.phase)
	}
	s.phase = contract.PhaseStarting
	octx, cancel := context.WithCancel(ctx)
	s.openCancel = cancel
	s.mu.Unlock()
	defer cancel()
	defer close(s.opened)

	fail := func(err error) (contract.OpenResult, error) {
		s.mu.Lock()
		s.phase = contract.PhaseExited
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return contract.OpenResult{}, contract.Errorf(contract.CodeClosed, "closed while opening")
		}
		var ce *contract.Error
		switch {
		case errors.As(err, &ce):
		case ctx.Err() != nil:
			// The caller abandoned the open: its ctx was cancelled or hit its
			// deadline. That says nothing about the config, so no reason is
			// claimed; the caller owns ctx and knows which it was.
			err = contract.OpenAbandoned(ctx.Err())
		default:
			err = &contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenConfigInvalid, Message: err.Error()}
		}
		return contract.OpenResult{}, err
	}

	m, err := OpenMarkers(s.req.Layout.Scratch)
	if err != nil {
		return fail(err)
	}
	var lk *sessionLock
	if id := s.req.SessionID; id != "" {
		// Before the harness starts: a Session another Host holds is
		// refused, never run twice.
		if lk, err = lockSession(s.req.Layout.Scratch, id); err != nil {
			return fail(err)
		}
	}
	t, err := s.a.p.Start(octx, Start{
		Mode: s.req.Mode, SessionID: s.req.SessionID, OpenConfig: s.req.OpenConfig,
		Layout: s.req.Layout, Credential: s.req.Credential, Loaded: s.req.Loaded, Report: s.report,
	})
	if err != nil {
		lk.release()
		return fail(err)
	}
	stop := func() {
		t.Stop(context.Background(), 0)
		lk.release()
	}
	if lk == nil {
		// The harness chose the id, which no other Host holds yet; held, it
		// is refused to one that reopens it while this harness runs.
		if lk, err = lockSession(s.req.Layout.Scratch, t.SessionID()); err != nil {
			t.Stop(context.Background(), 0)
			return fail(err)
		}
	}
	if s.req.Loaded && t.SessionID() != s.req.SessionID {
		// A loaded Session continues under its saved id, or not at all.
		got := t.SessionID()
		stop()
		return fail(&contract.Error{
			Code: contract.CodeOpenFailed, Reason: contract.OpenSessionNotFound,
			Message: fmt.Sprintf("the harness opened session %q, not the loaded %q", got, s.req.SessionID),
		})
	}
	s.mu.Lock()
	s.t, s.markers, s.sessionID = t, m, t.SessionID()
	select {
	case <-s.exited:
		// The harness ended before Open got this far.
		lk.release()
	default:
		s.lock = lk
	}
	s.mu.Unlock()
	r, err := s.a.p.Record(RecordSource{
		SessionID: t.SessionID(), OpenConfig: s.req.OpenConfig, Layout: s.req.Layout,
		Checkpoint: s.req.Checkpoint, Markers: m,
	})
	if err != nil {
		stop()
		return fail(err)
	}
	s.cur.readerMu.Lock()
	s.cur.reader = r
	s.cur.readerMu.Unlock()

	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return contract.OpenResult{}, contract.Errorf(contract.CodeClosed, "closed while opening")
	}
	if s.phase == contract.PhaseStarting {
		s.phase = contract.PhaseIdle
		if s.auto != nil {
			// The harness took its own work up as it started.
			s.phase = contract.PhaseBusy
		}
	}
	st := s.stateLocked()
	id := s.sessionID
	s.mu.Unlock()
	version := ""
	if v, ok := t.(Versioned); ok {
		version = v.HarnessVersion()
	}
	return contract.OpenResult{SessionID: id, State: st, HarnessVersion: version}, nil
}

// ---- the harness's events

// liveKeyLocked is the key of a live observation that concerns no input: the
// harness process instance, and a counter.
func (s *session) liveKeyLocked() string {
	s.counter++
	return s.instance + "." + strconv.Itoa(s.counter)
}

func (s *session) liveObs(kind contract.Kind, key string, t *turn, at time.Time, data any) contract.Observation {
	o := contract.NewObservation(kind, key, contract.OriginLive, at, data)
	if t != nil {
		o.InputID, o.TurnID = t.inputID, t.turnID
	}
	return o
}

// report takes one event from the transport.
func (s *session) report(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	var obs []contract.Observation
	s.mu.Lock()
	if ev.Auto != "" {
		obs = s.autoLocked(ev)
		s.mu.Unlock()
		s.cur.push(obs...)
		return
	}
	t := s.byNative[ev.Native]
	if t == nil && ev.Native == "" {
		t = s.current
	}
	switch ev.Kind {
	case Started:
		if t == nil || t.started {
			break
		}
		t.started = true
		obs = append(obs, s.liveObs(contract.KindTurnStarted, t.inputID, t, ev.Time, struct{}{}))
		if t == s.current && !t.ended && s.phase == contract.PhaseIdle {
			s.phase = contract.PhaseBusy
		}
	case Ended:
		if t == nil || t.ended {
			break
		}
		obs = append(obs, s.endLocked(t, ev)...)
	case Retrying:
		if t == nil {
			break
		}
		s.retry = &contract.RetryInfo{Attempt: ev.Retry.Attempt, Max: ev.Retry.Max}
		if s.has(contract.CapRetryVisible) {
			t.retries++
			obs = append(obs, s.liveObs(contract.KindRetrying, t.inputID+":"+strconv.Itoa(t.retries), t, ev.Time, ev.Retry))
		}
	case Background:
		if !s.has(contract.CapBackgroundTurns) || s.phase == contract.PhaseExited {
			break
		}
		tasks := append([]contract.BackgroundTask{}, ev.Tasks...)
		s.background = tasks
		obs = append(obs, s.liveObs(contract.KindBackgroundTasks, s.liveKeyLocked(), nil, ev.Time, contract.BackgroundTasksData{Tasks: tasks}))
	case RateLimited:
		if ev.ResumeAt != nil {
			at := *ev.ResumeAt
			s.resumeHint = &at
		}
		if s.has(contract.CapRateLimits) {
			obs = append(obs, s.liveObs(contract.KindRateLimit, s.liveKeyLocked(), t, ev.Time, ev.RateLimit))
		}
	case PromptRaised:
		if ev.Prompt == nil || t == nil || t.ended {
			break
		}
		p := *ev.Prompt
		s.prompt = &p
		if s.phase == contract.PhaseBusy || s.phase == contract.PhaseIdle {
			s.phase = contract.PhaseAwaitingAnswer
		}
		obs = append(obs, s.liveObs(contract.KindPromptRaised, p.PromptID, t, ev.Time, p))
	case PromptResolved:
		if s.prompt == nil || s.prompt.PromptID != ev.PromptID {
			break
		}
		s.prompt = nil
		if s.phase == contract.PhaseAwaitingAnswer {
			s.phase = contract.PhaseBusy
		}
		obs = append(obs, s.liveObs(contract.KindPromptResolved, ev.PromptID, t, ev.Time, contract.PromptResolvedData{PromptID: ev.PromptID, By: ev.By}))
	case Exited:
		s.exitOnce.Do(func() {
			exit := ev.Exit
			exit.Detail, _ = Truncate(exit.Detail)
			obs = append(obs, s.liveObs(contract.KindSessionExited, s.liveKeyLocked(), nil, ev.Time, exit))
			s.phase = contract.PhaseExited
			s.prompt, s.retry, s.background = nil, nil, nil
			if s.gateTimer != nil {
				s.gateTimer.Stop()
			}
			s.lock.release()
			s.lock = nil
			close(s.exited)
		})
	}
	s.mu.Unlock()
	s.cur.push(obs...)
}

// autoLocked takes an event of a turn the harness started itself: its
// turn_started and turn_ended name the turn and no input, and while it runs
// the Session is busy.
func (s *session) autoLocked(ev Event) []contract.Observation {
	if !s.has(contract.CapAutonomousTurns) && !s.has(contract.CapBackgroundTurns) || ev.Kind != Started && ev.Kind != Ended {
		return nil
	}
	at := s.autos[ev.Auto]
	if at == nil {
		if len(s.autos) >= 64 { // the ended ones no interrupt will name again
			for native, old := range s.autos {
				if old.ended {
					delete(s.autos, native)
				}
			}
		}
		at = &autoTurn{native: ev.Auto, turnID: AutoTurnID(ev.Auto), endCh: make(chan struct{})}
		s.autos[ev.Auto] = at
	}
	observe := func(kind contract.Kind, data any) contract.Observation {
		o := contract.NewObservation(kind, at.turnID, contract.OriginLive, ev.Time, data)
		o.TurnID = at.turnID
		return o
	}
	if at.ended || ev.Kind == Started && at.started {
		return nil
	}
	var obs []contract.Observation
	if !at.started {
		at.started = true
		obs = append(obs, observe(contract.KindTurnStarted, struct{}{}))
	}
	if ev.Kind == Started {
		s.auto = at
		if s.phase == contract.PhaseIdle {
			s.phase = contract.PhaseBusy
		}
		return obs
	}
	at.ended = true
	data := contract.TurnEndedData{Outcome: ev.Outcome, Error: ev.Error}
	text, cut := Truncate(ev.Text)
	data.Text = text
	o := observe(contract.KindTurnEnded, data)
	o.Truncated = cut
	obs = append(obs, o)
	close(at.endCh)
	if op := at.interrupt; op != nil && op.outcome == "" {
		op.outcome = interruptOutcome(ev.Outcome)
		close(op.done)
	}
	if s.auto == at {
		s.auto = nil
	}
	s.retry = nil
	if s.phase == contract.PhaseExited || s.current != nil {
		// An input is on its way in, or on the harness: its own turn's end
		// says what the Session is next.
		return obs
	}
	if b := s.blockFor(ev.Error); b != nil {
		s.gate = b
		s.phase = contract.PhaseBlocked
		obs = append(obs, s.liveObs(contract.KindBlocked, s.liveKeyLocked(), nil, ev.Time, *b))
		if b.Reason == contract.BlockUsageLimited && b.ResumeAt != nil {
			s.armGateLocked(*b.ResumeAt)
		}
		return obs
	}
	s.restLocked()
	return obs
}

// restLocked makes a busy Session idle once no turn runs, an input's or the
// harness's own.
func (s *session) restLocked() {
	if s.phase == contract.PhaseBusy && s.current == nil && s.auto == nil {
		s.phase = contract.PhaseIdle
	}
}

// endLocked ends t: its turn_ended, the outcome an interrupt of it
// establishes, and the admission gate when its error prevents the next input.
func (s *session) endLocked(t *turn, ev Event) []contract.Observation {
	var obs []contract.Observation
	if !t.started {
		// A turn ends after it starts: a harness that never said so started
		// it anyway, if only to refuse it.
		t.started = true
		obs = append(obs, s.liveObs(contract.KindTurnStarted, t.inputID, t, ev.Time, struct{}{}))
	}
	t.ended, t.outcome = true, ev.Outcome
	data := contract.TurnEndedData{Outcome: ev.Outcome, Error: ev.Error}
	text, cut := Truncate(ev.Text)
	data.Text = text
	o := s.liveObs(contract.KindTurnEnded, t.inputID, t, ev.Time, data)
	o.Truncated = cut
	obs = append(obs, o)
	close(t.endCh)
	if op := t.interrupt; op != nil && op.outcome == "" {
		op.outcome = interruptOutcome(ev.Outcome)
		close(op.done)
	}
	if s.current == t {
		s.current = nil
	}
	if s.prompt != nil {
		obs = append(obs, s.liveObs(contract.KindPromptResolved, s.prompt.PromptID, t, ev.Time, contract.PromptResolvedData{PromptID: s.prompt.PromptID, By: "harness"}))
		s.prompt = nil
	}
	s.retry = nil
	if s.phase == contract.PhaseExited {
		return obs
	}
	if b := s.blockFor(ev.Error); b != nil {
		s.gate = b
		s.phase = contract.PhaseBlocked
		obs = append(obs, s.liveObs(contract.KindBlocked, s.liveKeyLocked(), nil, ev.Time, *b))
		if b.Reason == contract.BlockUsageLimited && b.ResumeAt != nil {
			s.armGateLocked(*b.ResumeAt)
		}
		return obs
	}
	if s.phase == contract.PhaseAwaitingAnswer {
		s.phase = contract.PhaseBusy
	}
	s.restLocked()
	return obs
}

// interruptOutcome is what an interrupt established once its turn ended so.
func interruptOutcome(o contract.TurnOutcome) contract.InterruptOutcome {
	switch o {
	case contract.TurnInterrupted:
		return contract.InterruptStopped
	case contract.TurnCancelled:
		return contract.InterruptCancelled
	}
	return contract.InterruptTooLate
}

// blockFor is the gate a turn's error closes: a wall the next input would hit
// too. Transient errors close none.
func (s *session) blockFor(e *contract.TurnError) *contract.Block {
	if e == nil {
		return nil
	}
	switch e.Class {
	case contract.ErrorUsageLimit:
		b := &contract.Block{Reason: contract.BlockUsageLimited, ResumeAt: e.ResumeAt}
		if b.ResumeAt == nil && s.resumeHint != nil && s.resumeHint.After(time.Now()) {
			at := *s.resumeHint
			b.ResumeAt = &at
		}
		return b
	case contract.ErrorAuth:
		return &contract.Block{Reason: contract.BlockAuthRequired}
	case contract.ErrorBilling:
		return &contract.Block{Reason: contract.BlockBilling}
	}
	return nil
}

// armGateLocked opens the gate again at resume: a usage wall with a known reset
// lets the caller try again from then; nothing is retried here.
func (s *session) armGateLocked(resume time.Time) {
	if s.gateTimer != nil {
		s.gateTimer.Stop()
	}
	s.gateTimer = time.AfterFunc(time.Until(resume), func() {
		s.mu.Lock()
		if s.phase != contract.PhaseBlocked || s.gate == nil {
			s.mu.Unlock()
			return
		}
		s.gate, s.phase = nil, contract.PhaseIdle
		o := s.liveObs(contract.KindUnblocked, s.liveKeyLocked(), nil, time.Now(), struct{}{})
		s.mu.Unlock()
		s.cur.push(o)
	})
}

// ---- Send

// admitLocked is the admission gate: nil when the Session takes an input now.
func (s *session) admitLocked() error {
	refuse := func(code contract.Code, format string, args ...any) error {
		e := contract.Errorf(code, format, args...)
		e.Certainty = contract.NotSubmitted
		return e
	}
	switch {
	case s.closing:
		return refuse(contract.CodeClosed, "the session is closing")
	case s.sending:
		return refuse(contract.CodeBusy, "a send is outstanding")
	}
	switch s.phase {
	case contract.PhaseIdle:
		return nil
	case contract.PhaseBusy:
		if s.current == nil && s.auto != nil && s.has(contract.CapAutonomousTurns) {
			// The harness is on a turn of its own: Submit stops it first.
			// A turn taking background work up (background_turns) is not
			// cut short: the input waits, as for an input's turn.
			return nil
		}
		return refuse(contract.CodeBusy, "a turn is running")
	case contract.PhaseAwaitingAnswer:
		return refuse(contract.CodePromptPending, "a prompt awaits an answer")
	case contract.PhaseBlocked:
		reason := contract.BlockReason("")
		if s.gate != nil {
			reason = s.gate.Reason
		}
		return refuse(contract.CodeBlocked, "the session is blocked (%s)", reason)
	case contract.PhaseExited:
		return refuse(contract.CodeExited, "the harness exited")
	}
	return refuse(contract.CodeUnexpected, "send in %s", s.phase)
}

// Native ids are the profile's to choose: a Transport that names them
// implements NativeNamer; any other takes a random one.
type NativeNamer interface {
	// NewNative is a fresh native id for an input.
	NewNative(inputID string) string
}

func (s *session) Send(ctx context.Context, in contract.Input) (contract.SendResult, error) {
	if err := in.Validate(s.a.desc.Limits.MaxInputBytes); err != nil {
		return contract.SendResult{}, err
	}
	s.mu.Lock()
	if err := s.admitLocked(); err != nil {
		s.mu.Unlock()
		return contract.SendResult{}, err
	}
	if _, used := s.turns[in.InputID]; used {
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeProtocol, Field: "input_id", Certainty: contract.NotSubmitted, Message: "the input id was already sent"}
	}
	s.sending = true
	s.sendDone = make(chan struct{})
	tr, markers, sessionID := s.t, s.markers, s.sessionID
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sending = false
		close(s.sendDone)
		s.mu.Unlock()
	}()

	native := randomHex(16)
	if nn, ok := tr.(NativeNamer); ok {
		native = nn.NewNative(in.InputID)
	}
	if err := markers.Write(Marker{InputID: in.InputID, Native: native, SessionID: sessionID, At: time.Now().UTC()}); err != nil {
		if errors.Is(err, ErrMarked) {
			return contract.SendResult{}, &contract.Error{Code: contract.CodeProtocol, Field: "input_id", Certainty: contract.NotSubmitted, Message: "the input id was already sent"}
		}
		// Nothing reached the harness, but the marker may be on disk; Recover
		// settles it.
		return contract.SendResult{}, &contract.Error{Code: contract.CodeInternal, Certainty: contract.MaybeSubmitted, Message: err.Error()}
	}
	t := &turn{inputID: in.InputID, native: native, turnID: TurnID(in.InputID), endCh: make(chan struct{})}
	s.mu.Lock()
	s.turns[t.inputID], s.byNative[native] = t, t
	s.current = t
	s.mu.Unlock()

	err := tr.Submit(ctx, Submission{InputID: in.InputID, Native: native, Text: in.JoinText()})
	if err == nil {
		s.mu.Lock()
		if !t.ended && s.phase == contract.PhaseIdle {
			s.phase = contract.PhaseBusy
		}
		s.mu.Unlock()
		return contract.SendResult{Receipt: contract.ReceiptSubmitted, TurnID: t.turnID}, nil
	}
	var ce *contract.Error
	if errors.Is(err, ErrNotSubmitted) || errors.As(err, &ce) && ce.Certainty == contract.NotSubmitted {
		// Nothing reached the harness: the marker is withdrawn, so the input
		// id is free for the Host to send again and no record says it may
		// have run. A marker that stays keeps the id taken.
		freed := markers.Withdraw(in.InputID) == nil
		s.mu.Lock()
		if s.current == t {
			s.current = nil
		}
		delete(s.byNative, native)
		if freed {
			delete(s.turns, in.InputID)
		}
		s.restLocked()
		s.mu.Unlock()
		code := contract.CodeInternal
		if ce != nil {
			code = ce.Code
		}
		return contract.SendResult{}, &contract.Error{Code: code, Certainty: contract.NotSubmitted, Message: err.Error()}
	}
	// The input may run: its turn stays the current one, and the Session
	// takes no other input, until the harness ends it or exits.
	s.mu.Lock()
	if !t.ended && s.phase == contract.PhaseIdle {
		s.phase = contract.PhaseBusy
	}
	s.mu.Unlock()
	code := contract.CodeInternal
	select {
	case <-s.exited:
		code = contract.CodeExited
	default:
	}
	return contract.SendResult{}, &contract.Error{Code: code, Certainty: contract.MaybeSubmitted, Message: err.Error()}
}

// ---- Interrupt

func (s *session) Interrupt(ctx context.Context, req contract.InterruptRequest) (contract.InterruptOutcome, error) {
	if err := req.Validate(); err != nil {
		return "", err
	}
	if req.TurnID != "" && !s.has(contract.CapAutonomousTurns) && !s.has(contract.CapBackgroundTurns) {
		return "", contract.Errorf(contract.CodeUnsupported, "%s starts no turn of its own to name", s.a.desc.Harness.Name)
	}
	deadline := time.NewTimer(req.Deadline())
	defer deadline.Stop()

	// An interrupt never lands inside an outstanding send: it waits for the
	// send's result.
	s.mu.Lock()
	for s.sending {
		done := s.sendDone
		s.mu.Unlock()
		select {
		case <-done:
		case <-deadline.C:
			return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "a send is still outstanding")
		case <-ctx.Done():
			return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "%v", ctx.Err())
		}
		s.mu.Lock()
	}
	switch s.phase {
	case contract.PhaseUnopened, contract.PhaseStarting:
		s.mu.Unlock()
		return "", contract.Errorf(contract.CodeUnexpected, "interrupt in %s", s.phase)
	}
	if req.TurnID != "" {
		return s.interruptAuto(ctx, req, deadline.C) // unlocks
	}
	t := s.turns[req.InputID]
	switch {
	case t == nil && s.current == nil && s.auto == nil:
		s.mu.Unlock()
		return contract.InterruptNoTurn, nil
	case t == nil:
		s.mu.Unlock()
		return contract.InterruptTooLate, nil
	case t.interrupt != nil:
		op := t.interrupt
		s.mu.Unlock()
		return s.awaitInterrupt(ctx, op, deadline.C)
	case t.ended || t != s.current:
		s.mu.Unlock()
		return contract.InterruptTooLate, nil
	}
	if s.phase == contract.PhaseExited {
		s.mu.Unlock()
		return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "the harness exited; the turn's outcome is the record's")
	}
	op := &interruptOp{done: make(chan struct{})}
	t.interrupt = op
	tr := s.t
	s.mu.Unlock()

	ictx, cancel := context.WithTimeout(ctx, req.Deadline())
	err := tr.Interrupt(ictx)
	cancel()
	if err != nil {
		s.mu.Lock()
		if t.interrupt == op && op.outcome == "" {
			t.interrupt = nil // never reached the harness: a repeat asks again
		}
		settled := op.outcome != ""
		s.mu.Unlock()
		if !settled {
			return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "%v", err)
		}
	}
	return s.awaitInterrupt(ctx, op, deadline.C)
}

// interruptAuto stops the turn req names by its turn id, one the harness
// started itself, if it is the turn the harness is on. It is called with s.mu
// held, and releases it.
func (s *session) interruptAuto(ctx context.Context, req contract.InterruptRequest, deadline <-chan time.Time) (contract.InterruptOutcome, error) {
	var at *autoTurn
	for _, a := range s.autos {
		if a.turnID == req.TurnID {
			at = a
		}
	}
	switch {
	case at == nil && s.current == nil && s.auto == nil:
		s.mu.Unlock()
		return contract.InterruptNoTurn, nil
	case at == nil:
		s.mu.Unlock()
		return contract.InterruptTooLate, nil
	case at.interrupt != nil:
		op := at.interrupt
		s.mu.Unlock()
		return s.awaitInterrupt(ctx, op, deadline)
	case at.ended || at != s.auto:
		s.mu.Unlock()
		return contract.InterruptTooLate, nil
	}
	if s.phase == contract.PhaseExited {
		s.mu.Unlock()
		return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "the harness exited; the turn's outcome is the record's")
	}
	tr, ok := s.t.(SelfStarter)
	if !ok {
		s.mu.Unlock()
		return "", contract.Errorf(contract.CodeUnsupported, "%s starts no turn of its own to name", s.a.desc.Harness.Name)
	}
	op := &interruptOp{done: make(chan struct{})}
	at.interrupt = op
	s.mu.Unlock()

	ictx, cancel := context.WithTimeout(ctx, req.Deadline())
	err := tr.InterruptTurn(ictx, at.native)
	cancel()
	if err != nil {
		s.mu.Lock()
		if at.interrupt == op && op.outcome == "" {
			at.interrupt = nil // never reached the harness: a repeat asks again
		}
		settled := op.outcome != ""
		s.mu.Unlock()
		if !settled {
			return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "%v", err)
		}
	}
	return s.awaitInterrupt(ctx, op, deadline)
}

func (s *session) awaitInterrupt(ctx context.Context, op *interruptOp, deadline <-chan time.Time) (contract.InterruptOutcome, error) {
	select {
	case <-op.done:
		s.mu.Lock()
		out := op.outcome
		s.mu.Unlock()
		return out, nil
	case <-s.exited:
		// The harness may have ended the turn as it exited.
		select {
		case <-op.done:
			s.mu.Lock()
			out := op.outcome
			s.mu.Unlock()
			return out, nil
		default:
		}
		return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "the harness exited; the turn's outcome is the record's")
	case <-deadline:
		return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "the turn did not end within the deadline")
	case <-ctx.Done():
		return "", contract.Errorf(contract.CodeInterruptUnconfirmed, "%v", ctx.Err())
	}
}

// ---- Answer

func (s *session) Answer(ctx context.Context, promptID string, c contract.Choice) error {
	if !s.has(contract.CapPrompts) {
		return contract.Errorf(contract.CodeUnsupported, "%s raises no prompts", s.a.desc.Harness.Name)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	if prev, ok := s.answered[promptID]; ok {
		s.mu.Unlock()
		if sameChoice(prev, c) {
			return nil
		}
		return contract.Errorf(contract.CodePromptGone, "prompt %q was answered", promptID)
	}
	p := s.prompt
	if p == nil || p.PromptID != promptID {
		s.mu.Unlock()
		return contract.Errorf(contract.CodePromptGone, "no prompt %q awaits an answer", promptID)
	}
	if !choiceFits(*p, c) {
		s.mu.Unlock()
		return contract.Errorf(contract.CodeInvalidChoice, "the prompt offers no such choice")
	}
	tr := s.t
	s.mu.Unlock()
	if err := tr.Answer(ctx, promptID, c); err != nil {
		var ce *contract.Error
		if errors.As(err, &ce) {
			return ce
		}
		return &contract.Error{Code: contract.CodeInternal, Message: err.Error()}
	}
	s.mu.Lock()
	s.answered[promptID] = c
	s.mu.Unlock()
	return nil
}

func sameChoice(a, b contract.Choice) bool {
	if a.OptionID != b.OptionID || a.Text != b.Text || len(a.OptionIDs) != len(b.OptionIDs) {
		return false
	}
	for i := range a.OptionIDs {
		if a.OptionIDs[i] != b.OptionIDs[i] {
			return false
		}
	}
	return true
}

func choiceFits(p contract.PromptInfo, c contract.Choice) bool {
	has := func(id string) bool {
		for _, o := range p.Options {
			if o.OptionID == id {
				return true
			}
		}
		return false
	}
	switch {
	case c.OptionID != "":
		return has(c.OptionID)
	case len(c.OptionIDs) > 0:
		if !p.MultiSelect {
			return false
		}
		for _, id := range c.OptionIDs {
			if !has(id) {
				return false
			}
		}
		return true
	}
	return len(p.Options) == 0
}

// ---- State, Observe, Ack

func (s *session) stateLocked() contract.State {
	st := contract.State{Phase: s.phase}
	if t := s.current; t != nil && !t.ended && (s.phase == contract.PhaseBusy || s.phase == contract.PhaseAwaitingAnswer) {
		st.TurnID, st.InputID = t.turnID, t.inputID
	}
	if at := s.auto; at != nil && !at.ended && st.TurnID == "" && s.phase == contract.PhaseBusy {
		st.TurnID = at.turnID
	}
	if s.prompt != nil {
		p := *s.prompt
		st.Prompt = &p
	}
	if s.gate != nil {
		b := *s.gate
		st.Block = &b
	}
	if s.retry != nil && s.has(contract.CapRetryVisible) {
		r := *s.retry
		st.Retry = &r
	}
	if len(s.background) > 0 {
		st.Background = append([]contract.BackgroundTask(nil), s.background...)
	}
	return st
}

func (s *session) State() contract.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *session) Observe(ctx context.Context, wait time.Duration, maxBytes int) (contract.Batch, error) {
	s.mu.Lock()
	unopened := s.phase == contract.PhaseUnopened
	s.mu.Unlock()
	if unopened {
		return contract.Batch{}, contract.Errorf(contract.CodeUnexpected, "observe before open")
	}
	return s.cur.observe(ctx, wait, maxBytes)
}

func (s *session) Ack(batchID string) error { return s.cur.ack(batchID) }

// ---- Close

func (s *session) Close(ctx context.Context, reason contract.CloseReason, drain time.Duration) (contract.CloseResult, error) {
	switch reason {
	case contract.ClosePark, contract.CloseDelete, contract.CloseUpgrade, contract.CloseReset:
	default:
		return contract.CloseResult{}, &contract.Error{Code: contract.CodeProtocol, Field: "reason", Message: fmt.Sprintf("%q", reason)}
	}
	if drain < 0 || drain > contract.MaxDrain {
		return contract.CloseResult{}, &contract.Error{Code: contract.CodeProtocol, Field: "drain_ms", Message: "out of range"}
	}
	s.mu.Lock()
	if s.closing {
		done := s.closeDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.closeRes, nil
		case <-ctx.Done():
			return contract.CloseResult{}, contract.Errorf(contract.CodeInternal, "close is still in progress: %v", ctx.Err())
		}
	}
	s.closing = true
	s.closeDone = make(chan struct{})
	phase, cancelOpen := s.phase, s.openCancel
	s.mu.Unlock()

	finish := func(res contract.CloseResult) (contract.CloseResult, error) {
		s.mu.Lock()
		s.phase = contract.PhaseExited
		if s.gateTimer != nil {
			s.gateTimer.Stop()
		}
		s.closeRes = res
		close(s.closeDone)
		s.mu.Unlock()
		s.cur.readerMu.Lock()
		if s.cur.reader != nil {
			_ = s.cur.reader.Close()
		}
		s.cur.readerMu.Unlock()
		return res, nil
	}
	if phase == contract.PhaseUnopened {
		close(s.opened)
		return finish(contract.CloseResult{Stopped: true, Drained: true})
	}
	if phase == contract.PhaseStarting && cancelOpen != nil {
		cancelOpen()
	}
	<-s.opened

	deadline := time.Now().Add(drain)
	s.mu.Lock()
	tr := s.t
	s.mu.Unlock()
	stopped := true
	if tr != nil {
		sctx, cancel := context.WithTimeout(context.Background(), drain+contract.CloseSlack)
		stopped = tr.Stop(sctx, drain)
		cancel()
		select {
		case <-s.exited:
		case <-time.After(time.Until(deadline) + time.Second):
		}
	}
	// Keep delivering until the Host has acknowledged everything, the exit
	// included, and the record holds nothing more — or the drain runs out.
	drained := false
	dctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for {
		exited := tr == nil
		select {
		case <-s.exited:
			exited = true
		default:
		}
		if exited && s.cur.settled(dctx) {
			drained = true
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		s.cur.waitChange(dctx, pollEvery)
	}
	return finish(contract.CloseResult{Stopped: stopped, Drained: drained})
}
