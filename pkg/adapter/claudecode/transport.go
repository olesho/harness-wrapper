package claudecode

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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/olesho/harness-wrapper/internal/harnesscore"
	"github.com/olesho/harness-wrapper/internal/procgroup"
	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
	tclaude "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// The stream-json transport: claude runs as
//
//	claude (--session-id ID | --resume ID) -p --input-format stream-json --output-format stream-json --verbose <open_config args>
//
// on pipes, one process for the Session. An input is one user frame on its
// stdin carrying the input's native id as its uuid, which claude's transcript
// keeps as the prompt entry's uuid; claude answers with a command_lifecycle
// receipt for that uuid (queued, started, then one terminal state), system and
// assistant frames, one result per turn, and control responses.

// requiredCapabilities are the protocol features the transport relies on;
// claude advertises them on system/init. Without the message lifecycle a send
// cannot be confirmed; without the interrupt receipt, an interrupt.
var requiredCapabilities = []string{"interrupt_receipt_v1", "msg_lifecycle_v1"}

const (
	// frameMax bounds one stdout frame; a longer line is skipped whole.
	frameMax = 32 << 20
	// initWait bounds the initialize round trip at start.
	initWait = time.Minute
	// submitWait bounds the wait for a message's receipt.
	submitWait = 2 * time.Minute
	// drainGrace is how long stdout may still be read after claude exits: a
	// descendant that inherited the pipe keeps it open.
	drainGrace = 2 * time.Second
	// quitWait is how long an idle claude may take to exit on stdin EOF.
	quitWait = 3 * time.Second
	// stderrTail is how much of claude's stderr an exit's detail keeps.
	stderrTail = 4 << 10
)

type transport struct {
	id     string
	report func(adapter.Event)
	cmd    *exec.Cmd
	stdout *os.File
	stderr *tailBuffer

	wmu         sync.Mutex // serializes frames on stdin, and closing it
	stdin       io.WriteCloser
	stdinClosed bool

	cmu     sync.Mutex
	control map[string]chan controlResult
	ctlSeq  int

	mu       sync.Mutex
	receipts map[string]chan struct{} // by native id: closed once claude has the message
	turn     *turnState               // the input claude is on, nil when none
	// own is the turn claude started itself to take up background work that
	// ended (background_turns), nil when none; task is the task the latest
	// task_notification named, which the next such turn is named after.
	own       *turnState
	task      string
	capsDone  bool
	stopping  bool
	killed    bool
	exited    chan struct{} // closed once the process ended and Exited was reported
	readerEnd chan struct{}
}

// turnState is what the transport knows of the input claude is on.
type turnState struct {
	native  string
	started bool
	fail    failure
}

type controlResult struct {
	response json.RawMessage
	err      error
}

// frame is the union of the stdout frames the transport reads; unknown
// fields and frame types are ignored.
type frame struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	Capabilities []string `json:"capabilities"`
	Version      string   `json:"claude_code_version"`

	// system/task_notification: background work ended
	TaskID string `json:"task_id"`
	// system/background_tasks_changed: every task running in the
	// background, decoded for that frame alone (backgroundTasks)
	Tasks json.RawMessage `json:"tasks"`

	// system/api_retry
	Attempt     int    `json:"attempt"`
	MaxRetries  int    `json:"max_retries"`
	RetryDelay  int64  `json:"retry_delay_ms"`
	ErrorStatus int    `json:"error_status"`
	Error       string `json:"error"`

	// command_lifecycle
	CommandUUID string `json:"command_uuid"`
	State       string `json:"state"`

	// assistant: content stays raw, so one field's shape never costs a frame
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ParentToolUseID *string `json:"parent_tool_use_id"`

	// result
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	TerminalReason string `json:"terminal_reason"`
	APIErrorStatus int    `json:"api_error_status"`
	// Origin says what a turn took up when no input started it: the
	// task-notification of background work that ended.
	Origin *struct {
		Kind string `json:"kind"`
	} `json:"origin"`

	// control_request / control_response
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  *struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	} `json:"response"`

	// rate_limit_event
	RateLimitInfo json.RawMessage `json:"rate_limit_info"`
}

