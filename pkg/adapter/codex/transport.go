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
)

type transport struct {
	thread string
	report func(adapter.Event)
	cmd    *exec.Cmd
	stdout *os.File
	stderr *tailBuffer

	wmu         sync.Mutex // serializes lines on stdin, and closing it
	stdin       io.WriteCloser
	stdinClosed bool

	cmu     sync.Mutex
	calls   map[int64]chan rpcResult
	nextID  int64
	callEnd bool // codex exited: no call gets an answer

	mu        sync.Mutex
	turn      *turnState // the input codex is on, nil when none
	limits    *rateLimits
	stopping  bool
	killed    bool
	exited    chan struct{} // closed once the process ended and Exited was reported
	readerEnd chan struct{}
}

// turnState is what the transport knows of the input codex is on.
type turnState struct {
	native  string
	turnID  string // codex's, once turn/start answers or turn/started names it
	started bool
	text    string // the turn's last agent message
	// interrupt: an interrupt was asked; interrupting: it went to codex.
	interrupt, interrupting bool
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
	env := append(adapter.HostEnv(), cfg.Env...)
	apiKey := ""
	if c := req.Credential; c != nil {
		tok, err := readToken(c.File)
		if err != nil {
			return nil, openFailed(contract.OpenAuthRequired, "%v", err)
		}
		switch c.Kind {
		case CredentialAPIKey:
			apiKey = tok
		case CredentialAccessToken:
			env = append(env, "CODEX_ACCESS_TOKEN="+tok)
		default:
			return nil, openFailed(contract.OpenConfigInvalid, "credential kind %q, want %s or %s", c.Kind, CredentialAPIKey, CredentialAccessToken)
		}
	}
	t := &transport{
		report: req.Report,
		calls:  map[int64]chan rpcResult{},
		exited: make(chan struct{}), readerEnd: make(chan struct{}),
		stderr: newTailBuffer(stderrTail),
	}
	if err := t.start(cfg.Binary, cfg.WorkingDir, env); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "start %s: %v", cfg.Binary, err)
	}
	fail := func(reason contract.OpenFailure, err error) (adapter.Transport, error) {
		t.Stop(context.Background(), 0)
		<-t.exited
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if tail := lastLine(t.stderr.String()); tail != "" {
			return nil, openFailed(reason, "%v: %s", err, tail)
		}
		return nil, openFailed(reason, "%v", err)
	}
	ictx, cancel := context.WithTimeout(ctx, initWait)
	defer cancel()
	if _, err := t.call(ictx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "harness-wrapper", "title": "harness-wrapper", "version": adapter.Name()},
	}); err != nil {
		return fail(contract.OpenConfigInvalid, fmt.Errorf("initialize: %w", err))
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
	if req.Mode == contract.OpenReopen {
		if _, err := tcodex.Rollout(cfg.CodexHome, req.SessionID); errors.Is(err, fs.ErrNotExist) {
			// A thread codex never wrote a rollout for — its launch ended
			// before its first turn — has nothing to resume: a new one
			// starts, under the id codex gives it.
			method = "thread/start"
		} else {
			method = "thread/resume"
			thread["threadId"] = req.SessionID
		}
	}
	raw, err := t.call(ictx, method, thread)
	if err != nil {
		reason := contract.OpenConfigInvalid
		switch msg := strings.ToLower(err.Error()); {
		case strings.Contains(msg, "active writer"):
			reason = contract.OpenSessionInUse
		case strings.Contains(msg, "not found") || strings.Contains(msg, "no rollout"):
			reason = contract.OpenSessionNotFound
		}
		return fail(reason, fmt.Errorf("%s: %w", method, err))
	}
	if json.Unmarshal(raw, &res) != nil || res.Thread.ID == "" {
		return fail(contract.OpenConfigInvalid, fmt.Errorf("%s answered no thread: %s", method, raw))
	}
	t.thread = res.Thread.ID
	return t, nil
}

// readToken reads a credential file: one line, no control characters.
func readToken(file string) (string, error) {
	b, err := os.ReadFile(file) //nolint:gosec // the staged credential's path
	if err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) {
			return "", fmt.Errorf("credential file: %w", pe.Err)
		}
		return "", errors.New("credential file unreadable")
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, "\n\r\x00") {
		return "", errors.New("credential file is empty or malformed")
	}
	return tok, nil
}

