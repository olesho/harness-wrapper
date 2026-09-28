package fakeadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// session is one fake Session: the handle, its cursor, and the harness
// goroutine it drives.
type session struct {
	adapter *Adapter
	req     contract.OpenRequest
	cfg     openConfig
	cursor  *cursor

	// sending is open while a Send is outstanding: an Interrupt or Answer
	// waits for it to close, so it lands after the Send's result.
	sending chan struct{}

	mu        sync.Mutex
	phase     contract.Phase
	store     store
	sessionID string
	proc      int // the harness process instance, for live keys
	counter   int
	turn      *turn
	ended     map[string]contract.InterruptOutcome // inputs whose turns ended: what an interrupt now finds
	block     *contract.Block
	prompt    *contract.PromptInfo
	answered  map[string]string // prompt id → the option it took
	closing   bool
	closed    chan struct{}
	closeRes  *contract.CloseResult
	closeDone chan struct{}
	kill      chan struct{} // closed to crash the harness
	killOnce  sync.Once
	exited    chan struct{}
	opening   bool
	openErr   error
}

// turn is the turn in flight.
type turn struct {
	in        contract.Input
	started   bool // the model call began: an interrupt now stops, never cancels
	output    bool
	interrupt chan struct{}
	intOnce   sync.Once
	outcome   chan contract.InterruptOutcome // the established interrupt outcome
	answer    chan string
}

func newSession(a *Adapter, req contract.OpenRequest, cfg openConfig) *session {
	return &session{
		adapter:   a,
		req:       req,
		cfg:       cfg,
		cursor:    newCursor(a),
		phase:     contract.PhaseUnopened,
		ended:     map[string]contract.InterruptOutcome{},
		answered:  map[string]string{},
		closed:    make(chan struct{}),
		closeDone: make(chan struct{}),
		kill:      make(chan struct{}),
		exited:    make(chan struct{}),
	}
}

// Kill crashes a Session's harness — the process dies without Close — for a
// test's crash scenarios. It is a no-op for a Session not from this package.
func Kill(s contract.Session) {
	if fs, ok := s.(*session); ok {
		fs.killOnce.Do(func() { close(fs.kill) })
	}
}

func (s *session) Open(ctx context.Context) (contract.OpenResult, error) {
	s.mu.Lock()
	if s.phase != contract.PhaseUnopened || s.opening {
		s.mu.Unlock()
		return contract.OpenResult{}, contract.Errorf(contract.CodeUnexpected, "open in phase %s", s.phase)
	}
	if s.closing {
		s.mu.Unlock()
		return contract.OpenResult{}, &contract.Error{Code: contract.CodeClosed}
	}
	s.opening = true
	s.phase = contract.PhaseStarting
	s.mu.Unlock()

	fail := func(err error) (contract.OpenResult, error) {
		s.mu.Lock()
		s.phase = contract.PhaseExited
		s.opening = false
		s.openErr = err
		s.mu.Unlock()
		s.killOnce.Do(func() { close(s.kill) })
		close(s.exited)
		return contract.OpenResult{}, err
	}
	if d := s.adapter.opts.StartDelay; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-s.closed:
			return fail(&contract.Error{Code: contract.CodeClosed})
		}
	}
	if _, err := os.Stat(s.cfg.Binary); err != nil {
		return fail(&contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenBinaryNotFound, Message: err.Error()})
	}
	if c := s.req.Credential; c == nil || c.Kind != CredentialKind {
		return fail(&contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenAuthRequired, Message: "no " + CredentialKind})
	} else if b, err := os.ReadFile(c.File); err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return fail(&contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenAuthRequired, Message: "the credential file is missing or empty"})
	}

	id := s.req.SessionID
	if id == "" {
		id = "fake-" + randomHex(8)
	}
	st := store{dir: sessionDir(s.req.Layout, id), adapter: s.adapter}
	switch s.req.Mode {
	case contract.OpenFresh:
		if _, err := os.Stat(st.dir); err == nil {
			return fail(&contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenSessionInUse, Message: id})
		}
		if err := st.create(); err != nil {
			return fail(contract.Errorf(contract.CodeInternal, "%v", err))
		}
	case contract.OpenReopen:
		if _, err := os.Stat(st.recordPath()); err != nil {
			return fail(&contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenSessionNotFound, Message: id})
		}
		if err := s.cursor.load(st, s.req.Checkpoint); err != nil {
			return fail(err)
		}
	}
	select {
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-s.closed:
		return fail(&contract.Error{Code: contract.CodeClosed})
	default:
	}

	s.mu.Lock()
	s.store = st
	s.sessionID = id
	s.proc = int(time.Now().UnixNano() % 1e9)
	s.phase = contract.PhaseIdle
	s.opening = false
	state := s.stateLocked()
	s.mu.Unlock()
	go s.watchKill()
	return contract.OpenResult{SessionID: id, State: state}, nil
}

