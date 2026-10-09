package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/internal/procgroup"
	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/internal/proc"
	"github.com/olesho/harness-wrapper/pkg/contract"
	tcodex "github.com/olesho/harness-wrapper/pkg/transcript/codex"
)

// The app-server transport: codex runs as
//
//	codex app-server
//
// on pipes, one process for the Session, speaking JSON-RPC 2.0 a line at a
// time. The transport initializes it, hands it an API key when that is the
// credential, and starts the Session's thread (thread/start) or resumes it
// (thread/resume); the thread's id is the Session's. An input is one
// turn/start carrying the input's native id as its clientUserMessageId,
// which the rollout records with the input; its response is the receipt.
// turn/started and turn/completed bound the turn, error notifications with
// willRetry are its retries, and account/rateLimits/updated reports usage.
//
// codex folds an input sent during a turn into that turn instead of queueing
// it, so the Session's one outstanding send is the transport's too. An
// interrupt is settled by its turn's turn/completed, never by the
// turn/interrupt response, which codex may hold until a later turn.
//
// A thread with an active goal makes codex start turns by itself: when the
// thread resumes, and whenever a turn ends other than interrupted. Such a
// turn has an id of codex's own and no user message; the transport reports it
// as codex's own (adapter.SelfStarter). An input never joins one: Submit
// stops the turn codex is on, waiting first for one it is about to start, and
// only then starts the input's. Interrupted, codex starts no turn of its own
// until an input's turn ends or the thread resumes, so the input's turn is
// the input's alone. Should codex start one in the instant before it reads
// the input all the same, it folds the input into that turn: the turn is then
// the input's from its user message on, and codex's own ends there. An input
// such a turn drops — it was interrupted first — is sent again; codex keeps
// no record of a client id, so one a turn may still hold never is.

const (
	// frameMax bounds one stdout line; a longer one is skipped whole.
	frameMax = 32 << 20
	// initWait bounds the start: initialize, the login and the thread.
	initWait = time.Minute
	// submitWait bounds the wait for turn/start's response.
	submitWait = 2 * time.Minute
	// drainGrace is how long stdout may still be read after codex exits: a
	// descendant that inherited the pipe keeps it open.
	drainGrace = 2 * time.Second
	// quitWait is how long an idle codex may take to exit on stdin EOF.
	quitWait = 3 * time.Second
	// interruptAnswer is how long an interrupt waits for turn/interrupt's
	// answer before it leaves the turn's end to settle it: codex may hold
	// the answer until a later turn.
	interruptAnswer = 5 * time.Second
	// interruptRetry is how soon an interrupt codex refused — its turn not
	// active yet — is asked again.
	interruptRetry = 50 * time.Millisecond
	// stderrTail is how much of codex's stderr an exit's detail keeps.
	stderrTail = 4 << 10
	// chainWait is how long an input waits for the turn codex is about to
	// start by itself, so as to stop it, before it goes ahead.
	chainWait = 3 * time.Second
	// resendWait is how long an input codex dropped — it was queued for a
	// turn of codex's own, which was interrupted first — waits before it is
	// sent again. One queued for a turn that ended otherwise, never taking it
	// in, waits resendPatience for codex to start it, and then ends errored:
	// codex takes such an input in before the turn ends (probes/codexturns),
	// so it is the transport that missed it, or a codex that holds it still.
	resendWait     = time.Second
	resendPatience = 5 * time.Second
	// nativeWait bounds the read of what codex keeps of the thread beside
	// its rollout, as the thread parks.
	nativeWait = 2 * time.Second
)

// killWait bounds Stop's wait for the group to end after SIGKILL: a process
// of it nobody reaps (the Host as PID 1) never does. A var for tests.
var killWait = 5 * time.Second

// groupEmpty reports whether a process group ended; a var for tests.
var groupEmpty = procgroup.Empty

type transport struct {
	report func(adapter.Event)
	cmd    *exec.Cmd
	stdout *os.File
	stderr *proc.TailBuffer
	// scratch and home are the layout's scratch root and CODEX_HOME: where
	// the thread's native state is kept, and where codex keeps the thread.
	scratch, home string
	// version is codex's, from its initialize answer; "" when unknown.
	version string

	wmu         sync.Mutex // serializes lines on stdin, and closing it
	stdin       io.WriteCloser
	stdinClosed bool

	cmu     sync.Mutex
	calls   map[int64]chan rpcResult
	nextID  int64
	callEnd bool // codex exited: no call gets an answer

	// rmu serializes reports: a change that reports holds it from the change
	// to the report, so events reach the Session in the order they happened.
	rmu sync.Mutex
	// nmu serializes writes of the thread's native state; kept is the last.
	nmu  sync.Mutex
	kept *nativeState

	mu     sync.Mutex
	thread string
	in     *inputState // the input being submitted, or on codex; nil when none
	run    *runState   // the turn codex is on, from turn/started to turn/completed
	// name and goal are what codex last said of the thread's.
	name string
	goal *goal
	// chain: codex is about to start a turn of its own — the thread resumed,
	// or a turn ended other than interrupted, with the goal active. chainAt
	// is since when.
	chain   bool
	chainAt time.Time
	wake    chan struct{} // closed, and replaced, whenever run or chain changes
	// steer sends an input while a turn of codex's own runs, as no Session
	// does: for a test of what codex then does with it.
	steer     bool
	limits    *rateLimits
	stopping  bool
	killed    bool
	exited    chan struct{} // closed once the process ended and Exited was reported
	readerEnd chan struct{}
}