func (t *transport) start(bin, dir string, env []string) error {
	cmd := exec.Command(bin, "app-server")
	cmd.Dir, cmd.Env = dir, env
	procgroup.Set(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// stdout and stderr are plain pipes read here, so Wait never waits on a
	// descendant that inherited them.
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		_, _, _, _ = outR.Close(), outW.Close(), errR.Close(), errW.Close()
		return err
	}
	_, _ = outW.Close(), errW.Close()
	t.cmd, t.stdin, t.stdout = cmd, stdin, outR
	go func() {
		_, _ = io.Copy(t.stderr, errR)
		_ = errR.Close()
	}()
	go t.read()
	go t.wait()
	return nil
}

// SessionID is the Session's thread id.
func (t *transport) SessionID() string { return t.thread }

// NewNative is an input's id in codex's terms: its clientUserMessageId.
func (t *transport) NewNative(string) string { return sessionid.NewUUID() }

// ---- reading

func (t *transport) read() {
	defer close(t.readerEnd)
	r := bufio.NewReaderSize(t.stdout, 64<<10)
	for {
		line, err := readBoundedLine(r, frameMax)
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

// readBoundedLine reads one line of at most max bytes; a longer one is
// consumed and returned empty.
func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	over := false
	for {
		part, isPrefix, err := r.ReadLine()
		if !over {
			line = append(line, part...)
			if len(line) > max {
				over, line = true, nil
			}
		}
		if err != nil {
			return line, err
		}
		if !isPrefix {
			return line, nil
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
	} `json:"item"`
	Error      *turnError  `json:"error"`
	WillRetry  bool        `json:"willRetry"`
	RateLimits *rateLimits `json:"rateLimits"`
}

func (t *transport) onNotification(method string, raw json.RawMessage) {
	var n notification
	if json.Unmarshal(raw, &n) != nil {
		return
	}
	var ev *adapter.Event
	interrupt := false
	t.mu.Lock()
	ts := t.turn
	// mine binds codex's turn id to the input the transport sent: codex runs
	// one turn at a time, and folds no other input into it.
	mine := func(id string) bool {
		if ts == nil || id == "" {
			return false
		}
		if ts.turnID == "" {
			ts.turnID = id
		}
		return ts.turnID == id
	}
	switch method {
	case "turn/started":
		if n.Turn != nil && mine(n.Turn.ID) && !ts.started {
			ts.started = true
			ev = &adapter.Event{Kind: adapter.Started, Native: ts.native}
			// An interrupt asked before the turn was active goes now.
			if ts.interrupt && !ts.interrupting {
				ts.interrupting, interrupt = true, true
			}
		}
	case "item/completed":
		if ts != nil && n.Item != nil && n.Item.Type == "agentMessage" && (n.TurnID == "" || mine(n.TurnID)) {
			ts.text = n.Item.Text
		}
	case "turn/completed":
		if n.Turn == nil || !mine(n.Turn.ID) {
			break
		}
		t.turn = nil
		e := adapter.Event{Kind: adapter.Ended, Native: ts.native}
		switch n.Turn.Status {
		case "completed":
			e.Outcome, e.Text = contract.TurnCompleted, ts.text
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
		ev = &e
	case "error":
		if n.WillRetry && n.Error != nil && mine(n.TurnID) {
			ev = &adapter.Event{Kind: adapter.Retrying, Native: ts.native, Retry: retry(*n.Error)}
		}
	case "account/rateLimits/updated":
		if n.RateLimits == nil {
			break
		}
		t.limits = t.limits.merge(n.RateLimits)
		e := adapter.Event{Kind: adapter.RateLimited, RateLimit: t.limits.data(), ResumeAt: t.limits.resumeAt()}
		if ts != nil {
			e.Native = ts.native
		}
		ev = &e
	}
	t.mu.Unlock()
	if ev != nil {
		t.report(*ev)
	}
	if interrupt {
		go t.interrupt(ts)
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

	exit := contract.SessionExitedData{Class: contract.ExitCrashed}
	var code int
	sig := ""
	if ps := t.cmd.ProcessState; ps != nil {
		code, sig = ps.ExitCode(), procgroup.ExitSignal(ps)
	}
	if sig == "" && code >= 0 {
		c := code
		exit.ExitCode = &c
	}
	exit.Signal = sig
	t.mu.Lock()
	stopping, killed := t.stopping, t.killed
	t.mu.Unlock()
	switch {
	case stopping && sig == "killed":
		exit.Class = contract.ExitKilled
	case stopping, sig == "" && code == 0:
		exit.Class = contract.ExitClean
	case sig != "" || killed:
		exit.Class = contract.ExitKilled
	}
	exit.Detail = lastLine(t.stderr.String())
	if exit.Detail == "" && err != nil && sig == "" && code != 0 {
		exit.Detail = err.Error()
	}
	t.report(adapter.Event{Kind: adapter.Exited, Exit: exit})
	close(t.exited)
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
// request codex never got fails with errStdinClosed.
func (t *transport) send(method string, params any) (int64, chan rpcResult, error) {
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
	id, ch, err := t.send(method, params)
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

// Submit starts a turn on the input, its native id as the turn's
// clientUserMessageId, and returns once codex answers with the turn.
func (t *transport) Submit(ctx context.Context, s adapter.Submission) error {
	t.mu.Lock()
	select {
	case <-t.exited:
		t.mu.Unlock()
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "codex exited"}
	default:
	}
	ts := &turnState{native: s.Native}
	t.turn = ts
	t.mu.Unlock()
	forget := func() {
		t.mu.Lock()
		if t.turn == ts {
			t.turn = nil
		}
		t.mu.Unlock()
	}
	sctx, cancel := context.WithTimeout(ctx, submitWait)
	defer cancel()
	raw, err := t.call(sctx, "turn/start", map[string]any{
		"threadId":            t.thread,
		"input":               []map[string]string{{"type": "text", "text": s.Text}},
		"clientUserMessageId": s.Native,
	})
	var re *rpcError
	switch {
	case errors.Is(err, errStdinClosed):
		forget()
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "codex exited"}
	case errors.As(err, &re):
		// codex refused the turn: nothing of it ran.
		forget()
		return fmt.Errorf("%w: %v", adapter.ErrNotSubmitted, err)
	case err != nil:
		return fmt.Errorf("starting the turn: %w", err)
	}
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if json.Unmarshal(raw, &res) == nil && res.Turn.ID != "" {
		t.mu.Lock()
		if ts.turnID == "" {
			ts.turnID = res.Turn.ID
		}
		t.mu.Unlock()
	}
	return nil
}

// Interrupt asks codex to stop the turn it is on. codex refuses to interrupt
// a turn it has not made active yet, so an interrupt asked before
// turn/started goes once it comes. Its end comes as the turn's
// turn/completed.
func (t *transport) Interrupt(context.Context) error {
	t.mu.Lock()
	ts := t.turn
	now := false
	if ts != nil && !ts.interrupt {
		ts.interrupt = true
		if ts.started && ts.turnID != "" {
			ts.interrupting, now = true, true
		}
	}
	t.mu.Unlock()
	if now {
		go t.interrupt(ts)
	}
	return nil
}

// interrupt sends turn/interrupt for ts, again while codex refuses it and
// the turn is still codex's current one; the turn's end, not the answer,
// settles it.
func (t *transport) interrupt(ts *turnState) {
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		t.mu.Lock()
		current := t.turn == ts
		t.mu.Unlock()
		if !current {
			return
		}
		_, ch, err := t.send("turn/interrupt", map[string]string{"threadId": t.thread, "turnId": ts.turnID})
		if err != nil {
			return
		}
		timer := time.NewTimer(interruptAnswer)
		select {
		case r := <-ch:
			timer.Stop()
			var re *rpcError
			if !errors.As(r.err, &re) {
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

// Stop ends codex's process group: an idle codex is asked to quit by closing
// its stdin, which it answers by exiting; then the group gets SIGTERM, and
// SIGKILL once grace ends. It reports whether the group is gone.
func (t *transport) Stop(ctx context.Context, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	t.mu.Lock()
	t.stopping = true
	busy := t.turn != nil
	t.mu.Unlock()
	if !busy {
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
		t.untilGone(ctx, time.Time{})
	}
	return t.gone()
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
	return procgroup.Empty(t.cmd)
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

// untilGone polls for the group to end, until deadline (never, when zero) or
// ctx ends.
func (t *transport) untilGone(ctx context.Context, deadline time.Time) {
	for !t.gone() {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ---- helpers

// tailBuffer keeps the last n bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

func newTailBuffer(n int) *tailBuffer { return &tailBuffer{n: n} }

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.n {
		b.buf = b.buf[len(b.buf)-b.n:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