// watchKill turns a crash into the Session's exit.
func (s *session) watchKill() {
	<-s.kill
	s.exit(contract.ExitCrashed, "killed without close")
}

// exit ends the harness process: the Session is exited, and a
// session_exited observation reports how, once.
func (s *session) exit(class contract.ExitClass, detail string) {
	s.mu.Lock()
	if s.phase == contract.PhaseExited {
		s.mu.Unlock()
		return
	}
	s.phase = contract.PhaseExited
	t := s.turn
	s.turn = nil
	s.counter++
	key := fmt.Sprintf("%d.%d", s.proc, s.counter)
	s.mu.Unlock()
	if t != nil {
		t.intOnce.Do(func() { close(t.interrupt) })
		select {
		case t.outcome <- "":
		default:
		}
	}
	if !s.adapter.breaks("no-session-exited") {
		s.cursor.push(item{obs: contract.NewObservation(contract.KindSessionExited, key, contract.OriginLive, time.Now(),
			contract.SessionExitedData{Class: class, Detail: detail})})
	}
	close(s.exited)
}

func (s *session) stateLocked() contract.State {
	st := contract.State{Phase: s.phase, Block: s.block, Prompt: s.prompt}
	if s.turn != nil {
		st.InputID = s.turn.in.InputID
		st.TurnID = turnID(s.turn.in.InputID)
	}
	return st
}

func turnID(inputID string) string { return "t_" + inputID }

