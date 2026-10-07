package fakeadapter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
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
	// auto is the turn the harness started itself and is on; autoEnded, what
	// an interrupt of an ended one finds, by turn id. rest: a turn did not
	// complete, and the harness starts none of its own until an input's turn
	// does.
	auto      *autoTurn
	autoEnded map[string]contract.InterruptOutcome
	rest      bool
	// background is the work the harness runs in the background
	// (background_turns); taken, the work that ended, each awaiting the
	// turn of the harness's own that takes its result up.
	background []contract.BackgroundTask
	taken      []contract.BackgroundTask
	// heard is the conversation the model was last given.
	heard []string
	// loadedAt is, for a loaded Session, where its record ended when it
	// opened.
	loadedAt  int64
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
	// beforeIdle, when set, runs in Open once the Session's directory is
	// held and before it turns idle: a test's way into that window.
	beforeIdle func()
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

// autoTurn is a turn the harness started by itself: for its goal, or to take
// up the result of work it ran in the background.
type autoTurn struct {
	id string // its turn id
	// bg is the background work the turn takes up; nil for a goal's turn.
	bg        *contract.BackgroundTask
	interrupt chan struct{}
	intOnce   sync.Once
	done      chan struct{} // closed once it ended
	doneOnce  sync.Once
	outcome   contract.InterruptOutcome
	// preempted: an input stopped it, and that input's turn follows.
	preempted bool
}

// stop asks the turn to stop; finish marks it ended.
func (at *autoTurn) stop()   { at.intOnce.Do(func() { close(at.interrupt) }) }
func (at *autoTurn) finish() { at.doneOnce.Do(func() { close(at.done) }) }

func newSession(a *Adapter, req contract.OpenRequest, cfg openConfig) *session {
	return &session{
		adapter:   a,
		req:       req,
		cfg:       cfg,
		cursor:    newCursor(a),
		phase:     contract.PhaseUnopened,
		autoEnded: map[string]contract.InterruptOutcome{},
		ended:     map[string]contract.InterruptOutcome{},
		answered:  map[string]string{},
		closed:    make(chan struct{}),
		closeDone: make(chan struct{}),
		kill:      make(chan struct{}),
		exited:    make(chan struct{}),
	}
}

// Heard is the conversation the Session's harness last gave its model: every
// prompt, reply and tool result, in order, the input it was answering last.
// It is what a real harness's model API would have been sent.
func Heard(s contract.Session) []string {
	fs, ok := s.(*session)
	if !ok {
		return nil
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]string(nil), fs.heard...)
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
		// A Close during the start may have ended the Session already: exit
		// then signals it, once.
		exited := s.phase == contract.PhaseExited
		s.phase = contract.PhaseExited
		s.opening = false
		s.openErr = err
		s.mu.Unlock()
		s.killOnce.Do(func() { close(s.kill) })
		if !exited {
			close(s.exited)
		}
		return contract.OpenResult{}, err
	}
	crowded, started := s.starting()
	defer started()
	if d := s.adapter.opts.StartDelay; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-s.closed:
			return fail(&contract.Error{Code: contract.CodeClosed})
		}
	}
	if s.adapter.breaks("open-race") && crowded() {
		// As codex's app-servers started together on a fresh home do
		// (openai/codex#50290).
		return fail(&contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenConfigInvalid, Message: "failed to initialize state runtime: another start is under way"})
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
			if !s.req.Loaded || !s.adapter.breaks("load-starts-fresh") {
				return fail(&contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenSessionNotFound, Message: id})
			}
			if err := st.create(); err != nil {
				return fail(contract.Errorf(contract.CodeInternal, "%v", err))
			}
		}
		if err := s.cursor.load(st, s.req.Checkpoint); err != nil {
			return fail(err)
		}
		if fi, err := os.Stat(st.recordPath()); err == nil && s.req.Loaded {
			s.loadedAt = fi.Size()
		}
		if s.req.Loaded && s.adapter.breaks("load-drops-goal") {
			_ = os.Remove(st.goalPath())
		}
	}
	select {
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-s.closed:
		return fail(&contract.Error{Code: contract.CodeClosed})
	default:
	}
	if !s.hold(st.dir) {
		return fail(&contract.Error{Code: contract.CodeOpenFailed, Reason: contract.OpenSessionInUse, Message: id + " is open in another Host"})
	}

	if s.beforeIdle != nil {
		s.beforeIdle()
	}

	s.mu.Lock()
	if s.closing || s.phase == contract.PhaseExited {
		// A Close (or crash) landed after the check above: the Session must
		// not come up idle holding its directory for good.
		s.mu.Unlock()
		s.letGo(st.dir)
		return fail(&contract.Error{Code: contract.CodeClosed})
	}
	s.store = st
	s.sessionID = id
	s.proc = int(time.Now().UnixNano() % 1e9)
	s.phase = contract.PhaseIdle
	s.opening = false
	state := s.stateLocked()
	s.mu.Unlock()
	go s.watchKill()
	go s.connectMCP()
	// A Session with an active goal goes back to work as it opens.
	s.work()
	return contract.OpenResult{SessionID: id, State: state}, nil
}

