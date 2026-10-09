package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/internal/procgroup"
	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/internal/proc"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// The RPC transport: pi runs as
//
//	pi --mode rpc --session-id ID --session-dir DIR -e EXTENSION <open_config args>
//
// on pipes, one process for the Session. A command is a JSON line on its
// stdin; pi answers each with a response carrying the command's id, and
// streams the session's events. An input is a prompt whose message starts
// with the input's tag (<!--hw:NATIVE-->), which the tag extension takes off
// and writes into the session; pi answers "started", and the run ends at
// agent_settled, its outcome the run's last assistant message.

const (
	// frameMax bounds one stdout line; a longer one is skipped whole.
	frameMax = 32 << 20
	// readyWait bounds the get_state and get_commands round trips at start.
	readyWait = time.Minute
	// submitWait bounds the wait for a prompt's answer.
	submitWait = 2 * time.Minute
	// drainGrace is how long stdout may still be read after pi exits: a
	// descendant that inherited the pipe keeps it open.
	drainGrace = 2 * time.Second
	// quitWait is how long an idle pi may take to exit on stdin EOF.
	quitWait = 3 * time.Second
	// stderrTail is how much of pi's stderr an exit's detail keeps.
	stderrTail = 4 << 10
)

// killWait bounds Stop's wait for the group to end after SIGKILL: a process
// of it nobody reaps (the Host as PID 1) never does. A var for tests.
var killWait = 5 * time.Second

// groupEmpty reports whether a process group ended; a var for tests.
var groupEmpty = procgroup.Empty

// retryCancelled is the finalError of the auto_retry_end pi emits when an
// abort ends the wait before a retry: the failed attempt is already dropped,
// and pi writes nothing more for the run.
const retryCancelled = "Retry cancelled"

// abortedMessage is the errorMessage pi records for a run it aborted.
const abortedMessage = "The operation was aborted."

// tagCommand is the tag extension's command: get_commands lists it once the
// extension loaded.
const tagCommand = "hw-tag"

type transport struct {
	id      string
	version string // pi's, from its package.json; "" when unknown
	scratch string // the layout's scratch: the interrupt notes
	report  func(adapter.Event)
	cmd     *exec.Cmd
	stdout  *os.File
	stderr  *proc.TailBuffer

	wmu         sync.Mutex // serializes lines on stdin, and closing it
	stdin       io.WriteCloser
	stdinClosed bool

	cmu     sync.Mutex // guards the calls awaiting an answer
	seq     int
	waiting map[string]chan rpcResult

	// rmu serializes reports: a change that reports holds it from the change
	// to the report, so events reach the Session in the order they happened,
	// from one goroutine at a time.
	rmu sync.Mutex

	mu        sync.Mutex
	turn      *turnState // the input pi is on, nil when none
	stopping  bool
	killed    bool
	exited    chan struct{} // closed once the process ended and Exited was reported
	readerEnd chan struct{}
}

// turnState is what the transport knows of the input pi is on.
type turnState struct {
	native      string
	started     bool
	interrupted bool
	// cancelled is set when an abort ended pi's wait before a retry.
	cancelled bool
	last      *assistantMessage // the run's last assistant message so far
}

// assistantMessage is what the transport keeps of an assistant message.
type assistantMessage struct {
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Content      json.RawMessage `json:"content"`
}

// text is the message's text blocks, joined.
func (m *assistantMessage) text() string {
	var out []string
	for _, b := range blocksOf(m.Content) {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "")
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func blocksOf(raw json.RawMessage) []contentBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []contentBlock{{Type: "text", Text: s}}
	}
	var out []contentBlock
	_ = json.Unmarshal(raw, &out)
	return out
}

// rpcResult is a command's response, or why none came.
type rpcResult struct {
	resp rpcResponse
	err  error
}

// rpcResponse is pi's answer to one command.
type rpcResponse struct {
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// line is the union of the stdout lines the transport reads; unknown fields
// and types are ignored.
type line struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	rpcResponse

	// message_end
	Message *struct {
		Role string `json:"role"`
		assistantMessage
	} `json:"message"`

	// auto_retry_start, auto_retry_end (whose success is the response's)
	Attempt      int    `json:"attempt"`
	MaxAttempts  int    `json:"maxAttempts"`
	DelayMS      int    `json:"delayMs"`
	ErrorMessage string `json:"errorMessage"`
	FinalError   string `json:"finalError"`

	// extension_ui_request
	Method string `json:"method"`
}