// inputState is what the transport knows of the input it sent codex.
type inputState struct {
	native string
	text   string
	// call is its turn/start's request id while that is unanswered; turnID,
	// the turn codex answered it with.
	call     int64
	answered bool
	turnID   string
	// bound: a turn holds it — its own, started, or one of codex's that took
	// it in.
	bound bool
	// interrupt: an interrupt of it was asked.
	interrupt bool
	// resent: codex dropped it, and it was sent again; again is set while it
	// waits for that.
	resent bool
	again  *time.Timer
	// held: a turn of codex's own held it and ended other than interrupted,
	// never taking it in, so codex may hold it still: it is never sent again.
	held bool
}

// runState is the turn codex is on.
type runState struct {
	id string
	// in is the input whose turn it is; auto, that it was reported as one
	// codex started itself. Neither, while turn/start's answer is awaited to
	// say which.
	in   *inputState
	auto bool
	text string // its last agent message
	// stopping: turn/interrupt went to codex for it.
	stopping bool
}

// signalLocked wakes what waits on run or chain.
func (t *transport) signalLocked() {
	close(t.wake)
	t.wake = make(chan struct{})
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

// rpcError is a JSON-RPC error codex answered with.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("codex: %s (%d)", e.Message, e.Code) }

// message is the union of the lines codex writes: a response, a
// notification, or a request of its own.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func openFailed(reason contract.OpenFailure, format string, args ...any) error {
	return &contract.Error{Code: contract.CodeOpenFailed, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Start launches codex for a Session: its app-server, initialized, holding
// the Session's thread.
func (Profile) Start(ctx context.Context, req adapter.Start) (adapter.Transport, error) {
	cfg, err := parseOpenConfig(req.OpenConfig)
	if err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "%v", err)
	}
	if req.Mode == contract.OpenFresh && req.SessionID != "" {
		return nil, openFailed(contract.OpenConfigInvalid, "codex chooses a thread's id itself")
	}
	if _, err := os.Stat(cfg.Binary); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "%v", err)
	}
	resume := false
	if req.Mode == contract.OpenReopen {
		_, err := tcodex.Rollout(cfg.CodexHome, req.SessionID)
		switch {
		case !errors.Is(err, fs.ErrNotExist):
			resume = true
		case req.Loaded:
			// A loaded thread is its rollout: with none where codex looks,
			// codex would start a thread of its own.
			return nil, openFailed(contract.OpenSessionNotFound, "the loaded thread's rollout is not where codex looks: %v", err)
		}
		// Otherwise a thread codex never wrote a rollout for — its launch
		// ended before its first turn — has nothing to resume: a new one
		// starts, under the id codex gives it.
	}
	env := caEnv(append(adapter.HostEnv(), cfg.Env...))
	apiKey := ""
	if c := req.Credential; c != nil {
		switch c.Kind {
		case CredentialAPIKey, CredentialAccessToken:
			tok, err := proc.ReadToken(c.File)
			if err != nil {
				return nil, openFailed(contract.OpenAuthRequired, "%v", err)
			}
			if c.Kind == CredentialAPIKey {
				apiKey = tok
			} else {
				env = append(env, "CODEX_ACCESS_TOKEN="+tok)
			}
		case CredentialLogin:
			if err := writeLogin(c.File, cfg.CodexHome); err != nil {
				return nil, openFailed(contract.OpenAuthRequired, "%v", err)
			}
		default:
			return nil, openFailed(contract.OpenConfigInvalid, "credential kind %q, want %s, %s or %s", c.Kind, CredentialAPIKey, CredentialAccessToken, CredentialLogin)
		}
	}
	t := &transport{
		report: req.Report, scratch: req.Layout.Scratch, home: cfg.CodexHome,
		calls: map[int64]chan rpcResult{}, wake: make(chan struct{}),
		exited: make(chan struct{}), readerEnd: make(chan struct{}),
		stderr: proc.NewTailBuffer(stderrTail),
	}
	release, err := startLock(ctx, req.Layout.Scratch)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, openFailed(contract.OpenConfigInvalid, "waiting to start: %v", err)
	}
	defer release()
	if err := t.start(cfg.Binary, cfg.WorkingDir, env); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "start %s: %v", cfg.Binary, err)
	}
	fail := func(reason contract.OpenFailure, err error) (adapter.Transport, error) {
		t.abandon()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if tail := proc.LastLine(t.stderr.String()); tail != "" {
			return nil, openFailed(reason, "%v: %s", err, tail)
		}
		return nil, openFailed(reason, "%v", err)
	}
	ictx, cancel := context.WithTimeout(ctx, initWait)
	defer cancel()
	initRaw, err := t.call(ictx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "harness-wrapper", "title": "harness-wrapper", "version": adapter.Name()},
	})
	release() // codex has initialized the home: the next start may go
	if err != nil {
		return fail(contract.OpenConfigInvalid, fmt.Errorf("initialize: %w", err))
	}
	// codex's version, for Open's result and the version policy: its user
	// agent carries it. Under flexible a codex other than the pin runs on.
	var initRes struct {
		UserAgent string `json:"userAgent"`
	}
	_ = json.Unmarshal(initRaw, &initRes)
	t.version = versionOfUserAgent(initRes.UserAgent)
	if err := adapter.CheckHarnessVersion(cfg.VersionPolicy, "codex", pinned(), t.version); err != nil {
		t.abandon()
		return nil, err
	}
	if err := t.notify("initialized", nil); err != nil {
		return fail(contract.OpenConfigInvalid, err)
	}
	if apiKey != "" {
		if _, err := t.call(ictx, "account/login/start", map[string]string{"type": "apiKey", "apiKey": apiKey}); err != nil {
			return fail(contract.OpenAuthRequired, fmt.Errorf("the API key: %w", err))
		}
	}
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	thread := map[string]any{"cwd": cfg.WorkingDir, "approvalPolicy": "never", "sandbox": "danger-full-access"}
	method := "thread/start"
	if resume {
		method = "thread/resume"
		thread["threadId"] = req.SessionID
		// What codex holds of the thread beside its rollout, asked before the
		// thread resumes: codex starts nothing to answer.
		st, err := t.readState(ictx, req.SessionID)
		if req.Loaded {
			if err != nil {
				return fail(openReason(err), fmt.Errorf("reading the loaded thread: %w", err))
			}
			if st, err = t.restoreName(ictx, st); err != nil {
				return fail(openReason(err), fmt.Errorf("naming the loaded thread: %w", err))
			}
			if err := checkLoaded(t.scratch, t.home, st); err != nil {
				return fail(contract.OpenStateMismatch, err)
			}
		}
		if err == nil {
			t.mu.Lock()
			t.name, t.goal = st.Name, st.Goal
			// Resumed, codex goes back to a goal that is active.
			t.chain, t.chainAt = st.Goal.active(), time.Now()
			t.mu.Unlock()
		}
	}
	raw, err := t.call(ictx, method, thread)
	if err != nil {
		return fail(openReason(err), fmt.Errorf("%s: %w", method, err))
	}
	if json.Unmarshal(raw, &res) != nil || res.Thread.ID == "" {
		return fail(contract.OpenConfigInvalid, fmt.Errorf("%s answered no thread: %s", method, raw))
	}
	t.mu.Lock()
	t.thread = res.Thread.ID
	t.mu.Unlock()
	t.syncNative()
	return t, nil
}