// watchKill turns a crash into the Session's exit.
func (s *session) watchKill() {
	<-s.kill
	siblings := s.siblings()
	s.exit(contract.ExitCrashed, "killed without close")
	if s.adapter.breaks("crash-siblings") {
		for _, sib := range siblings {
			Kill(sib)
		}
	}
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
	at := s.auto
	s.auto = nil
	// The background work dies with the harness.
	s.background, s.taken = nil, nil
	s.counter++
	key := fmt.Sprintf("%d.%d", s.proc, s.counter)
	dir := s.store.dir
	s.mu.Unlock()
	s.letGo(dir)
	if t != nil {
		t.intOnce.Do(func() { close(t.interrupt) })
		select {
		case t.outcome <- "":
		default:
		}
	}
	if at != nil {
		at.stop()
		at.finish()
	}
	if !s.adapter.breaks("no-session-exited") {
		s.cursor.push(item{obs: contract.NewObservation(contract.KindSessionExited, key, contract.OriginLive, time.Now(),
			contract.SessionExitedData{Class: class, Detail: detail})})
	}
	close(s.exited)
}

func (s *session) stateLocked() contract.State {
	st := contract.State{Phase: s.phase, Block: s.block, Prompt: s.prompt}
	switch {
	case s.turn != nil:
		st.InputID = s.turn.in.InputID
		st.TurnID = turnID(s.turn.in.InputID)
	case s.auto != nil:
		st.TurnID = s.auto.id
	}
	st.Background = slices.Clone(s.background)
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
		// An input that was refused leaves the harness to its own work.
		s.work()
	}()
	s.mu.Lock()
	for s.auto != nil && s.auto.bg == nil && s.turn == nil && s.phase == contract.PhaseBusy && !s.closing && !s.adapter.breaks("auto-send-busy") {
		// The harness is on a turn of its own: the input stops it, and the
		// input's turn follows.
		at := s.auto
		at.preempted = true
		s.mu.Unlock()
		at.stop()
		select {
		case <-at.done:
		case <-ctx.Done():
			return contract.SendResult{}, &contract.Error{Code: contract.CodeBusy, Certainty: contract.NotSubmitted, Message: "the harness's own turn did not stop"}
		}
		s.mu.Lock()
	}
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

// record appends a record observation and queues it for delivery. One of a
// turn the harness started itself names that turn already, and no input.
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
	if s.adapter.breaks("cross-deliver") {
		for _, sib := range s.siblings() {
			sib.cursor.push(item{obs: o})
		}
	}
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
	s.hear()

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
	case "LIMIT":
		s.fail(t, limitStatus)
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
	case "BG":
		s.startBackground(arg)
		reply("BG STARTED")
	case "MKGOAL":
		s.mu.Lock()
		st := s.store
		s.mu.Unlock()
		g := goal{Objective: arg, Active: true}
		if old := st.readGoal(); old != nil {
			g.Seq = old.Seq
		}
		st.writeGoal(g)
		reply("TOOL DONE")
	default:
		reply("ok")
	}
}