func openFailed(reason contract.OpenFailure, format string, args ...any) error {
	return &contract.Error{Code: contract.CodeOpenFailed, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// sessionIDRE is pi's rule for a session id.
var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// Start writes the credential into auth.json under the model's provider,
// launches pi for a Session, and waits for it to answer get_state for that
// session and to list the tag extension's command.
func (Profile) Start(ctx context.Context, req adapter.Start) (adapter.Transport, error) {
	cfg, err := parseOpenConfig(req.OpenConfig)
	if err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "%v", err)
	}
	id := req.SessionID
	if id == "" {
		id = sessionid.NewUUID()
	}
	if !sessionIDRE.MatchString(id) {
		return nil, openFailed(contract.OpenConfigInvalid, "%q is not a pi session id", id)
	}
	if _, err := os.Stat(cfg.Binary); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "%v", err)
	}
	if _, err := os.Stat(cfg.Extension); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "the tag extension: %v", err)
	}
	// pi's version, for Open's result and the version policy: the one the
	// release's package.json beside it says, which pi reads its own from.
	// Under flexible a pi other than the pin runs on: get_state and the tag
	// extension's command still decide whether the profile can drive it.
	version := packageVersion(cfg.Binary)
	if err := adapter.CheckHarnessVersion(cfg.VersionPolicy, "pi", pinned(), version); err != nil {
		return nil, err
	}
	auth := map[string]any{}
	if c := req.Credential; c != nil {
		if c.Kind != CredentialKind {
			return nil, openFailed(contract.OpenConfigInvalid, "credential kind %q, want %s", c.Kind, CredentialKind)
		}
		key, err := proc.ReadToken(c.File)
		if err != nil {
			return nil, openFailed(contract.OpenAuthRequired, "%v", err)
		}
		if subscriptionToken(key) {
			return nil, openFailed(contract.OpenConfigInvalid, "an %s is a provider's API key: a subscription token is not one", CredentialKind)
		}
		auth[cfg.Provider] = map[string]any{"type": "api_key", "key": key}
	}
	if err := writeAuth(cfg.AgentDir, auth); err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "auth.json: %v", err)
	}
	if err := os.MkdirAll(cfg.SessionDir, 0o700); err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "the session dir: %v", err)
	}
	env := append(adapter.HostEnv(), cfg.Env...)
	args := append([]string{"--mode", "rpc", "--session-id", id, "--session-dir", cfg.SessionDir, "-e", cfg.Extension}, cfg.Args...)

	t := &transport{
		id: id, version: version, scratch: req.Layout.Scratch, report: req.Report,
		waiting: map[string]chan rpcResult{},
		exited:  make(chan struct{}), readerEnd: make(chan struct{}),
		stderr: proc.NewTailBuffer(stderrTail),
	}
	if err := t.start(cfg.Binary, args, cfg.WorkingDir, env); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "start %s: %v", cfg.Binary, err)
	}
	fail := func(err error) error {
		t.abandon()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	ictx, cancel := context.WithTimeout(ctx, readyWait)
	defer cancel()
	st, err := t.call(ictx, map[string]any{"type": "get_state"})
	if err != nil || !st.Success {
		return nil, fail(openFailed(contract.OpenConfigInvalid, "pi did not answer get_state: %v %s: %s", err, st.Error, proc.LastLine(t.stderr.String())))
	}
	var state struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(st.Data, &state) != nil || state.SessionID != id {
		return nil, fail(openFailed(contract.OpenConfigInvalid, "pi opened session %q, not %q", state.SessionID, id))
	}
	cmds, err := t.call(ictx, map[string]any{"type": "get_commands"})
	if err != nil || !cmds.Success || !hasCommand(cmds.Data, tagCommand) {
		return nil, fail(openFailed(contract.OpenConfigInvalid, "pi did not load the tag extension %s (%v %s)", cfg.Extension, err, cmds.Error))
	}
	return t, nil
}