func openFailed(reason contract.OpenFailure, format string, args ...any) error {
	return &contract.Error{Code: contract.CodeOpenFailed, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Start launches claude for a Session and waits for it to answer the
// stream-json initialize request.
func (Profile) Start(ctx context.Context, req adapter.Start) (adapter.Transport, error) {
	cfg, err := parseOpenConfig(req.OpenConfig)
	if err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "%v", err)
	}
	id := req.SessionID
	if id == "" {
		id = sessionid.NewUUID()
	}
	if !sessionid.IsUUID(id) {
		return nil, openFailed(contract.OpenConfigInvalid, "claude's session ids are UUIDs, not %q", id)
	}
	if _, err := os.Stat(cfg.Binary); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "%v", err)
	}
	// The hooks write to the Session's own spool, and the hook helper's
	// guard keeps out a hook of any other session.
	spool := sessionSpool(cfg.Spool, id)
	env := append(adapter.HostEnv(), cfg.Env...)
	env = append(env, harnesscore.EnvSpool+"="+spool, harnesscore.EnvHarnessSessionID+"="+id)
	if c := req.Credential; c != nil {
		if c.Kind != CredentialKind {
			return nil, openFailed(contract.OpenConfigInvalid, "credential kind %q, want %s", c.Kind, CredentialKind)
		}
		tok, err := readToken(c.File)
		if err != nil {
			return nil, openFailed(contract.OpenAuthRequired, "%v", err)
		}
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+tok)
	}
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "spool: %v", err)
	}
	if req.Loaded {
		// A loaded Session is its transcript: with none where claude looks,
		// claude would start a conversation of its own under the same id.
		if _, err := tclaude.Locate(id, cfg.WorkingDir, cfg.Env); err != nil {
			return nil, openFailed(contract.OpenSessionNotFound, "the loaded session's transcript is not where claude looks: %v", err)
		}
	}
	args := append(append(sessionArgs(req.Mode, id, cfg), "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"), cfg.Args...)

	t := &transport{
		id: id, report: req.Report,
		control:  map[string]chan controlResult{},
		receipts: map[string]chan struct{}{},
		exited:   make(chan struct{}), readerEnd: make(chan struct{}),
		stderr: newTailBuffer(stderrTail),
	}
	if err := t.start(cfg.Binary, args, cfg.WorkingDir, env); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "start %s: %v", cfg.Binary, err)
	}
	ictx, cancel := context.WithTimeout(ctx, initWait)
	_, err = t.request(ictx, map[string]any{"subtype": "initialize", "hooks": nil})
	cancel()
	if err != nil {
		t.Stop(context.Background(), 0)
		<-t.exited
		stderr := t.stderr.String()
		switch {
		case strings.Contains(stderr, "already in use"):
			return nil, openFailed(contract.OpenSessionInUse, "%s", lastLine(stderr))
		case strings.Contains(stderr, "No conversation found"):
			return nil, openFailed(contract.OpenSessionNotFound, "%s", lastLine(stderr))
		case ctx.Err() != nil:
			return nil, ctx.Err()
		}
		return nil, openFailed(contract.OpenConfigInvalid, "claude did not answer its initialize request: %v: %s", err, lastLine(stderr))
	}
	return t, nil
}