func (s *session) State() contract.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *session) Send(ctx context.Context, in contract.Input) (contract.SendResult, error) {
	s.mu.Lock()
	if s.sending != nil {
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeBusy, Certainty: s.refusal(), Message: "a send is outstanding"}
	}
	sending := make(chan struct{})
	s.sending = sending
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sending = nil
		s.mu.Unlock()
		close(sending)
	}()
	s.mu.Lock()
	switch {
	case s.closing && !s.adapter.breaks("send-after-close"):
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeClosed, Certainty: contract.NotSubmitted}
	case s.closing:
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeExited, Certainty: contract.MaybeSubmitted}
	case s.phase == contract.PhaseExited:
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted}
	case s.phase == contract.PhaseBusy:
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeBusy, Certainty: s.refusal()}
	case s.phase == contract.PhaseAwaitingAnswer:
		s.mu.Unlock()
		if s.adapter.breaks("prompt-pending-maybe") {
			return contract.SendResult{}, &contract.Error{Code: contract.CodePromptPending, Certainty: contract.MaybeSubmitted}
		}
		return contract.SendResult{}, &contract.Error{Code: contract.CodePromptPending, Certainty: contract.NotSubmitted}
	case s.phase == contract.PhaseBlocked && !s.adapter.breaks("open-gate"):
		if s.block.ResumeAt == nil || time.Now().Before(*s.block.ResumeAt) {
			s.mu.Unlock()
			return contract.SendResult{}, &contract.Error{Code: contract.CodeBlocked, Certainty: contract.NotSubmitted, Message: string(s.block.Reason)}
		}
		s.unblockLocked()
	case s.phase != contract.PhaseIdle && s.phase != contract.PhaseBlocked:
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeUnexpected, Certainty: contract.NotSubmitted, Message: string(s.phase)}
	}
	if err := in.Validate(s.adapter.Describe().Limits.MaxInputBytes); err != nil {
		s.mu.Unlock()
		return contract.SendResult{}, err
	}
	if _, seen := s.ended[in.InputID]; seen {
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeProtocol, Certainty: contract.NotSubmitted, Message: "input id " + in.InputID + " was sent before"}
	}
	st := s.store
	s.mu.Unlock()

	// The marker is durable before anything reaches the harness.
	if err := st.mark(in.InputID); err != nil {
		return contract.SendResult{}, &contract.Error{Code: contract.CodeInternal, Certainty: contract.NotSubmitted, Message: err.Error()}
	}
	// Handed over: the harness records the input, and from here a failure is
	// maybe_submitted.
	if _, err := s.record(contract.NewObservation(contract.KindUserInput, "u-"+in.InputID, contract.OriginRecord, time.Now(),
		contract.TextData{Text: in.JoinText()}), in.InputID); err != nil {
		return contract.SendResult{}, &contract.Error{Code: contract.CodeInternal, Certainty: contract.MaybeSubmitted, Message: err.Error()}
	}
	t := &turn{in: in, interrupt: make(chan struct{}), outcome: make(chan contract.InterruptOutcome, 1), answer: make(chan string, 1)}
	s.mu.Lock()
	if s.phase == contract.PhaseExited {
		s.mu.Unlock()
		return contract.SendResult{}, &contract.Error{Code: contract.CodeExited, Certainty: contract.MaybeSubmitted}
	}
	s.turn = t
	s.phase = contract.PhaseBusy
	s.mu.Unlock()
	if !s.adapter.breaks("no-turn-started") {
		s.live(contract.KindTurnStarted, in.InputID, in.InputID, struct{}{})
	}
	go s.run(t)
	return contract.SendResult{Receipt: contract.ReceiptSubmitted, TurnID: turnID(in.InputID)}, nil
}

// refusal is a busy refusal's certainty: not_submitted, unless broken.
func (s *session) refusal() contract.Certainty {
	if s.adapter.breaks("busy-maybe-submitted") {
		return contract.MaybeSubmitted
	}
	return contract.NotSubmitted
}

// record appends a record observation and queues it for delivery.
func (s *session) record(o contract.Observation, inputID string) (int64, error) {
	o.InputID = inputID
	if inputID != "" {
		o.TurnID = turnID(inputID)
	}
	s.mu.Lock()
	st := s.store
	s.mu.Unlock()
	end, err := st.appendRecord(o)
	if err != nil {
		return 0, err
	}
	s.cursor.push(item{obs: o, end: end})
	return end, nil
}

// live queues a live observation.
func (s *session) live(kind contract.Kind, key, inputID string, data any) {
	o := contract.NewObservation(kind, key, contract.OriginLive, time.Now(), data)
	o.InputID = inputID
	if inputID != "" {
		o.TurnID = turnID(inputID)
	}
	s.cursor.push(item{obs: o})
}

// liveKey is a key for a live observation of the harness process instance.
func (s *session) liveKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counter++
	return fmt.Sprintf("%d.%d", s.proc, s.counter)
}