// hear gives the model the conversation: everything the record holds. A
// loaded Session's record is the saved one, continued.
func (s *session) hear() {
	s.mu.Lock()
	st, from := s.store, int64(0)
	if s.adapter.breaks("load-forgets") {
		from = s.loadedAt
	}
	s.mu.Unlock()
	lines, _, err := st.readRecord(from)
	if err != nil {
		return
	}
	if s.req.Loaded && s.adapter.breaks("load-mixes-sessions") {
		for _, other := range workspaceRecords(st) {
			more, _, _ := other.readRecord(0)
			lines = append(lines, more...)
		}
	}
	var heard []string
	for _, l := range lines {
		var d struct {
			Text   string `json:"text"`
			Output string `json:"output"`
		}
		switch l.obs.Kind {
		case contract.KindUserInput, contract.KindAssistantText, contract.KindToolResult:
			if l.obs.Decode(&d) == nil {
				heard = append(heard, d.Text+d.Output)
			}
		}
	}
	s.mu.Lock()
	s.heard = heard
	s.mu.Unlock()
}

// work starts a turn of the harness's own when its goal is active and
// nothing else runs: once the Session opens, and once a turn completes. An
// input being sent comes first.
func (s *session) work() {
	s.mu.Lock()
	if s.phase != contract.PhaseIdle || s.turn != nil || s.auto != nil || s.closing || s.sending != nil {
		s.mu.Unlock()
		return
	}
	if len(s.taken) > 0 {
		// Background work ended: a turn takes its result up, whatever
		// stopped a turn of the harness's own before.
		bg := s.taken[0]
		s.taken = s.taken[1:]
		at := &autoTurn{id: "a_" + bg.ID, bg: &bg, interrupt: make(chan struct{}), done: make(chan struct{})}
		s.auto = at
		s.phase = contract.PhaseBusy
		s.mu.Unlock()
		go s.runBackgroundTurn(at)
		return
	}
	if s.rest {
		s.mu.Unlock()
		return
	}
	st := s.store
	g := st.readGoal()
	if g == nil || !g.Active {
		s.mu.Unlock()
		return
	}
	g.Turns++
	g.Seq++
	st.writeGoal(*g)
	at := &autoTurn{id: "a_g" + strconv.Itoa(g.Seq), interrupt: make(chan struct{}), done: make(chan struct{})}
	s.auto = at
	s.phase = contract.PhaseBusy
	s.mu.Unlock()
	go s.runAuto(at, *g)
}

// autoObs is an observation of a turn the harness started itself: it names
// the turn, and no input.
func (s *session) autoObs(at *autoTurn, kind contract.Kind, key string, origin contract.Origin, data any) contract.Observation {
	o := contract.NewObservation(kind, key, origin, time.Now(), data)
	o.TurnID = at.id
	if s.adapter.breaks("auto-input-id") {
		o.InputID = "auto"
	}
	return o
}

// runAuto is the fake harness working on its goal for one turn.
func (s *session) runAuto(at *autoTurn, g goal) {
	tick := s.adapter.opts.Tick
	reported := !s.adapter.breaks("auto-unreported")
	if reported {
		s.cursor.push(item{obs: s.autoObs(at, contract.KindTurnStarted, at.id, contract.OriginLive, struct{}{})})
	}
	words := strings.Fields(g.Objective)
	n, k := 1, 1
	if len(words) > 1 {
		n = atoi(words[1], 1)
	}
	if len(words) > 2 {
		k = atoi(words[2], 1)
	}
	text := "TOOL DONE"
	if g.Turns <= n {
		for i := 0; i < k; i++ {
			select {
			case <-at.interrupt:
				s.endAuto(at, contract.TurnInterrupted, "", reported)
				return
			case <-time.After(tick):
			}
		}
		text = "goal step " + strconv.Itoa(g.Turns)
	} else {
		g.Active = false
		s.mu.Lock()
		st := s.store
		s.mu.Unlock()
		st.writeGoal(g)
	}
	msg := "m-" + at.id
	o := s.autoObs(at, contract.KindAssistantText, msg+".0", contract.OriginRecord, contract.AssistantTextData{MessageID: msg, Text: text})
	s.mu.Lock()
	st := s.store
	s.mu.Unlock()
	if end, err := st.appendRecord(o); err == nil {
		s.cursor.push(item{obs: o, end: end})
	}
	s.endAuto(at, contract.TurnCompleted, text, reported)
}