// hasCommand reports whether a get_commands answer lists an extension's
// command name.
func hasCommand(data json.RawMessage, name string) bool {
	var d struct {
		Commands []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"commands"`
	}
	_ = json.Unmarshal(data, &d)
	for _, c := range d.Commands {
		if c.Name == name && c.Source == "extension" {
			return true
		}
	}
	return false
}

// subscriptionToken reports whether a key is a subscription's OAuth token
// rather than an API key: Anthropic's (sk-ant-oat…), which pi would send as
// a bearer token in a subscription's mode.
func subscriptionToken(key string) bool { return strings.HasPrefix(key, "sk-ant-oat") }

// writeAuth replaces auth.json in the agent dir with auth: a file renamed
// over the old one, never a half-written credential.
func writeAuth(dir string, auth map[string]any) error {
	b, err := json.Marshal(auth)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, authFile))
}

func (t *transport) start(bin string, args []string, dir string, env []string) error {
	cmd, stdin, stdout, err := proc.Start(bin, args, dir, env, t.stderr)
	if err != nil {
		return err
	}
	t.cmd, t.stdin, t.stdout = cmd, stdin, stdout
	go t.read()
	go t.wait()
	return nil
}

func (t *transport) SessionID() string { return t.id }

// HarnessVersion is pi's version, from its release's package.json.
func (t *transport) HarnessVersion() string { return t.version }

// packageVersion is the version the package.json beside pi's executable
// names, "" when there is none to read.
func packageVersion(bin string) string {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(bin), "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return ""
	}
	return pkg.Version
}

// NewNative is an input's id in pi's terms: the tag the extension records.
func (t *transport) NewNative(string) string { return sessionid.NewUUID() }

// ---- reading

func (t *transport) read() {
	defer close(t.readerEnd)
	r := bufio.NewReaderSize(t.stdout, 64<<10)
	for {
		raw, err := proc.ReadBoundedLine(r, frameMax)
		if len(bytes.TrimSpace(raw)) > 0 {
			var l line
			if json.Unmarshal(raw, &l) == nil {
				t.onLine(&l)
			}
		}
		if err != nil {
			return
		}
	}
}

func (t *transport) onLine(l *line) {
	switch l.Type {
	case "response":
		t.cmu.Lock()
		ch := t.waiting[l.ID]
		delete(t.waiting, l.ID)
		t.cmu.Unlock()
		if ch != nil {
			ch <- rpcResult{resp: l.rpcResponse}
		}
	case "agent_start":
		t.started()
	case "message_end":
		if l.Message == nil || l.Message.Role != "assistant" {
			return
		}
		t.mu.Lock()
		if t.turn != nil {
			m := l.Message.assistantMessage
			t.turn.last = &m
		}
		t.mu.Unlock()
	case "auto_retry_start":
		t.rmu.Lock()
		defer t.rmu.Unlock()
		t.mu.Lock()
		ts := t.turn
		t.mu.Unlock()
		if ts == nil {
			return
		}
		t.report(adapter.Event{Kind: adapter.Retrying, Native: ts.native, Time: time.Now(), Retry: contract.RetryingData{
			Attempt: l.Attempt, Max: l.MaxAttempts, DelayMS: l.DelayMS, HTTPStatus: httpStatus(l.ErrorMessage),
		}})
	case "auto_retry_end":
		t.mu.Lock()
		if t.turn != nil && !l.Success && l.FinalError == retryCancelled {
			t.turn.cancelled = true
		}
		t.mu.Unlock()
	case "agent_settled":
		t.settled()
	case "extension_ui_request":
		// pi raises no prompts of its own; an extension's dialog is cancelled
		// rather than left to hold the run.
		switch l.Method {
		case "select", "confirm", "input", "editor":
			_ = t.write(map[string]any{"type": "extension_ui_response", "id": l.ID, "cancelled": true})
		}
	}
}

// started reports the turn of the input pi is on as begun, once.
func (t *transport) started() {
	t.rmu.Lock()
	defer t.rmu.Unlock()
	t.startedLocked()
}

// startedLocked is started, its caller holding rmu.
func (t *transport) startedLocked() {
	t.mu.Lock()
	ts := t.turn
	first := ts != nil && !ts.started
	if first {
		ts.started = true
	}
	t.mu.Unlock()
	if first {
		t.report(adapter.Event{Kind: adapter.Started, Native: ts.native, Time: time.Now()})
	}
}

// settled ends the turn of the input pi is on: pi has nothing more to do for
// it. Its outcome is the run's last assistant message.
func (t *transport) settled() {
	t.rmu.Lock()
	defer t.rmu.Unlock()
	t.startedLocked()
	t.mu.Lock()
	ts := t.turn
	t.turn = nil
	t.mu.Unlock()
	if ts == nil {
		return
	}
	outcome, text, terr := ended(ts.last, ts.interrupted, time.Now())
	if ts.interrupted && ts.cancelled {
		// The interrupt ended the wait before a retry: the failure before it
		// was pi's to retry, not the run's end.
		outcome, text, terr = contract.TurnInterrupted, "", nil
	}
	t.report(adapter.Event{Kind: adapter.Ended, Native: ts.native, Time: time.Now(), Outcome: outcome, Text: text, Error: terr})
}

// ended is how a run whose last assistant message is last ended: stop
// completes it, aborted interrupts it, and an error fails it — unless the
// profile interrupted it, mid-tool, where pi records the abort as an error.
func ended(last *assistantMessage, interrupted bool, at time.Time) (contract.TurnOutcome, string, *contract.TurnError) {
	if last == nil {
		if interrupted {
			return contract.TurnInterrupted, "", nil
		}
		return contract.TurnErrored, "", &contract.TurnError{Class: contract.ErrorInternal}
	}
	switch last.StopReason {
	case "stop":
		text, _ := adapter.Truncate(last.text())
		return contract.TurnCompleted, text, nil
	case "aborted":
		return contract.TurnInterrupted, "", nil
	case "error":
		if interrupted && last.ErrorMessage == abortedMessage {
			return contract.TurnInterrupted, "", nil
		}
		return contract.TurnErrored, "", failure(last.ErrorMessage, at)
	case "length":
		return contract.TurnErrored, "", &contract.TurnError{Class: contract.ErrorMaxOutput}
	}
	return contract.TurnErrored, "", &contract.TurnError{Class: contract.ErrorInternal}
}

// wait turns pi's exit into the Session's: Exited, after every line pi
// wrote was read. A turn still open gets no Ended, as with the other
// profiles: session_exited is the last of the Session, its turn's included.
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
	for id, ch := range t.waiting {
		ch <- rpcResult{err: errExited}
		delete(t.waiting, id)
	}
	t.cmu.Unlock()

	t.mu.Lock()
	stopping, killed := t.stopping, t.killed
	t.mu.Unlock()
	exit := proc.Exit(t.cmd, err, stopping, killed, t.stderr.String())
	t.rmu.Lock()
	t.report(adapter.Event{Kind: adapter.Exited, Exit: exit})
	t.rmu.Unlock()
	close(t.exited)
}

var errExited = errors.New("pi exited")

// ---- writing

var errStdinClosed = errors.New("pi's stdin is closed")

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

// call sends a command and waits for pi's answer.
func (t *transport) call(ctx context.Context, cmd map[string]any) (rpcResponse, error) {
	t.cmu.Lock()
	t.seq++
	id := "hw" + strconv.Itoa(t.seq)
	ch := make(chan rpcResult, 1)
	t.waiting[id] = ch
	t.cmu.Unlock()
	cmd["id"] = id
	if err := t.write(cmd); err != nil {
		t.cmu.Lock()
		delete(t.waiting, id)
		t.cmu.Unlock()
		return rpcResponse{}, err
	}
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		t.cmu.Lock()
		delete(t.waiting, id)
		t.cmu.Unlock()
		return rpcResponse{}, ctx.Err()
	case <-t.exited:
		return rpcResponse{}, errExited
	}
}

// Submit prompts pi with the input behind its tag, and returns once pi has
// accepted it. A prompt pi refuses — a run is busy, no key for the provider
// — reached no run.
func (t *transport) Submit(ctx context.Context, s adapter.Submission) error {
	t.mu.Lock()
	select {
	case <-t.exited:
		t.mu.Unlock()
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "pi exited"}
	default:
	}
	if t.turn != nil {
		t.mu.Unlock()
		return &contract.Error{Code: contract.CodeBusy, Certainty: contract.NotSubmitted, Message: "pi is on another input"}
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
	cctx, cancel := context.WithTimeout(ctx, submitWait)
	defer cancel()
	resp, err := t.call(cctx, map[string]any{"type": "prompt", "message": "<!--hw:" + s.Native + "-->\n" + s.Text})
	switch {
	case errors.Is(err, errStdinClosed):
		forget()
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "pi exited"}
	case err != nil:
		return fmt.Errorf("pi did not answer the prompt: %w", err)
	case !resp.Success:
		forget()
		return fmt.Errorf("%w: pi refused the prompt: %s", adapter.ErrNotSubmitted, resp.Error)
	}
	var d struct {
		Disposition string `json:"disposition"`
	}
	_ = json.Unmarshal(resp.Data, &d)
	switch d.Disposition {
	case "started":
		t.started()
	case "handled":
		// Taken by a command, with no run: the tag keeps every input from
		// reading as one, so this is not expected.
		t.settled()
	}
	return nil
}

// Interrupt asks pi to stop the run it is on. The profile notes, durably and
// first, that it interrupted the input: pi records an abort mid-tool as an
// error, and the note is how the record tells it from a failure.
func (t *transport) Interrupt(ctx context.Context) error {
	t.mu.Lock()
	ts := t.turn
	t.mu.Unlock()
	if ts != nil {
		// Marked only once noted: an interrupt that sends no abort leaves
		// the run's end read as pi recorded it.
		if err := noteInterrupt(t.scratch, ts.native); err != nil {
			return fmt.Errorf("noting the interrupt: %w", err)
		}
		t.mu.Lock()
		ts.interrupted = true
		t.mu.Unlock()
	}
	_, err := t.call(ctx, map[string]any{"type": "abort"})
	return err
}

// Answer: pi raises no prompts.
func (t *transport) Answer(context.Context, string, contract.Choice) error {
	return contract.Errorf(contract.CodeUnsupported, "pi raises no prompts")
}

// Stop ends pi's process group: an idle pi is asked to quit by closing its
// stdin; then the group gets SIGTERM, on which pi also ends the tool
// commands it runs in sessions of their own, and SIGKILL once grace ends. It
// reports whether the group is gone.
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

// gone reports whether pi and every process left in its group ended.
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