// openReason is why codex would not open a thread, from what it said.
func openReason(err error) contract.OpenFailure {
	switch msg := strings.ToLower(err.Error()); {
	case strings.Contains(msg, "active writer"):
		return contract.OpenSessionInUse
	case strings.Contains(msg, "not found") || strings.Contains(msg, "no rollout"):
		return contract.OpenSessionNotFound
	}
	return contract.OpenConfigInvalid
}

// readState asks codex what it keeps of a thread beside its rollout: its name
// and its goal. A codex that answers no goal request keeps no goal.
func (t *transport) readState(ctx context.Context, thread string) (nativeState, error) {
	st := nativeState{Thread: thread, Home: t.home}
	raw, err := t.call(ctx, "thread/read", map[string]any{"threadId": thread})
	if err != nil {
		return st, fmt.Errorf("thread/read: %w", err)
	}
	var read struct {
		Thread struct {
			Name *string `json:"name"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &read); err != nil {
		return st, fmt.Errorf("thread/read: %w", err)
	}
	if read.Thread.Name != nil {
		st.Name = *read.Thread.Name
	}
	raw, err = t.call(ctx, "thread/goal/get", map[string]any{"threadId": thread})
	var re *rpcError
	switch {
	case errors.As(err, &re):
		return st, nil
	case err != nil:
		return st, fmt.Errorf("thread/goal/get: %w", err)
	}
	var got struct {
		Goal *goal `json:"goal"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		return st, fmt.Errorf("thread/goal/get: %w", err)
	}
	st.Goal = got.Goal
	return st, nil
}

// restoreName gives a loaded thread, first opened here, the name it was saved
// with when codex holds none but the session_index.jsonl that came with it
// gives that name: codex 0.160 answers a thread's name from its state
// database, not from that file, and no archive carries the database — it
// holds the old environment's paths. A thread that came without its name
// keeps none, and checkLoaded refuses it.
func (t *transport) restoreName(ctx context.Context, got nativeState) (nativeState, error) {
	saved, err := readNative(t.scratch, got.Thread)
	if err != nil || saved.Home == t.home || saved.Name == "" || got.Name != "" || indexedName(t.home, got.Thread) != saved.Name {
		return got, nil
	}
	if _, err := t.call(ctx, "thread/name/set", map[string]any{"threadId": got.Thread, "name": saved.Name}); err != nil {
		return got, fmt.Errorf("thread/name/set: %w", err)
	}
	return t.readState(ctx, got.Thread)
}

// syncNative keeps what codex last said of the thread's name and goal, when
// it changed: the thread's native state, saved with its history.
func (t *transport) syncNative() {
	t.nmu.Lock()
	defer t.nmu.Unlock()
	t.mu.Lock()
	st := nativeState{Thread: t.thread, Name: t.name, Home: t.home}
	if t.goal != nil {
		g := *t.goal
		st.Goal = &g
	}
	t.mu.Unlock()
	if st.Thread == "" || t.scratch == "" || t.kept != nil && t.kept.same(st) {
		return
	}
	if writeNative(t.scratch, st) == nil {
		t.kept = &st
	}
}

func (t *transport) start(bin, dir string, env []string) error {
	cmd, stdin, stdout, err := proc.Start(bin, []string{"app-server"}, dir, env, t.stderr)
	if err != nil {
		return err
	}
	t.cmd, t.stdin, t.stdout = cmd, stdin, stdout
	go t.read()
	go t.wait()
	return nil
}

// HarnessVersion is codex's version, from its initialize answer.
func (t *transport) HarnessVersion() string { return t.version }

// versionOfUserAgent is codex's version in the user agent its initialize
// answer carries: "<client>/<version> (<os>) …".
func versionOfUserAgent(ua string) string {
	first, _, _ := strings.Cut(ua, " ")
	_, v, ok := strings.Cut(first, "/")
	if !ok {
		return ""
	}
	return v
}

// SessionID is the Session's thread id.
func (t *transport) SessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.thread
}