// endAuto settles a turn of the harness's own, live and in the record. One
// that completed is followed by the next, while the goal is active; one an
// interrupt stopped is followed by none, until an input's turn completes.
func (s *session) endAuto(at *autoTurn, outcome contract.TurnOutcome, text string, reported bool) {
	s.mu.Lock()
	if s.auto != at || s.phase == contract.PhaseExited {
		s.mu.Unlock()
		return
	}
	st := s.store
	s.mu.Unlock()
	data := contract.TurnEndedData{Outcome: outcome, Text: text}
	if reported {
		s.cursor.push(item{obs: s.autoObs(at, contract.KindTurnEnded, at.id, contract.OriginLive, data)})
	}
	if reported && !s.adapter.breaks("auto-no-record-end") {
		o := s.autoObs(at, contract.KindTurnEnded, at.id, contract.OriginRecord, data)
		if end, err := st.appendRecord(o); err == nil {
			s.cursor.push(item{obs: o, end: end})
		}
	}
	s.mu.Lock()
	if s.auto == at {
		s.auto = nil
		if s.phase == contract.PhaseBusy {
			s.phase = contract.PhaseIdle
		}
	}
	at.outcome = contract.InterruptTooLate
	if outcome == contract.TurnInterrupted {
		at.outcome = contract.InterruptStopped
		if !at.preempted && at.bg == nil && !s.adapter.breaks("auto-restarts") {
			s.rest = true
		}
	}
	s.autoEnded[at.id] = at.outcome
	again := outcome == contract.TurnCompleted || at.bg != nil || !at.preempted && s.adapter.breaks("auto-restarts")
	s.mu.Unlock()
	at.finish()
	if again {
		s.work()
	}
}

// startBackground starts command in the background: it runs for a few ticks,
// past the turn that started it, and once it ends the harness takes its
// result up in a turn of its own (background_turns).
func (s *session) startBackground(command string) {
	task := contract.BackgroundTask{ID: "bg" + randomHex(6), Kind: contract.BackgroundCommand, Description: truncateDescription(command)}
	s.mu.Lock()
	s.background = append(s.background, task)
	tasks := slices.Clone(s.background)
	s.mu.Unlock()
	s.reportBackground(tasks)
	go s.runBackground(task)
}

// truncateDescription cuts a task's description to 200 bytes.
func truncateDescription(d string) string {
	if len(d) > 200 {
		return d[:200]
	}
	return d
}

// reportBackground reports the work running in the background now. It names
// no input or turn: the work outlives the turn that started it.
func (s *session) reportBackground(tasks []contract.BackgroundTask) {
	if s.adapter.breaks("bg-tasks-unreported") {
		return
	}
	if tasks == nil {
		tasks = []contract.BackgroundTask{}
	}
	s.live(contract.KindBackgroundTasks, s.liveKey(), "", contract.BackgroundTasksData{Tasks: tasks})
}

// runBackground is the background work itself: it ends after a few ticks,
// unless the harness exits first, and its result waits for a turn.
func (s *session) runBackground(task contract.BackgroundTask) {
	select {
	case <-time.After(5 * s.adapter.opts.Tick):
	case <-s.exited:
		return
	}
	s.mu.Lock()
	if s.phase == contract.PhaseExited {
		s.mu.Unlock()
		return
	}
	s.background = slices.DeleteFunc(s.background, func(t contract.BackgroundTask) bool { return t.ID == task.ID })
	if !s.adapter.breaks("bg-not-taken-up") {
		s.taken = append(s.taken, task)
	}
	tasks := slices.Clone(s.background)
	s.mu.Unlock()
	s.reportBackground(tasks)
	s.work()
}