// run is the fake harness answering one input.
func (s *session) run(t *turn) {
	tick := s.adapter.opts.Tick
	in := t.in.InputID
	words := strings.Fields(t.in.JoinText())
	cmd, arg := "", ""
	if len(words) > 0 {
		cmd = strings.ToUpper(words[0])
	}
	if len(words) > 1 {
		arg = strings.Join(words[1:], " ")
	}

	// One tick queued before the model call starts: an interrupt then
	// withdraws the input, and that is positive evidence of cancellation.
	if s.pause(t, tick) {
		s.end(t, contract.TurnCancelled, "", nil, contract.InterruptCancelled)
		return
	}
	s.mu.Lock()
	t.started = true
	s.mu.Unlock()

	msg := "m-" + in
	reply := func(text string) {
		s.assistant(t, msg, text)
		s.end(t, contract.TurnCompleted, text, nil, "")
	}
	switch cmd {
	case "PING":
		reply(strings.TrimSpace("PONG " + arg))
	case "SLOW":
		n := atoi(arg, 40)
		var text strings.Builder
		for i := 0; i < n; i++ {
			if s.pause(t, tick) {
				s.stopped(t, msg, text.String())
				return
			}
			text.WriteString("slow")
			s.mu.Lock()
			t.output = true
			s.mu.Unlock()
			s.live(contract.KindTextDelta, fmt.Sprintf("%s.%d", msg, i), in, contract.TextDeltaData{MessageID: msg, Index: i, Text: "slow", Final: i == n-1})
		}
		reply(text.String())
	case "STALL":
		if s.pause(t, time.Duration(atoi(arg, 60))*time.Second) {
			s.stopped(t, msg, "")
			return
		}
		reply("ok")
	case "TOOL":
		id := "tu-" + in
		input, _ := json.Marshal(map[string]string{"command": arg})
		s.mu.Lock()
		t.output = true
		s.mu.Unlock()
		_, _ = s.record(contract.NewObservation(contract.KindToolUse, id, contract.OriginRecord, time.Now(), contract.ToolUseData{ToolUseID: id, Name: "Bash", Input: input}), in)
		s.live(contract.KindToolStarted, id, in, contract.ToolUseData{ToolUseID: id, Name: "Bash", Input: input})
		if s.pause(t, 5*tick) {
			s.live(contract.KindToolFinished, id, in, contract.ToolFinishedData{ToolUseID: id, Name: "Bash", Failed: true})
			s.stopped(t, msg, "")
			return
		}
		_, _ = s.record(contract.NewObservation(contract.KindToolResult, id, contract.OriginRecord, time.Now(), contract.ToolResultData{ToolUseID: id, Output: "ran " + arg}), in)
		s.live(contract.KindToolFinished, id, in, contract.ToolFinishedData{ToolUseID: id, Name: "Bash", Output: "ran " + arg})
		reply("TOOL DONE")
	case "ERR":
		f := strings.Fields(arg)
		code, times := 529, 1
		if len(f) > 0 {
			code = atoi(f[0], 529)
		}
		if len(f) > 1 {
			times = atoi(f[1], 1)
		}
		for attempt := 1; attempt <= times; attempt++ {
			if attempt > MaxRetries {
				s.fail(t, code)
				return
			}
			if !s.adapter.breaks("no-retrying") {
				s.live(contract.KindRetrying, fmt.Sprintf("%s.%d", in, attempt), in, contract.RetryingData{Attempt: attempt, Max: MaxRetries, DelayMS: int(tick / time.Millisecond), HTTPStatus: code})
			}
			if s.pause(t, tick) {
				s.stopped(t, msg, "")
				return
			}
		}
		reply("RECOVERED")
	case "BIG":
		reply(strings.Repeat("x", atoi(arg, 64)<<10))
	case "ASK":
		p := &contract.PromptInfo{PromptID: "p-" + in, Kind: "confirm", Prompt: "Proceed?", Options: []contract.PromptOption{{OptionID: "yes", Label: "Yes"}, {OptionID: "no", Label: "No"}}}
		s.mu.Lock()
		s.prompt = p
		s.phase = contract.PhaseAwaitingAnswer
		s.mu.Unlock()
		s.live(contract.KindPromptRaised, p.PromptID, in, p)
		select {
		case choice := <-t.answer:
			s.live(contract.KindPromptResolved, p.PromptID, in, contract.PromptResolvedData{PromptID: p.PromptID, By: "answer"})
			s.mu.Lock()
			s.prompt = nil
			if s.phase == contract.PhaseAwaitingAnswer {
				s.phase = contract.PhaseBusy
			}
			s.mu.Unlock()
			reply("ANSWERED " + choice)
		case <-t.interrupt:
			s.live(contract.KindPromptResolved, p.PromptID, in, contract.PromptResolvedData{PromptID: p.PromptID, By: "harness"})
			s.mu.Lock()
			s.prompt = nil
			s.mu.Unlock()
			s.stopped(t, msg, "")
		}
	case "CRASH":
		s.killOnce.Do(func() { close(s.kill) })
	default:
		reply("ok")
	}
}