// sessionArgs name the session claude runs: a fresh one under its id, or the
// one it resumes. A reopened session claude never wrote a transcript for — the
// launch that opened it ended before its first entry — has nothing to resume,
// and starts under its id as a fresh one would.
func sessionArgs(mode contract.OpenMode, id string, cfg openConfig) []string {
	if mode == contract.OpenReopen {
		if _, err := tclaude.Locate(id, cfg.WorkingDir, cfg.Env); !errors.Is(err, fs.ErrNotExist) {
			return []string{"--resume", id}
		}
	}
	return []string{"--session-id", id}
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

func (t *transport) start(bin string, args []string, dir string, env []string) error {
	cmd := exec.Command(bin, args...)
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

func (t *transport) SessionID() string { return t.id }

// maxTaskDescription bounds a background task's description, in bytes.
const maxTaskDescription = 200

// backgroundTasks is claude's list of the tasks it runs in the background, in
// the contract's terms: a shell (local_bash) is a command, an agent a
// subagent.
func backgroundTasks(raw json.RawMessage) []contract.BackgroundTask {
	var tasks []struct {
		TaskID      string `json:"task_id"`
		TaskType    string `json:"task_type"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &tasks)
	out := make([]contract.BackgroundTask, 0, len(tasks))
	for _, x := range tasks {
		if x.TaskID == "" {
			continue
		}
		kind := contract.BackgroundOther
		switch {
		case x.TaskType == "local_bash":
			kind = contract.BackgroundCommand
		case strings.HasSuffix(x.TaskType, "_agent"):
			kind = contract.BackgroundSubagent
		}
		desc := x.Description
		if len(desc) > maxTaskDescription {
			cut := maxTaskDescription
			for cut > 0 && !utf8.RuneStart(desc[cut]) {
				cut--
			}
			desc = desc[:cut]
		}
		out = append(out, contract.BackgroundTask{ID: x.TaskID, Kind: kind, Description: desc})
	}
	return out
}

// NewNative is an input's id in claude's terms: a message uuid.
func (t *transport) NewNative(string) string { return sessionid.NewUUID() }

// ---- reading

func (t *transport) read() {
	defer close(t.readerEnd)
	r := bufio.NewReaderSize(t.stdout, 64<<10)
	for {
		line, err := readBoundedLine(r, frameMax)
		if len(bytes.TrimSpace(line)) > 0 {
			var f frame
			if json.Unmarshal(line, &f) == nil {
				t.onFrame(&f, line)
			}
		}
		if err != nil {
			return
		}
	}
}

// readBoundedLine returns the next line without its newline; a line longer
// than max is consumed and dropped (nil, nil).
func readBoundedLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	over := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !over {
			if len(buf)+len(chunk) > max {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			if over {
				return nil, nil
			}
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if over {
				return nil, err
			}
			return buf, err
		}
	}
}

func (t *transport) onFrame(f *frame, raw []byte) {
	switch f.Type {
	case "command_lifecycle":
		t.onLifecycle(f.CommandUUID, f.State)
	case "system":
		switch f.Subtype {
		case "init":
			t.onInit(f)
			t.ownStarted()
		case "task_notification":
			t.mu.Lock()
			t.task = f.TaskID
			t.mu.Unlock()
		case "background_tasks_changed":
			t.report(adapter.Event{Kind: adapter.Background, Tasks: backgroundTasks(f.Tasks)})
		case "api_retry":
			t.mu.Lock()
			native := ""
			if t.turn != nil {
				native = t.turn.native
			}
			t.mu.Unlock()
			if native != "" {
				t.report(adapter.Event{Kind: adapter.Retrying, Native: native, Retry: contract.RetryingData{
					Attempt: f.Attempt, Max: f.MaxRetries, DelayMS: int(f.RetryDelay), HTTPStatus: f.ErrorStatus,
				}})
			}
		}
	case "assistant":
		if f.ParentToolUseID != nil && *f.ParentToolUseID != "" {
			return // a subagent's
		}
		var tag struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &tag)
		if tag.Error == "" || f.Message == nil {
			return
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(f.Message.Content, &blocks)
		var text []string
		for _, b := range blocks {
			if b.Type == "text" {
				text = append(text, b.Text)
			}
		}
		t.mu.Lock()
		if ts := t.current(); ts != nil {
			ts.fail.tag, ts.fail.text = tag.Error, strings.Join(text, "\n")
		}
		t.mu.Unlock()
	case "rate_limit_event":
		data, resumeAt, ok := rateLimit(f.RateLimitInfo)
		if !ok {
			return
		}
		t.mu.Lock()
		native := ""
		if t.turn != nil {
			native = t.turn.native
			if data.Status == "rejected" {
				t.turn.fail.rejected, t.turn.fail.resetsAt = true, resumeAt
			}
		}
		t.mu.Unlock()
		t.report(adapter.Event{Kind: adapter.RateLimited, Native: native, RateLimit: data, ResumeAt: resumeAt})
	case "result":
		t.onResult(f)
	case "control_response":
		if f.Response == nil {
			return
		}
		t.cmu.Lock()
		ch := t.control[f.Response.RequestID]
		delete(t.control, f.Response.RequestID)
		t.cmu.Unlock()
		if ch == nil {
			return
		}
		if f.Response.Subtype != "success" {
			ch <- controlResult{err: fmt.Errorf("claude refused the control request: %s", f.Response.Error)}
			return
		}
		ch <- controlResult{response: f.Response.Response}
	case "control_request":
		// At bypass claude asks the host nothing; refuse whatever it asks,
		// so it never waits on a request this transport does not serve.
		_ = t.write(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": f.RequestID, "error": "not supported by this host",
		}})
	}
}

func (t *transport) onLifecycle(uuid, state string) {
	var ev *adapter.Event
	t.mu.Lock()
	ts := t.turn
	mine := ts != nil && ts.native == uuid
	switch state {
	case "queued", "started":
		if ch := t.receipts[uuid]; ch != nil {
			close(ch)
			delete(t.receipts, uuid)
		}
		if state == "started" && mine && !ts.started {
			ts.started = true
			ev = &adapter.Event{Kind: adapter.Started, Native: uuid}
		}
	case "cancelled", "discarded", "refused":
		if ch := t.receipts[uuid]; ch != nil {
			close(ch)
			delete(t.receipts, uuid)
		}
		// Only a message claude never started is proven not to have run;
		// claude also says cancelled after the result of a turn it
		// interrupted or failed (claude 2.1.283), which ends it already.
		if mine && !ts.started {
			t.turn = nil
			outcome := contract.TurnRefused
			if state == "cancelled" {
				outcome = contract.TurnCancelled
			}
			ev = &adapter.Event{Kind: adapter.Ended, Native: uuid, Outcome: outcome}
		}
	}
	t.mu.Unlock()
	if ev != nil {
		t.report(*ev)
	}
}

// onInit checks, once, that claude has what the transport relies on. A
// claude that lacks it is stopped: the turn it was on errors.
func (t *transport) onInit(f *frame) {
	t.mu.Lock()
	done := t.capsDone
	t.capsDone = true
	t.mu.Unlock()
	if done {
		return
	}
	have := map[string]bool{}
	for _, c := range f.Capabilities {
		have[c] = true
	}
	var missing []string
	for _, c := range requiredCapabilities {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return
	}
	t.mu.Lock()
	ts := t.turn
	t.turn = nil
	t.mu.Unlock()
	if ts != nil {
		t.report(adapter.Event{Kind: adapter.Ended, Native: ts.native, Outcome: contract.TurnErrored, Error: &contract.TurnError{Class: contract.ErrorInternal}})
	}
	_, _ = fmt.Fprintf(t.stderr, "claude %s lacks %s: stopped\n", f.Version, strings.Join(missing, ", "))
	go t.Stop(context.Background(), quitWait)
}

// current is the turn claude is on: an input's, or one of its own. It is
// called with t.mu held.
func (t *transport) current() *turnState {
	if t.turn != nil {
		return t.turn
	}
	return t.own
}

// ownStarted takes claude's init, which starts each of its turns, after a
// task notification while it is on no input's turn, for a turn of its own:
// one taking up background work that ended, named after the task the
// notification named. An init with no notification before it starts none.
func (t *transport) ownStarted() {
	t.mu.Lock()
	if t.turn != nil || t.own != nil || t.task == "" {
		t.mu.Unlock()
		return
	}
	native := ownNative(t.task)
	t.own, t.task = &turnState{native: native, started: true}, ""
	t.mu.Unlock()
	t.report(adapter.Event{Kind: adapter.Started, Auto: native})
}

// InterruptTurn stops claude's own turn native, if claude is on it: the
// interrupt an input's turn gets.
func (t *transport) InterruptTurn(ctx context.Context, native string) error {
	t.mu.Lock()
	on := t.own != nil && t.own.native == native
	t.mu.Unlock()
	if !on {
		return nil
	}
	return t.Interrupt(ctx)
}

// onResult ends the turn claude reports ended: by is_error and
// terminal_reason, never by subtype. A result whose origin is a task
// notification is claude's own turn's, and so is any result while it is on
// no input's.
func (t *transport) onResult(f *frame) {
	t.mu.Lock()
	var ts *turnState
	own := f.Origin != nil && f.Origin.Kind == "task-notification" || t.turn == nil && t.own != nil
	if own {
		ts, t.own = t.own, nil
	} else {
		ts, t.turn = t.turn, nil
	}
	started := ts != nil
	if own && ts == nil {
		ts = &turnState{native: ownNative(t.task)}
		if t.task == "" {
			ts.native = ownNative("result")
		}
		t.task = ""
	}
	t.mu.Unlock()
	if ts == nil {
		return
	}
	ev := adapter.Event{Kind: adapter.Ended, Native: ts.native}
	if own {
		ev.Native, ev.Auto = "", ts.native
		if !started {
			t.report(adapter.Event{Kind: adapter.Started, Auto: ts.native})
		}
	}
	switch {
	case strings.HasPrefix(f.TerminalReason, "aborted"):
		ev.Outcome = contract.TurnInterrupted
	case f.IsError:
		ev.Outcome = contract.TurnErrored
		fl := ts.fail
		fl.status = f.APIErrorStatus
		if fl.text == "" {
			fl.text = f.Result
		}
		ev.Error = fl.turnError(time.Now())
	default:
		ev.Outcome, ev.Text = contract.TurnCompleted, f.Result
	}
	t.report(ev)
}

// wait turns claude's exit into the Session's: Exited, after every frame
// claude wrote was read.
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
	for id, ch := range t.control {
		ch <- controlResult{err: errExited}
		delete(t.control, id)
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

var errExited = errors.New("claude exited")

// ---- writing

var errStdinClosed = errors.New("claude's stdin is closed")

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

// request sends a control request and waits for its response.
func (t *transport) request(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	t.cmu.Lock()
	t.ctlSeq++
	id := "req_" + strconv.Itoa(t.ctlSeq)
	ch := make(chan controlResult, 1)
	t.control[id] = ch
	t.cmu.Unlock()
	if err := t.write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		t.cmu.Lock()
		delete(t.control, id)
		t.cmu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.response, r.err
	case <-ctx.Done():
		t.cmu.Lock()
		delete(t.control, id)
		t.cmu.Unlock()
		return nil, ctx.Err()
	case <-t.exited:
		return nil, errExited
	}
}

// Submit writes the input as a user message whose uuid is its native id, and
// returns once claude has queued it.
func (t *transport) Submit(ctx context.Context, s adapter.Submission) error {
	t.mu.Lock()
	select {
	case <-t.exited:
		t.mu.Unlock()
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "claude exited"}
	default:
	}
	receipt := make(chan struct{})
	t.receipts[s.Native] = receipt
	t.turn = &turnState{native: s.Native}
	t.mu.Unlock()
	forget := func() {
		t.mu.Lock()
		delete(t.receipts, s.Native)
		if t.turn != nil && t.turn.native == s.Native {
			t.turn = nil
		}
		t.mu.Unlock()
	}
	err := t.write(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": s.Text},
		"parent_tool_use_id": nil,
		"session_id":         "",
		"uuid":               s.Native,
	})
	if errors.Is(err, errStdinClosed) {
		forget()
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "claude exited"}
	}
	if err != nil {
		return fmt.Errorf("writing the message: %w", err)
	}
	timer := time.NewTimer(submitWait)
	defer timer.Stop()
	select {
	case <-receipt:
		return nil
	case <-t.exited:
		return fmt.Errorf("claude exited before it received the message")
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("claude did not receive the message within %s", submitWait)
	}
}

// Interrupt asks claude to stop the turn it is on: one control request,
// answered with a receipt. The turn's result follows.
func (t *transport) Interrupt(ctx context.Context) error {
	_, err := t.request(ctx, map[string]any{"subtype": "interrupt"})
	return err
}

// Answer: at bypass claude raises no prompts.
func (t *transport) Answer(context.Context, string, contract.Choice) error {
	return contract.Errorf(contract.CodeUnsupported, "claude raises no prompts at bypass")
}

// Stop ends claude's process group: an idle claude is asked to quit by
// closing its stdin; then the group gets SIGTERM, and SIGKILL once grace
// ends. It reports whether the group is gone.
func (t *transport) Stop(ctx context.Context, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	t.mu.Lock()
	t.stopping = true
	busy := t.turn != nil || t.own != nil
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

// gone reports whether claude and every process left in its group ended.
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
	if over := len(b.buf) - b.n; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// lastLine is s's last nonempty line, cut to 512 bytes.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	l := strings.TrimSpace(lines[len(lines)-1])
	if len(l) > 512 {
		l = l[:512]
	}
	return l
}