// runBackgroundTurn is the fake harness taking up background work's result
// in a turn of its own. An input sent meanwhile is refused busy, as during
// an input's turn; an interrupt naming the turn stops it.
func (s *session) runBackgroundTurn(at *autoTurn) {
	s.cursor.push(item{obs: s.autoObs(at, contract.KindTurnStarted, at.id, contract.OriginLive, struct{}{})})
	select {
	case <-at.interrupt:
		s.endAuto(at, contract.TurnInterrupted, "", true)
		return
	case <-time.After(s.adapter.opts.Tick):
	}
	text := "BG DONE: ran " + at.bg.Description
	msg := "m-" + at.id
	o := s.autoObs(at, contract.KindAssistantText, msg+".0", contract.OriginRecord, contract.AssistantTextData{MessageID: msg, Text: text})
	s.mu.Lock()
	st := s.store
	s.mu.Unlock()
	if end, err := st.appendRecord(o); err == nil {
		s.cursor.push(item{obs: o, end: end})
	}
	s.endAuto(at, contract.TurnCompleted, text, true)
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

// limitStatus is the status fail takes for a usage wall: a 429 the account's
// limit refused, not one the server's load did.
const limitStatus = -429

// fail ends a turn whose model call failed for good, and closes the gate on a
// wall that prevents the next input.
func (s *session) fail(t *turn, code int) {
	class, block := contract.ErrorAPI, contract.BlockReason("")
	var resume *time.Time
	switch code {
	case 529:
		class = contract.ErrorOverloaded
		if s.adapter.breaks("block-on-overloaded") {
			block = contract.BlockUsageLimited
		}
	case limitStatus:
		code = 429
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
	// The harness takes its own work up once an input's turn completes.
	s.rest = outcome != contract.TurnCompleted
	s.mu.Unlock()
	select {
	case t.outcome <- intOutcome:
	default:
	}
	// Background work that ended meanwhile is taken up however the turn
	// ended; the goal, only once it completed (work reads rest).
	s.work()
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
	if err := req.Validate(); err != nil {
		return "", err
	}
	s.afterSend()
	s.mu.Lock()
	if s.phase == contract.PhaseUnopened || s.phase == contract.PhaseStarting {
		s.mu.Unlock()
		return "", contract.Errorf(contract.CodeUnexpected, "interrupt in phase %s", s.phase)
	}
	if req.TurnID != "" {
		return s.interruptAuto(ctx, req) // unlocks
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
		own := s.auto != nil
		s.mu.Unlock()
		if own || s.adapter.breaks("idle-too-late") {
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
	if s.adapter.breaks("interrupt-siblings") {
		for _, sib := range s.siblings() {
			sib.mu.Lock()
			st := sib.turn
			sib.mu.Unlock()
			if st != nil {
				st.intOnce.Do(func() { close(st.interrupt) })
			}
		}
	}
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

// interruptAuto stops the turn req names by its turn id, one the harness
// started itself, if it is the turn the harness is on. It is called with s.mu
// held, and releases it.
func (s *session) interruptAuto(ctx context.Context, req contract.InterruptRequest) (contract.InterruptOutcome, error) {
	if outcome, done := s.autoEnded[req.TurnID]; done {
		s.mu.Unlock()
		return outcome, nil
	}
	at := s.auto
	switch {
	case at == nil && s.turn == nil:
		s.mu.Unlock()
		return contract.InterruptNoTurn, nil
	case at == nil || at.id != req.TurnID:
		s.mu.Unlock()
		return contract.InterruptTooLate, nil
	}
	s.mu.Unlock()
	if s.adapter.breaks("auto-interrupt-ignored") {
		return contract.InterruptStopped, nil
	}
	at.stop()
	deadline := time.NewTimer(req.Deadline())
	defer deadline.Stop()
	select {
	case <-at.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		if at.outcome == "" {
			return "", &contract.Error{Code: contract.CodeExited}
		}
		return at.outcome, nil
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
	siblings := s.siblings()
	s.exit(contract.ExitClean, "closed: "+string(reason))
	if s.adapter.breaks("close-siblings") {
		for _, sib := range siblings {
			sib.exit(contract.ExitCrashed, "a sibling closed")
		}
	}
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