// pause waits d, and reports whether the turn was interrupted meanwhile.
func (s *session) pause(t *turn, d time.Duration) bool {
	select {
	case <-t.interrupt:
		return true
	case <-time.After(d):
		return false
	}
}

func (s *session) assistant(t *turn, msg, text string) {
	cut := truncate(text)
	if s.adapter.breaks("no-truncate") {
		cut = text
	}
	o := contract.NewObservation(contract.KindAssistantText, msg+".0", contract.OriginRecord, time.Now(), contract.AssistantTextData{MessageID: msg, Text: cut})
	o.Truncated = len(cut) < len(text)
	_, _ = s.record(o, t.in.InputID)
}

func truncate(s string) string {
	if len(s) > contract.MaxObservationText {
		return s[:contract.MaxObservationText]
	}
	return s
}

// stopped ends an interrupted turn: its partial reply, then its end.
func (s *session) stopped(t *turn, msg, text string) {
	if text != "" {
		s.assistant(t, msg, text)
	}
	s.end(t, contract.TurnInterrupted, text, nil, contract.InterruptStopped)
}

// fail ends a turn whose model call failed for good, and closes the gate on a
// wall that prevents the next input.
func (s *session) fail(t *turn, code int) {
	class, block := contract.ErrorOverloaded, contract.BlockReason("")
	if s.adapter.breaks("block-on-overloaded") {
		block = contract.BlockUsageLimited
	}
	var resume *time.Time
	switch code {
	case 429:
		class, block = contract.ErrorUsageLimit, contract.BlockUsageLimited
		at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
		resume = &at
	case 401:
		class, block = contract.ErrorAuth, contract.BlockAuthRequired
	case 402:
		class, block = contract.ErrorBilling, contract.BlockBilling
	}
	_, _ = s.record(contract.NewObservation(contract.KindAPIError, "e-"+t.in.InputID, contract.OriginRecord, time.Now(), contract.APIErrorData{Class: class, HTTPStatus: code}), t.in.InputID)
	s.end(t, contract.TurnErrored, "", &contract.TurnError{Class: class, HTTPStatus: code, ResumeAt: resume}, "")
	if block != "" {
		s.mu.Lock()
		if s.phase == contract.PhaseIdle {
			s.block = &contract.Block{Reason: block, ResumeAt: resume}
			s.phase = contract.PhaseBlocked
		}
		s.mu.Unlock()
		s.live(contract.KindBlocked, s.liveKey(), "", contract.Block{Reason: block, ResumeAt: resume})
	}
}

// unblockLocked opens the gate again once a known resume time passed.
func (s *session) unblockLocked() {
	s.block = nil
	s.phase = contract.PhaseIdle
	s.counter++
	key := fmt.Sprintf("%d.%d", s.proc, s.counter)
	go s.live(contract.KindUnblocked, key, "", struct{}{})
}