// NewNative is an input's id in codex's terms: its clientUserMessageId.
func (t *transport) NewNative(string) string { return sessionid.NewUUID() }

// ---- reading

func (t *transport) read() {
	defer close(t.readerEnd)
	r := bufio.NewReaderSize(t.stdout, 64<<10)
	for {
		line, err := proc.ReadBoundedLine(r, frameMax)
		if len(bytes.TrimSpace(line)) > 0 {
			var m message
			if json.Unmarshal(line, &m) == nil {
				t.onMessage(&m)
			}
		}
		if err != nil {
			return
		}
	}
}

func (t *transport) onMessage(m *message) {
	switch {
	case m.Method != "" && len(m.ID) > 0:
		// A request of codex's own: an approval, a question, a token
		// refresh. At bypass none comes; refuse any, so codex never waits on
		// one this transport does not serve.
		_ = t.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{
			"code": -32601, "message": "not supported by this host",
		}})
	case m.Method != "":
		t.onNotification(m.Method, m.Params)
	case len(m.ID) > 0:
		var id int64
		if json.Unmarshal(m.ID, &id) != nil {
			return
		}
		t.cmu.Lock()
		ch := t.calls[id]
		delete(t.calls, id)
		t.cmu.Unlock()
		t.onAnswer(id, m)
		if ch == nil {
			return
		}
		if m.Error != nil {
			ch <- rpcResult{err: m.Error}
			return
		}
		ch <- rpcResult{result: m.Result}
	}
}