// end settles a turn: its end, live and in the record, and the outcome an
// interrupt of it establishes.
func (s *session) end(t *turn, outcome contract.TurnOutcome, text string, terr *contract.TurnError, intOutcome contract.InterruptOutcome) {
	in := t.in.InputID
	data := contract.TurnEndedData{Outcome: outcome, Text: truncate(text), Error: terr}
	s.mu.Lock()
	if s.turn != t || s.phase == contract.PhaseExited {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.live(contract.KindTurnEnded, in, in, data)
	if s.adapter.breaks("two-outcomes") && outcome == contract.TurnCompleted {
		data.Outcome = contract.TurnInterrupted
	}
	if !s.adapter.breaks("no-record-turn-end") {
		_, _ = s.record(contract.NewObservation(contract.KindTurnEnded, in, contract.OriginRecord, time.Now(), data), in)
	}
	s.mu.Lock()
	if s.turn == t {
		s.turn = nil
		if s.phase != contract.PhaseExited {
			s.phase = contract.PhaseIdle
		}
	}
	if intOutcome == "" {
		intOutcome = contract.InterruptTooLate
	}
	s.ended[in] = intOutcome
	s.mu.Unlock()
	select {
	case t.outcome <- intOutcome:
	default:
	}
}

// afterSend waits for an outstanding Send's result: an Interrupt or Answer
// never lands inside one.
func (s *session) afterSend() {
	s.mu.Lock()
	sending := s.sending
	s.mu.Unlock()
	if sending != nil {
		<-sending
	}
}

func (s *session) Interrupt(ctx context.Context, req contract.InterruptRequest) (contract.InterruptOutcome, error) {
	if !contract.ValidID(req.InputID) {
		return "", &contract.Error{Code: contract.CodeProtocol, Field: "input_id"}
	}
	s.afterSend()
	s.mu.Lock()
	if s.phase == contract.PhaseUnopened || s.phase == contract.PhaseStarting {
		s.mu.Unlock()
		return "", contract.Errorf(contract.CodeUnexpected, "interrupt in phase %s", s.phase)
	}
	t := s.turn
	if outcome, done := s.ended[req.InputID]; done && (!s.adapter.breaks("interrupt-other-turn") || t == nil) {
		s.mu.Unlock()
		if s.adapter.breaks("interrupt-twice-differs") && outcome != contract.InterruptTooLate {
			return contract.InterruptTooLate, nil
		}
		return outcome, nil
	}
	if t == nil {
		s.mu.Unlock()
		if s.adapter.breaks("idle-too-late") {
			return contract.InterruptTooLate, nil
		}
		return contract.InterruptNoTurn, nil
	}
	if t.in.InputID != req.InputID && !s.adapter.breaks("interrupt-other-turn") {
		s.mu.Unlock()
		return contract.InterruptTooLate, nil
	}
	s.mu.Unlock()
	t.intOnce.Do(func() { close(t.interrupt) })
	deadline := time.NewTimer(req.Deadline())
	defer deadline.Stop()
	select {
	case outcome := <-t.outcome:
		if outcome == "" {
			return "", &contract.Error{Code: contract.CodeExited}
		}
		t.outcome <- outcome // for a repeated interrupt racing this one
		return outcome, nil
	case <-deadline.C:
		return "", &contract.Error{Code: contract.CodeInterruptUnconfirmed}
	case <-ctx.Done():
		return "", &contract.Error{Code: contract.CodeInterruptUnconfirmed, Message: ctx.Err().Error()}
	}
}

func (s *session) Answer(_ context.Context, promptID string, c contract.Choice) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.afterSend()
	s.mu.Lock()
	if took, done := s.answered[promptID]; done {
		s.mu.Unlock()
		if took == c.OptionID || s.adapter.breaks("answer-after-taken") {
			return nil
		}
		return &contract.Error{Code: contract.CodePromptGone, Message: "answered already"}
	}
	p, t := s.prompt, s.turn
	if p == nil || p.PromptID != promptID || t == nil {
		s.mu.Unlock()
		return &contract.Error{Code: contract.CodePromptGone}
	}
	if c.OptionID != "yes" && c.OptionID != "no" {
		s.mu.Unlock()
		return &contract.Error{Code: contract.CodeInvalidChoice, Message: "want yes or no"}
	}
	s.answered[promptID] = c.OptionID
	s.mu.Unlock()
	t.answer <- c.OptionID
	return nil
}

func (s *session) Observe(ctx context.Context, wait time.Duration, maxBytes int) (contract.Batch, error) {
	s.mu.Lock()
	phase, st := s.phase, s.store
	s.mu.Unlock()
	if phase == contract.PhaseUnopened {
		return contract.Batch{}, contract.Errorf(contract.CodeUnexpected, "observe before open")
	}
	return s.cursor.observe(ctx, wait, maxBytes, st, false, nil)
}

func (s *session) Ack(batchID string) error { return s.cursor.ack(batchID) }

func (s *session) Close(ctx context.Context, reason contract.CloseReason, drain time.Duration) (contract.CloseResult, error) {
	switch reason {
	case contract.ClosePark, contract.CloseDelete, contract.CloseUpgrade, contract.CloseReset:
	default:
		return contract.CloseResult{}, &contract.Error{Code: contract.CodeProtocol, Field: "reason", Message: string(reason)}
	}
	if drain < 0 || drain > contract.MaxDrain {
		return contract.CloseResult{}, &contract.Error{Code: contract.CodeProtocol, Field: "drain_ms"}
	}
	s.mu.Lock()
	first := !s.closing
	s.closing = true
	opened := s.phase != contract.PhaseUnopened
	s.mu.Unlock()
	if !first {
		select {
		case <-s.closeDone:
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.adapter.breaks("close-differs") {
				return contract.CloseResult{Stopped: !s.closeRes.Stopped, Drained: s.closeRes.Drained}, nil
			}
			return *s.closeRes, nil
		case <-ctx.Done():
			return contract.CloseResult{}, ctx.Err()
		}
	}
	close(s.closed)
	if !opened {
		s.mu.Lock()
		s.phase = contract.PhaseExited
		s.closeRes = &contract.CloseResult{Stopped: true, Drained: true}
		s.mu.Unlock()
		close(s.closeDone)
		return *s.closeRes, nil
	}
	// TERM the harness; a turn in flight ends with the process.
	s.exit(contract.ExitClean, "closed: "+string(reason))
	dctx, cancel := context.WithTimeout(ctx, drain)
	defer cancel()
	drained := s.cursor.waitDrained(dctx)
	if s.adapter.breaks("drained-without-acks") {
		drained = true
	}
	s.mu.Lock()
	s.closeRes = &contract.CloseResult{Stopped: true, Drained: drained}
	res := *s.closeRes
	s.mu.Unlock()
	close(s.closeDone)
	return res, nil
}

// ---- the record handle

type record struct {
	adapter *Adapter
	store   store
	cursor  *cursor
}

func (r *record) Observe(ctx context.Context, wait time.Duration, maxBytes int) (contract.Batch, error) {
	return r.cursor.observe(ctx, wait, maxBytes, r.store, true, nil)
}

func (r *record) Ack(batchID string) error { return r.cursor.ack(batchID) }

// Recover answers from the marker and the record. The fake's record proves a
// turn's end durably before the turn is reported ended, so an input the
// record holds with no end was cut short: interrupted.
func (r *record) Recover(_ context.Context, inputID string) (contract.Recovered, error) {
	marked, err := r.store.marked(inputID)
	if err != nil {
		return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
	}
	if !marked {
		if r.adapter.breaks("unsent-unknown") {
			return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
		}
		return contract.Recovered{Outcome: contract.RecoveredNotFound}, nil
	}
	lines, _, err := r.store.readRecord(0)
	if err != nil {
		return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
	}
	found := false
	for _, l := range lines {
		if l.obs.InputID != inputID {
			continue
		}
		switch l.obs.Kind {
		case contract.KindUserInput:
			found = true
		case contract.KindTurnEnded:
			var d contract.TurnEndedData
			if l.obs.Decode(&d) == nil {
				return contract.Recovered{Outcome: contract.RecoveredOutcome(d.Outcome), TurnID: turnID(inputID)}, nil
			}
		}
	}
	if found {
		if r.adapter.breaks("recover-not-found") {
			return contract.Recovered{Outcome: contract.RecoveredNotFound}, nil
		}
		return contract.Recovered{Outcome: contract.RecoveredInterrupted, TurnID: turnID(inputID)}, nil
	}
	return contract.Recovered{Outcome: contract.RecoveredUnknown}, nil
}

func (r *record) Close() error { return nil }

// ---- helpers

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func atoi(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 {
		return def
	}
	return n
}