// notification is the union of the notifications the transport reads.
type notification struct {
	TurnID string `json:"turnId"`
	Turn   *struct {
		ID     string     `json:"id"`
		Status string     `json:"status"`
		Error  *turnError `json:"error"`
	} `json:"turn"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// ClientID is a userMessage's clientUserMessageId: the input's
		// native id.
		ClientID string `json:"clientId"`
	} `json:"item"`
	Error      *turnError  `json:"error"`
	WillRetry  bool        `json:"willRetry"`
	RateLimits *rateLimits `json:"rateLimits"`
	Goal       *goal       `json:"goal"`
	ThreadName *string     `json:"threadName"`
}

// bindLocked makes r the input's turn, and reports it started. It returns r
// when an interrupt asked of the input before now is due.
func (t *transport) bindLocked(r *runState, in *inputState, evs []adapter.Event) ([]adapter.Event, *runState) {
	r.in, r.auto, in.bound = in, false, true
	evs = append(evs, adapter.Event{Kind: adapter.Started, Native: in.native})
	if in.interrupt && !r.stopping {
		r.stopping = true
		return evs, r
	}
	return evs, nil
}

// emit reports evs, in order, and sends codex the interrupt that fell due.
// The caller holds rmu.
func (t *transport) emit(evs []adapter.Event, stop *runState) {
	for _, ev := range evs {
		t.report(ev)
	}
	if stop != nil {
		go t.interrupt(stop)
	}
}

// onAnswer takes turn/start's answer: the turn codex gave the input, which
// says whether a turn that started meanwhile is the input's or codex's own.
func (t *transport) onAnswer(id int64, m *message) {
	t.rmu.Lock()
	defer t.rmu.Unlock()
	t.mu.Lock()
	in, r := t.in, t.run
	if in == nil || in.call != id {
		t.mu.Unlock()
		return
	}
	in.call = 0
	var evs []adapter.Event
	var stop *runState
	pending := r != nil && r.in == nil && !r.auto
	if m.Error != nil {
		// codex refused the turn: nothing of the input ran.
		t.in = nil
		if in.resent {
			evs = append(evs, adapter.Event{Kind: adapter.Ended, Native: in.native, Outcome: contract.TurnCancelled})
		}
	} else {
		var res struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(m.Result, &res)
		in.answered, in.turnID = true, res.Turn.ID
		if pending && r.id == in.turnID {
			evs, stop = t.bindLocked(r, in, evs)
			pending = false
		}
	}
	if pending {
		r.auto = true
		evs = append(evs, adapter.Event{Kind: adapter.Started, Auto: r.id})
	}
	t.mu.Unlock()
	t.emit(evs, stop)
}

func (t *transport) onNotification(method string, raw json.RawMessage) {
	var n notification
	if json.Unmarshal(raw, &n) != nil {
		return
	}
	var evs []adapter.Event
	var stop *runState
	native := false
	t.rmu.Lock()
	defer t.rmu.Unlock()
	t.mu.Lock()
	in, r := t.in, t.run
	switch method {
	case "turn/started":
		if n.Turn == nil || n.Turn.ID == "" || r != nil && r.id == n.Turn.ID {
			break
		}
		r = &runState{id: n.Turn.ID}
		t.run, t.chain = r, false
		t.signalLocked()
		switch {
		case in != nil && in.answered && in.turnID == r.id:
			evs, stop = t.bindLocked(r, in, evs)
		case in != nil && !in.answered:
			// turn/start's answer says whose turn this is.
		default:
			r.auto = true
			evs = append(evs, adapter.Event{Kind: adapter.Started, Auto: r.id})
		}
	case "item/started", "item/completed":
		if n.Item == nil || r == nil || n.TurnID != "" && n.TurnID != r.id {
			break
		}
		switch n.Item.Type {
		case "userMessage":
			if in == nil || in.bound || r.in != nil || n.Item.ClientID != in.native {
				break
			}
			// The input's message is in a turn that did not start as its
			// own: codex folded it in. The turn is the input's from here,
			// and codex's own ends.
			if r.auto {
				evs = append(evs, adapter.Event{Kind: adapter.Ended, Auto: r.id, Outcome: contract.TurnInterrupted})
				r.text = ""
			}
			evs, stop = t.bindLocked(r, in, evs)
		case "agentMessage":
			if method == "item/completed" {
				r.text = n.Item.Text
			}
		}
	case "turn/completed":
		if n.Turn == nil || r == nil || r.id != n.Turn.ID {
			break
		}
		e := adapter.Event{Kind: adapter.Ended}
		switch n.Turn.Status {
		case "completed":
			e.Outcome, e.Text = contract.TurnCompleted, r.text
		case "interrupted":
			e.Outcome = contract.TurnInterrupted
		default:
			e.Outcome = contract.TurnErrored
			te := turnError{}
			if n.Turn.Error != nil {
				te = *n.Turn.Error
			}
			e.Error = failure(te, t.limits, time.Now())
		}
		t.run = nil
		// codex goes on with an active goal after any turn but one that was
		// interrupted.
		t.chain, t.chainAt = e.Outcome != contract.TurnInterrupted && t.goal.active(), time.Now()
		t.signalLocked()
		if r.in != nil {
			e.Native = r.in.native
			if t.in == r.in {
				t.in = nil
			}
			evs = append(evs, e)
			break
		}
		e.Auto = r.id
		evs = append(evs, e)
		if in != nil && in.answered && !in.bound {
			// codex held the input for this turn and never took it in. An
			// interrupted turn drops what it held.
			switch {
			case e.Outcome != contract.TurnInterrupted:
				in.held = true
				t.resendLocked(in, resendPatience)
			case in.interrupt:
				t.in = nil
				evs = append(evs, adapter.Event{Kind: adapter.Ended, Native: in.native, Outcome: contract.TurnCancelled})
			default:
				t.resendLocked(in, resendWait)
			}
		}
	case "error":
		if n.WillRetry && n.Error != nil && r != nil && r.in != nil && (n.TurnID == "" || n.TurnID == r.id) {
			evs = append(evs, adapter.Event{Kind: adapter.Retrying, Native: r.in.native, Retry: retry(*n.Error)})
		}
	case "account/rateLimits/updated":
		if n.RateLimits == nil {
			break
		}
		t.limits = t.limits.merge(n.RateLimits)
		e := adapter.Event{Kind: adapter.RateLimited, RateLimit: t.limits.data(), ResumeAt: t.limits.resumeAt()}
		if r != nil && r.in != nil {
			e.Native = r.in.native
		}
		evs = append(evs, e)
	case "thread/goal/updated":
		if n.Goal == nil {
			break
		}
		g := *n.Goal
		t.goal, native = &g, true
		switch {
		case !g.active():
			t.chain = false
			t.signalLocked()
		case n.TurnID == "" && r == nil && !t.chain:
			// Set active from outside a turn, the goal makes codex start one.
			t.chain, t.chainAt = true, time.Now()
			t.signalLocked()
		}
	case "thread/goal/cleared":
		t.goal, t.chain, native = nil, false, true
		t.signalLocked()
	case "thread/name/updated":
		if n.ThreadName != nil {
			t.name, native = *n.ThreadName, true
		}
	}
	t.mu.Unlock()
	t.emit(evs, stop)
	if native {
		t.syncNative()
	}
}

// resendLocked arranges for an input codex no longer holds to be sent again —
// or, held, to end — once wait has passed with codex starting no turn for it.
func (t *transport) resendLocked(in *inputState, wait time.Duration) {
	if in.again != nil {
		in.again.Stop()
	}
	in.again = time.AfterFunc(wait, func() { t.resend(in) })
}

// resend sends an input codex dropped again; one an interrupt was asked of
// meanwhile never ran, and ends cancelled. One codex may hold still ends
// errored instead: codex would run it twice.
func (t *transport) resend(in *inputState) {
	t.rmu.Lock()
	defer t.rmu.Unlock()
	t.mu.Lock()
	select {
	case <-t.exited:
		t.mu.Unlock()
		return
	default:
	}
	if t.in != in || in.bound || in.again == nil || t.run != nil && t.run.in == nil {
		// Its turn came after all, or a turn of codex's own holds it now.
		in.again = nil
		t.mu.Unlock()
		return
	}
	in.again = nil
	if in.interrupt {
		t.in = nil
		t.mu.Unlock()
		t.report(adapter.Event{Kind: adapter.Ended, Native: in.native, Outcome: contract.TurnCancelled})
		return
	}
	if in.held {
		t.in = nil
		t.mu.Unlock()
		t.report(adapter.Event{Kind: adapter.Ended, Native: in.native, Outcome: contract.TurnErrored, Error: &contract.TurnError{Class: contract.ErrorInternal}})
		return
	}
	in.resent, in.answered, in.turnID = true, false, ""
	thread := t.thread
	t.mu.Unlock()
	if _, _, err := t.startTurn(in, thread); err != nil {
		t.mu.Lock()
		if t.in == in {
			t.in = nil
		}
		t.mu.Unlock()
		t.report(adapter.Event{Kind: adapter.Ended, Native: in.native, Outcome: contract.TurnCancelled})
	}
}

// merge is r with the values update reports: an update is sparse, and what
// it leaves out stands.
func (r *rateLimits) merge(update *rateLimits) *rateLimits {
	out := rateLimits{}
	if r != nil {
		out = *r
	}
	if update.LimitID != "" {
		out.LimitID = update.LimitID
	}
	if update.Primary != nil {
		out.Primary = update.Primary
	}
	if update.Secondary != nil {
		out.Secondary = update.Secondary
	}
	return &out
}

// wait turns codex's exit into the Session's: Exited, after every line codex
// wrote was read.
func (t *transport) wait() {
	err := t.cmd.Wait()
	select {
	case <-t.readerEnd:
	case <-time.After(drainGrace):
		_ = t.stdout.Close() // a descendant holds the pipe open
		<-t.readerEnd
	}
	t.wmu.Lock()
	t.stdinClosed = true
	_ = t.stdin.Close()
	t.wmu.Unlock()
	t.cmu.Lock()
	t.callEnd = true
	for id, ch := range t.calls {
		ch <- rpcResult{err: errExited}
		delete(t.calls, id)
	}
	t.cmu.Unlock()

	t.mu.Lock()
	stopping, killed := t.stopping, t.killed
	t.mu.Unlock()
	exit := proc.Exit(t.cmd, err, stopping, killed, t.stderr.String())
	t.rmu.Lock()
	t.report(adapter.Event{Kind: adapter.Exited, Exit: exit})
	close(t.exited)
	t.rmu.Unlock()
}

var errExited = errors.New("codex exited")

// ---- writing

var errStdinClosed = errors.New("codex's stdin is closed")

func (t *transport) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if t.stdinClosed {
		return errStdinClosed
	}
	_, err = t.stdin.Write(append(b, '\n'))
	return err
}

// send writes a request and returns the channel its answer comes on. A
// request codex never got fails with errStdinClosed. before, when set, is
// told the request's id before the request is written: before its answer can
// come.
func (t *transport) send(method string, params any, before func(id int64)) (int64, chan rpcResult, error) {
	t.cmu.Lock()
	if t.callEnd {
		t.cmu.Unlock()
		return 0, nil, errStdinClosed
	}
	t.nextID++
	id := t.nextID
	ch := make(chan rpcResult, 1)
	t.calls[id] = ch
	t.cmu.Unlock()
	if before != nil {
		before(id)
	}
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := t.write(msg); err != nil {
		t.cmu.Lock()
		delete(t.calls, id)
		t.cmu.Unlock()
		return 0, nil, err
	}
	return id, ch, nil
}

// call sends a request and waits for its answer.
func (t *transport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id, ch, err := t.send(method, params, nil)
	if err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		t.cmu.Lock()
		delete(t.calls, id)
		t.cmu.Unlock()
		return nil, ctx.Err()
	}
}

func (t *transport) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return t.write(msg)
}

// startTurn sends turn/start for in, its native id as the turn's
// clientUserMessageId. The request's id is in's before codex can answer.
func (t *transport) startTurn(in *inputState, thread string) (int64, chan rpcResult, error) {
	return t.send("turn/start", map[string]any{
		"threadId":            thread,
		"input":               []map[string]string{{"type": "text", "text": in.text}},
		"clientUserMessageId": in.native,
	}, func(id int64) {
		t.mu.Lock()
		in.call = id
		t.mu.Unlock()
	})
}

// yield makes codex ready for in, and makes in the transport's input: a turn
// codex started itself is stopped, and one it is about to start is awaited
// and stopped, so that the input's turn is the input's alone.
func (t *transport) yield(ctx context.Context, in *inputState) error {
	for {
		t.mu.Lock()
		select {
		case <-t.exited:
			t.mu.Unlock()
			return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "codex exited"}
		default:
		}
		r, wake := t.run, t.wake
		// patience is how much longer to wait for a turn codex is about to
		// start; none, once it runs.
		patience := time.Duration(0)
		switch {
		case r != nil && r.in != nil:
			t.mu.Unlock()
			return &contract.Error{Code: contract.CodeBusy, Certainty: contract.NotSubmitted, Message: "an input's turn runs"}
		case t.steer:
			r = nil
		case r != nil:
			if !r.stopping {
				r.stopping = true
				go t.interrupt(r)
			}
		case t.chain:
			patience = chainWait - time.Since(t.chainAt)
		}
		if r == nil && patience <= 0 {
			t.chain, t.in = false, in
			t.mu.Unlock()
			return nil
		}
		t.mu.Unlock()
		var due <-chan time.Time
		var timer *time.Timer
		if patience > 0 {
			timer = time.NewTimer(patience)
			due = timer.C
		}
		select {
		case <-wake:
		case <-due:
		case <-t.exited:
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return fmt.Errorf("%w: codex's own turn did not stop: %v", adapter.ErrNotSubmitted, ctx.Err())
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// Submit starts a turn on the input, its native id as the turn's
// clientUserMessageId, and returns once codex answers with the turn. A turn
// codex started itself is stopped first.
func (t *transport) Submit(ctx context.Context, s adapter.Submission) error {
	sctx, cancel := context.WithTimeout(ctx, submitWait)
	defer cancel()
	in := &inputState{native: s.Native, text: s.Text}
	if err := t.yield(sctx, in); err != nil {
		return err
	}
	forget := func() {
		t.mu.Lock()
		if t.in == in {
			t.in = nil
		}
		t.mu.Unlock()
	}
	id, ch, err := t.startTurn(in, t.SessionID())
	switch {
	case errors.Is(err, errStdinClosed):
		forget()
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "codex exited"}
	case err != nil:
		return fmt.Errorf("starting the turn: %w", err)
	}
	select {
	case res := <-ch:
		var re *rpcError
		switch {
		case errors.As(res.err, &re):
			// codex refused the turn: nothing of it ran.
			forget()
			return fmt.Errorf("%w: %v", adapter.ErrNotSubmitted, res.err)
		case res.err != nil:
			return fmt.Errorf("starting the turn: %w", res.err)
		}
		return nil
	case <-sctx.Done():
		t.cmu.Lock()
		delete(t.calls, id)
		t.cmu.Unlock()
		return fmt.Errorf("starting the turn: %w", sctx.Err())
	}
}

// Interrupt asks codex to stop the input's turn. codex refuses to interrupt
// a turn it has not made active yet, so an interrupt asked before
// turn/started goes once it comes. Its end comes as the turn's
// turn/completed. An input codex holds for a turn of its own is stopped with
// that turn, which drops it: it ends cancelled.
func (t *transport) Interrupt(context.Context) error {
	t.mu.Lock()
	in, r := t.in, t.run
	var stop *runState
	if in != nil && !in.interrupt {
		in.interrupt = true
		if r != nil && !r.stopping && (r.in == in || r.in == nil && in.answered && !in.bound) {
			r.stopping, stop = true, r
		}
	}
	t.mu.Unlock()
	if stop != nil {
		go t.interrupt(stop)
	}
	return nil
}

// InterruptTurn asks codex to stop the turn native, one it started itself,
// if it is the turn codex is on. Its end comes as that turn's turn/completed;
// codex then starts none of its own until an input's turn ends, or the
// thread resumes.
func (t *transport) InterruptTurn(_ context.Context, native string) error {
	t.mu.Lock()
	r := t.run
	var stop *runState
	if r != nil && r.in == nil && r.id == native && !r.stopping {
		r.stopping, stop = true, r
	}
	t.mu.Unlock()
	if stop != nil {
		go t.interrupt(stop)
	}
	return nil
}

// interrupt sends turn/interrupt for r, again while codex refuses it and the
// turn is still codex's current one; the turn's end, not the answer, settles
// it.
func (t *transport) interrupt(r *runState) {
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		t.mu.Lock()
		current, thread := t.run == r, t.thread
		t.mu.Unlock()
		if !current {
			return
		}
		_, ch, err := t.send("turn/interrupt", map[string]string{"threadId": thread, "turnId": r.id}, nil)
		if err != nil {
			return
		}
		timer := time.NewTimer(interruptAnswer)
		select {
		case res := <-ch:
			timer.Stop()
			var re *rpcError
			if !errors.As(res.err, &re) {
				return
			}
		case <-timer.C:
			return
		case <-t.exited:
			timer.Stop()
			return
		}
		time.Sleep(interruptRetry)
	}
}

// Answer: at bypass codex raises no prompts.
func (t *transport) Answer(context.Context, string, contract.Choice) error {
	return contract.Errorf(contract.CodeUnsupported, "codex raises no prompts at bypass")
}

// Stop ends codex's process group: a codex on no input's turn is asked to
// quit by closing its stdin, which it answers by exiting — a turn of its own
// that runs, it aborts first, and says so in the rollout; then the group gets
// SIGTERM, and SIGKILL once grace ends. It reports whether the group is gone.
// What codex holds of the thread beside its rollout is read once more before
// it is asked to quit, so the native state saved is the state it parked in.
func (t *transport) Stop(ctx context.Context, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	t.mu.Lock()
	first := !t.stopping
	t.stopping = true
	busy := t.in != nil || t.run != nil && t.run.in != nil
	thread := t.thread
	t.mu.Unlock()
	if !busy {
		if first && thread != "" && grace > 0 {
			rctx, cancel := context.WithTimeout(ctx, min(grace, nativeWait))
			if st, err := t.readState(rctx, thread); err == nil {
				t.mu.Lock()
				t.name, t.goal = st.Name, st.Goal
				t.mu.Unlock()
				t.syncNative()
			}
			cancel()
		}
		t.wmu.Lock()
		if !t.stdinClosed {
			t.stdinClosed = true
			_ = t.stdin.Close()
		}
		t.wmu.Unlock()
		t.until(ctx, t.exited, min(time.Until(deadline), quitWait))
	}
	if !t.gone() {
		procgroup.Signal(t.cmd, false)
		t.untilGone(ctx, deadline)
	}
	if !t.gone() {
		t.mu.Lock()
		t.killed = true
		t.mu.Unlock()
		procgroup.Signal(t.cmd, true)
		t.untilGone(ctx, time.Now().Add(killWait))
	}
	return t.gone()
}

// abandon stops a process Start gives up on, waiting at most killWait for
// its group, and then for its exit.
func (t *transport) abandon() {
	ctx, cancel := context.WithTimeout(context.Background(), killWait)
	defer cancel()
	t.Stop(ctx, 0)
	<-t.exited
}

// kill is a crash: SIGKILL to the group, with nothing asked first.
func (t *transport) kill() {
	t.mu.Lock()
	t.killed = true
	t.mu.Unlock()
	procgroup.Signal(t.cmd, true)
}

// gone reports whether codex and every process left in its group ended.
func (t *transport) gone() bool {
	select {
	case <-t.exited:
	default:
		return false
	}
	return groupEmpty(t.cmd)
}

func (t *transport) until(ctx context.Context, ch <-chan struct{}, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// untilGone polls for the group to end, until deadline or ctx ends.
func (t *transport) untilGone(ctx context.Context, deadline time.Time) {
	for !t.gone() {
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
