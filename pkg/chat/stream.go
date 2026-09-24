package chat

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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/olesho/harness-wrapper/internal/delivery"
	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// The stream-json transport (ADR-009): claude-code runs as
//
//	claude -p --input-format stream-json --output-format stream-json --verbose --include-partial-messages
//
// on pipes, one process for the life of the Conversation. A user message is
// one NDJSON frame on its stdin, carrying a uuid chat mints; claude answers
// with frames on its stdout: command_lifecycle receipts for that uuid,
// system/init, stream events, assistant messages, one result per turn,
// control responses. Nothing is read off a screen, so none of the TUI's
// screen machinery runs: no busy gate, no idle completion, no keystroke
// interrupt. What stays shared with the TUI is everything above the harness:
// the Store, the turn records and their events, the delivery queue and
// EventExited (ADR-008), the control token, and History from the transcript.

// streamRequiredCapabilities are the protocol features the driver relies on;
// claude advertises them on every system/init. Without the interrupt receipt
// an Interrupt could not be confirmed, and without message lifecycles a Send
// could not be.
var streamRequiredCapabilities = []string{"interrupt_receipt_v1", "msg_lifecycle_v1"}

const (
	// streamFrameMax bounds one stdout frame. A longer line — a tool result
	// the size of a file tree — is skipped whole, never half-parsed.
	streamFrameMax = 32 << 20
	// streamSubmitWait bounds how long Send waits for claude's receipt of a
	// message (command_lifecycle queued or started).
	streamSubmitWait = 2 * time.Minute
	// streamInitWait bounds Open's initialize round trip.
	streamInitWait = time.Minute
	// streamDrainGrace is how long the reader may keep draining stdout after
	// claude exits: a descendant that inherited the pipe keeps it open, and
	// its output is not claude's.
	streamDrainGrace = 2 * time.Second
	// streamStopGrace is how long Close waits after closing stdin, and again
	// after SIGTERM, before escalating.
	streamStopGrace = 3 * time.Second
	// streamStderrTail is how much of claude's stderr an exit reason keeps.
	streamStderrTail = 4 << 10
)

// streamProc is the claude process of a stream-json Conversation.
type streamProc struct {
	cmd *exec.Cmd

	wmu         sync.Mutex // serializes frames on stdin, and closing it
	stdin       io.WriteCloser
	stdinClosed bool

	stdout *os.File
	stderr *tailBuffer

	lastFrameAt atomic.Int64 // Unix nanos of the last stdout frame

	cmu     sync.Mutex
	control map[string]chan streamControlResult // pending control requests by id

	// Guarded by the Conversation's mu.
	turn          *streamTurn // the in-flight turn's protocol state, nil when none
	capsChecked   bool
	mode          string // claude's permission mode, from system/init or set_permission_mode
	retrying      string // the api_retry the open turn is in, "" when none
	retryingSince time.Time
	permission    *streamPermission // a can_use_tool request awaiting an answer
}

// streamTurn is the protocol state of the turn in flight.
type streamTurn struct {
	uuid        string
	receipt     chan struct{} // closed once claude has queued or started the message
	receiptOnce sync.Once
	text        strings.Builder // the reply as it streams
	output      bool            // the turn produced text or a tool call
	apiError    string          // the error tag on the turn's synthetic assistant message
	interrupt   *streamInterruptOp
}

func (st *streamTurn) received() { st.receiptOnce.Do(func() { close(st.receipt) }) }

// streamInterruptOp is the one Interrupt of a turn; concurrent callers join it.
type streamInterruptOp struct {
	done   chan struct{}
	once   sync.Once
	result InterruptResult
	err    error
}

func (op *streamInterruptOp) finish(result InterruptResult, err error) {
	op.once.Do(func() {
		op.result, op.err = result, err
		close(op.done)
	})
}

// streamPermission is a can_use_tool request claude is blocked on.
type streamPermission struct {
	requestID string
	input     json.RawMessage
	request   InputRequest
	surfaced  bool
}

type streamControlResult struct {
	response json.RawMessage
	err      error
}

// streamFrame is the union of the stdout frames the driver reads. Decoding is
// tolerant: fields it does not know are ignored, and so are frame types.
type streamFrame struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	SessionID      string   `json:"session_id"`
	Capabilities   []string `json:"capabilities"`
	PermissionMode string   `json:"permissionMode"`
	Version        string   `json:"claude_code_version"`

	// system/api_retry
	Attempt     int    `json:"attempt"`
	MaxRetries  int    `json:"max_retries"`
	RetryDelay  int64  `json:"retry_delay_ms"`
	ErrorStatus int    `json:"error_status"`
	Error       string `json:"error"`

	// system/session_state_changed
	State string `json:"state"`

	// command_lifecycle
	CommandUUID string `json:"command_uuid"`

	// stream_event
	Event *struct {
		Type         string `json:"type"`
		ContentBlock *struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`

	// assistant. Content stays raw: a user frame's is a string, an
	// assistant's a list of blocks, and one field's shape must never cost a
	// whole frame.
	Message *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ParentToolUseID *string `json:"parent_tool_use_id"`

	// result
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	TerminalReason string `json:"terminal_reason"`
	APIErrorStatus int    `json:"api_error_status"`

	// control_request / control_response
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  *struct {
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Response  json.RawMessage `json:"response"`
		Error     string          `json:"error"`
	} `json:"response"`
}

// openStream is openWithSession for TransportStreamJSON.
func openStream(ctx context.Context, opts Options, session Session, persist bool, adapter turns.Adapter, harnessDir string) (*Conversation, error) {
	if !isClaudeCode(opts.Harness) {
		return nil, fmt.Errorf("%w: transport %s is for claude-code, not %q", ErrInvalidOptions, TransportStreamJSON, opts.Harness)
	}
	if opts.Containment != nil || session.Containment != nil {
		return nil, fmt.Errorf("%w: containment is not available on transport %s", ErrInvalidOptions, TransportStreamJSON)
	}
	if bad := firstSessionControlConflict(opts.Args, streamReservedFlags); bad != "" {
		return nil, fmt.Errorf("%w: argument %s conflicts with transport %s, which sets it", ErrInvalidOptions, bad, TransportStreamJSON)
	}
	configureAdapterEnv(adapter, opts.Env)

	sessionArgs, harnessID, err := sessionLaunch(adapter, opts, harnessDir, false)
	if err != nil {
		return nil, err
	}
	if len(sessionArgs) > 0 {
		if scf, ok := adapter.(turns.SessionControlFlags); ok {
			if bad := firstSessionControlConflict(opts.Args, scf.SessionControlFlags()); bad != "" {
				return nil, fmt.Errorf("%w: argument %s conflicts with chat-managed session control; use Options.HarnessSessionID, Options.Resume or Reopen", ErrInvalidOptions, bad)
			}
		}
		session = session.withHarnessID(harnessID)
	}

	args := append(append(append([]string{}, sessionArgs...), streamBaseArgs...), opts.Args...)
	launchRung := wrapper.EffectiveLaunchRung(opts.Harness, args, opts.PermissionMode)
	if launchRung != "bypass" {
		// Permission prompts come to the Conversation as can_use_tool control
		// requests (InputPolicy, OnInputRequest, then Answer). At bypass
		// there are none.
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	args, err = wrapper.HarnessArgs(wrapper.Config{
		BinaryPath: opts.BinaryPath, Args: args, WorkingDir: opts.WorkingDir, Env: opts.Env,
		Harness: opts.Harness, Effort: opts.Effort, Model: opts.Model, PermissionMode: opts.PermissionMode,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: chat: %w", ErrInvalidOptions, err)
	}

	env := opts.Env
	if env == nil {
		env = os.Environ()
	}
	env = append(append([]string{}, env...), "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1")

	proc, err := startStreamProc(opts.BinaryPath, args, opts.WorkingDir, env)
	if err != nil {
		return nil, fmt.Errorf("chat: start %s: %w", opts.BinaryPath, err)
	}

	c := &Conversation{
		opts:                    opts,
		store:                   opts.Store,
		adapter:                 adapter,
		queue:                   newControlQueue(),
		session:                 session,
		harnessDir:              harnessDir,
		eventCh:                 make(chan ConversationEvent, opts.EventBuffer),
		abandoned:               make(chan struct{}),
		done:                    make(chan struct{}),
		inputStateCh:            make(chan struct{}, 1),
		markerArmCh:             make(chan struct{}, 1),
		closed:                  make(chan struct{}),
		sentTranscriptWatermark: watermarkUnknown,
		stream:                  proc,
	}
	c.delivery = delivery.New(delivery.Limits(opts.EventQueue), eventSize, c.deliver)
	go func() {
		<-c.delivery.Drained()
		close(c.eventCh)
	}()

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		c.streamRead()
	}()
	go c.streamWait(readerDone)

	fail := func(err error) (*Conversation, error) {
		c.stopStream(context.Background())
		<-c.done
		return nil, err
	}
	ictx, cancel := context.WithTimeout(ctx, streamInitWait)
	_, err = c.streamControl(ictx, map[string]any{"subtype": "initialize", "hooks": nil})
	cancel()
	if err != nil {
		return fail(fmt.Errorf("chat: %s did not answer the stream-json initialize request: %w", opts.BinaryPath, err))
	}
	if persist {
		rec := session.clone()
		if err := opts.Store.CreateSession(ctx, &rec); err != nil {
			return fail(fmt.Errorf("chat: store CreateSession: %w", err))
		}
	}
	return c, nil
}

// streamBaseArgs puts claude into the persistent stream-json mode.
var streamBaseArgs = []string{
	"-p", "--input-format", "stream-json", "--output-format", "stream-json",
	"--verbose", "--include-partial-messages",
}

// streamReservedFlags are the flags the transport sets; Options.Args may not.
var streamReservedFlags = []string{
	"-p", "--print", "--input-format", "--output-format", "--include-partial-messages",
	"--replay-user-messages", "--permission-prompt-tool",
}

func isClaudeCode(harness string) bool {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude", chatClaudeCode:
		return true
	}
	return false
}

func startStreamProc(bin string, args []string, dir string, env []string) (*streamProc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	setStreamProcAttr(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// stdout and stderr are plain pipes the Conversation reads itself, so
	// Wait never waits on a descendant that inherited them (streamWait).
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_ = outR.Close()
		_ = outW.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if err := cmd.Start(); err != nil {
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return nil, err
	}
	_ = outW.Close()
	_ = errW.Close()
	p := &streamProc{
		cmd: cmd, stdin: stdin, stdout: outR,
		stderr:  newTailBuffer(streamStderrTail),
		control: map[string]chan streamControlResult{},
	}
	go func() {
		_, _ = io.Copy(p.stderr, errR)
		_ = errR.Close()
	}()
	return p, nil
}

// streamWait turns claude's exit into the Conversation's: the in-flight turn
// ends, and EventExited is the last event (ADR-008).
func (c *Conversation) streamWait(readerDone <-chan struct{}) {
	p := c.stream
	err := p.cmd.Wait()
	select {
	case <-readerDone:
	case <-time.After(streamDrainGrace):
		_ = p.stdout.Close() // a descendant holds the pipe open
		<-readerDone
	}
	info := ExitInfo{Status: wrapper.StatusIdle, EndedAt: time.Now()}
	if ps := p.cmd.ProcessState; ps != nil {
		info.ExitCode = ps.ExitCode()
		info.Signal = streamExitSignal(ps)
	}
	if err != nil || info.ExitCode != 0 || info.Signal != "" {
		info.Status = wrapper.StatusFailed
		info.Reason = strings.TrimSpace(oneLineCapped(p.stderr.String(), apiErrorDetailCap))
		if info.Reason == "" && err != nil {
			info.Reason = err.Error()
		}
	}
	p.wmu.Lock()
	p.stdinClosed = true
	_ = p.stdin.Close()
	p.wmu.Unlock()
	p.cmu.Lock()
	for id, ch := range p.control {
		ch <- streamControlResult{err: ErrExited}
		delete(p.control, id)
	}
	p.cmu.Unlock()
	c.mu.Lock()
	st := p.turn
	p.turn = nil
	perm := p.permission
	p.permission = nil
	c.mu.Unlock()
	if st != nil && st.interrupt != nil {
		st.interrupt.finish("", fmt.Errorf("%w: %w", ErrInterruptUnconfirmed, ErrExited))
	}
	if perm != nil && perm.surfaced {
		c.emit(ConversationEvent{Type: EventInputResolved, Input: &InputRequest{ID: perm.requestID}})
	}
	c.exitWith(info)
}

// streamRead reads claude's stdout until it closes.
func (c *Conversation) streamRead() {
	r := bufio.NewReaderSize(c.stream.stdout, 64<<10)
	for {
		line, err := readBoundedLine(r, streamFrameMax)
		if len(bytes.TrimSpace(line)) > 0 {
			c.stream.lastFrameAt.Store(time.Now().UnixNano())
			var f streamFrame
			if json.Unmarshal(line, &f) == nil {
				c.onStreamFrame(&f, line)
			}
		}
		if err != nil {
			return
		}
	}
}

// readBoundedLine returns the next line without its newline. A line longer
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

func (c *Conversation) onStreamFrame(f *streamFrame, raw []byte) {
	switch f.Type {
	case "command_lifecycle":
		c.onStreamLifecycle(f)
	case "system":
		switch f.Subtype {
		case "init":
			c.onStreamInit(f)
		case "api_retry":
			c.mu.Lock()
			c.stream.retrying = fmt.Sprintf("api error %d %s: retrying, attempt %d/%d in %dms",
				f.ErrorStatus, f.Error, f.Attempt, f.MaxRetries, f.RetryDelay)
			c.stream.retryingSince = time.Now()
			c.mu.Unlock()
		}
	case "stream_event":
		if f.Event == nil || f.ParentToolUseID != nil && *f.ParentToolUseID != "" {
			return // a sub-agent's stream is not the reply
		}
		c.mu.Lock()
		if st := c.stream.turn; st != nil {
			switch {
			case f.Event.Delta != nil && f.Event.Delta.Type == "text_delta":
				st.text.WriteString(f.Event.Delta.Text)
				st.output = true
				c.stream.retrying = ""
			case f.Event.ContentBlock != nil && f.Event.ContentBlock.Type == "tool_use":
				st.output = true
				c.stream.retrying = ""
			}
		}
		c.mu.Unlock()
	case "assistant":
		var tag struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &tag)
		c.mu.Lock()
		if st := c.stream.turn; st != nil && (f.ParentToolUseID == nil || *f.ParentToolUseID == "") {
			if tag.Error != "" {
				st.apiError = tag.Error
			}
			if f.Message != nil {
				var blocks []struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(f.Message.Content, &blocks)
				for _, b := range blocks {
					if b.Type == "tool_use" {
						st.output = true
					}
				}
			}
		}
		c.mu.Unlock()
	case "result":
		c.onStreamResult(f)
	case "control_response":
		if f.Response == nil {
			return
		}
		c.stream.cmu.Lock()
		ch := c.stream.control[f.Response.RequestID]
		delete(c.stream.control, f.Response.RequestID)
		c.stream.cmu.Unlock()
		if ch == nil {
			return
		}
		if f.Response.Subtype != "success" {
			ch <- streamControlResult{err: fmt.Errorf("claude refused the control request: %s", f.Response.Error)}
			return
		}
		ch <- streamControlResult{response: f.Response.Response}
	case "control_request":
		c.onStreamControlRequest(f)
	}
}

func (c *Conversation) onStreamInit(f *streamFrame) {
	c.mu.Lock()
	if f.PermissionMode != "" {
		c.stream.mode = f.PermissionMode
	}
	checked := c.stream.capsChecked
	c.stream.capsChecked = true
	c.mu.Unlock()
	if checked {
		return
	}
	have := map[string]bool{}
	for _, cp := range f.Capabilities {
		have[cp] = true
	}
	var missing []string
	for _, cp := range streamRequiredCapabilities {
		if !have[cp] {
			missing = append(missing, cp)
		}
	}
	if len(missing) == 0 {
		return
	}
	// A claude that cannot confirm a message or an interrupt is not run: end
	// the turn with the reason and stop the process.
	c.mu.Lock()
	turn := c.claimTurnLocked()
	st := c.stream.turn
	c.stream.turn = nil
	c.mu.Unlock()
	if st != nil {
		st.received()
		if st.interrupt != nil {
			st.interrupt.finish("", fmt.Errorf("%w: claude lacks %s", ErrInterruptUnconfirmed, strings.Join(missing, ", ")))
		}
	}
	if turn != nil {
		turn.State = TurnStateErrored
		turn.CompletedAt = time.Now()
		turn.Reason = fmt.Sprintf("%s: claude %s does not support the stream-json transport: it lacks %s",
			c.opts.Harness, f.Version, strings.Join(missing, ", "))
		c.finishTurn(turn, nil)
	}
	go c.stopStream(context.Background())
}

func (c *Conversation) onStreamLifecycle(f *streamFrame) {
	c.mu.Lock()
	st := c.stream.turn
	if st == nil || st.uuid != f.CommandUUID {
		c.mu.Unlock()
		return
	}
	switch f.State {
	case "queued", "started":
		c.mu.Unlock()
		st.received()
		return
	case "completed":
		// The result frame ends the turn; it precedes this receipt.
		c.mu.Unlock()
		return
	}
	// cancelled, discarded or refused with no result: the message never ran.
	turn := c.claimTurnLocked()
	c.stream.turn = nil
	c.mu.Unlock()
	st.received()
	if turn == nil {
		return
	}
	turn.CompletedAt = time.Now()
	result := InterruptResult("")
	if f.State == "cancelled" {
		turn.State = TurnStateInterrupted
		turn.Reason = interruptReason(c.opts.Harness, InterruptCancelled, st.interrupt != nil)
		result = InterruptCancelled
	} else {
		turn.State = TurnStateErrored
		turn.Reason = c.opts.Harness + ": claude " + f.State + " the message"
	}
	c.finishTurn(turn, nil)
	if st.interrupt != nil {
		if result == "" {
			result = InterruptTooLate
		}
		st.interrupt.finish(result, nil)
	}
}

func (c *Conversation) onStreamResult(f *streamFrame) {
	c.mu.Lock()
	st := c.stream.turn
	turn := c.claimTurnLocked()
	c.stream.turn = nil
	c.stream.retrying = ""
	c.mu.Unlock()
	if turn == nil {
		return
	}
	if st != nil {
		st.received()
	}
	turn.CompletedAt = time.Now()
	interrupted := strings.HasPrefix(f.TerminalReason, "aborted")
	outcome := InterruptTooLate
	switch {
	case interrupted:
		outcome = InterruptCancelled
		if st != nil && st.output {
			outcome = InterruptStopped
		}
		turn.State = TurnStateInterrupted
		turn.Reason = interruptReason(c.opts.Harness, outcome, st != nil && st.interrupt != nil)
		if outcome == InterruptStopped && st != nil {
			turn.Text = st.text.String()
		}
	case f.IsError:
		turn.State = TurnStateErrored
		turn.HTTPCode = f.APIErrorStatus
		tag := ""
		if st != nil {
			tag = st.apiError
		}
		if v, known := apiErrorClasses[tag]; known {
			v.tag, v.text = tag, f.Result
			turn.Reason = v.turnReason(c.opts.Harness)
			turn.Code = v.code
			if v.code == CodeUsageLimited {
				turn.ResumeAt = resumeAtFrom(f.Result)
			}
		} else {
			turn.Reason = c.opts.Harness + ": " + oneLineCapped(streamErrorText(f), apiErrorDetailCap)
		}
	default:
		turn.State = TurnStateComplete
		turn.Text = f.Result
	}
	c.finishTurn(turn, nil)
	if st != nil && st.interrupt != nil {
		st.interrupt.finish(outcome, nil)
	}
}

func streamErrorText(f *streamFrame) string {
	if f.Result != "" {
		return f.Result
	}
	return "turn failed (" + f.TerminalReason + ")"
}

// onStreamControlRequest answers what claude asks of the host. can_use_tool
// is a permission prompt; anything else is refused, so claude never waits on
// a request this driver does not serve.
func (c *Conversation) onStreamControlRequest(f *streamFrame) {
	var req struct {
		Subtype     string          `json:"subtype"`
		ToolName    string          `json:"tool_name"`
		DisplayName string          `json:"display_name"`
		Description string          `json:"description"`
		Input       json.RawMessage `json:"input"`
		BlockedPath string          `json:"blocked_path"`
	}
	if json.Unmarshal(f.Request, &req) != nil || req.Subtype != "can_use_tool" {
		_ = c.streamWrite(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": f.RequestID, "error": "not supported by this host",
		}})
		return
	}
	name := req.DisplayName
	if name == "" {
		name = req.ToolName
	}
	prompt := "Allow " + name
	if req.Description != "" {
		prompt += ": " + req.Description
	}
	if req.BlockedPath != "" {
		prompt += " (" + req.BlockedPath + ")"
	}
	perm := &streamPermission{requestID: f.RequestID, input: req.Input, request: InputRequest{
		ID: f.RequestID, Kind: streamPermissionKind, Prompt: prompt + "?", Header: name,
		Options: []InputOption{{ID: "allow", Label: "Allow"}, {ID: "deny", Label: "Deny"}},
	}}
	if d, ok := c.opts.InputPolicy.resolve(streamPermissionKind); ok && d.Kind == DispositionAnswer {
		_ = c.answerStreamPermission(perm, InputAnswer{OptionID: d.OptionID})
		return
	}
	if h := c.opts.OnInputRequest; h != nil {
		if ans, ok := h(perm.request); ok {
			_ = c.answerStreamPermission(perm, ans)
			return
		}
	}
	perm.surfaced = true
	c.mu.Lock()
	c.stream.permission = perm
	c.mu.Unlock()
	req2 := perm.request
	c.emit(ConversationEvent{Type: EventInputRequest, Input: &req2})
}

// streamPermissionKind is the InputRequest.Kind of a can_use_tool prompt.
const streamPermissionKind = "permission_prompt"

func (c *Conversation) answerStreamPermission(perm *streamPermission, ans InputAnswer) error {
	var resp map[string]any
	switch ans.OptionID {
	case "allow":
		input := perm.input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		resp = map[string]any{"behavior": "allow", "updatedInput": input}
	case "deny":
		resp = map[string]any{"behavior": "deny", "message": "denied by the host"}
	default:
		return fmt.Errorf("%w: %q (want allow or deny)", ErrUnknownOption, ans.OptionID)
	}
	return c.streamWrite(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": perm.requestID, "response": resp,
	}})
}

// streamWrite writes one frame to claude's stdin.
func (c *Conversation) streamWrite(frame any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	p := c.stream
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.stdinClosed {
		return ErrExited
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err
}

// streamControl sends a control request and waits for its response.
func (c *Conversation) streamControl(ctx context.Context, request map[string]any) (json.RawMessage, error) {
	id := "req_" + newID()[:16]
	ch := make(chan streamControlResult, 1)
	p := c.stream
	p.cmu.Lock()
	p.control[id] = ch
	p.cmu.Unlock()
	if err := c.streamWrite(map[string]any{"type": "control_request", "request_id": id, "request": request}); err != nil {
		p.cmu.Lock()
		delete(p.control, id)
		p.cmu.Unlock()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.response, r.err
	case <-ctx.Done():
		p.cmu.Lock()
		delete(p.control, id)
		p.cmu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, ErrExited
	}
}

// streamSend is Send on the stream-json transport: record the turn, write the
// message with a fresh uuid, and return once claude has received it.
func (c *Conversation) streamSend(ctx context.Context, text string) (string, error) {
	assistantTurn, err := c.beginTurn(ctx, text)
	if err != nil {
		return "", err
	}
	st := &streamTurn{uuid: sessionid.NewUUID(), receipt: make(chan struct{})}
	c.mu.Lock()
	if c.currentTurn == nil || c.currentTurn.ID != assistantTurn.ID {
		// Ended already: the process exited between recording and here.
		c.mu.Unlock()
		return assistantTurn.ID, nil
	}
	c.stream.turn = st
	c.mu.Unlock()

	err = c.streamWrite(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": text},
		"parent_tool_use_id": nil,
		"session_id":         "",
		"uuid":               st.uuid,
	})
	if err != nil {
		c.dropStreamTurn(st)
		return c.failSubmit(assistantTurn.ID, err)
	}
	wait := time.NewTimer(streamSubmitWait)
	defer wait.Stop()
	select {
	case <-st.receipt:
		return assistantTurn.ID, nil
	case <-c.done:
		return assistantTurn.ID, nil // the exit ended the turn
	case <-ctx.Done():
		err = ctx.Err()
	case <-wait.C:
		err = fmt.Errorf("claude did not receive the message within %s", streamSubmitWait)
	}
	c.dropStreamTurn(st)
	return c.failSubmit(assistantTurn.ID, err)
}

func (c *Conversation) dropStreamTurn(st *streamTurn) {
	c.mu.Lock()
	if c.stream.turn == st {
		c.stream.turn = nil
	}
	c.mu.Unlock()
}

// streamInterrupt is Interrupt on the stream-json transport: one control
// request per turn, answered by claude with a receipt, and settled by the
// turn's result — stopped or cancelled, or too late when it finished first.
func (c *Conversation) streamInterrupt(ctx context.Context) (InterruptResult, error) {
	c.mu.Lock()
	st := c.stream.turn
	if st == nil {
		c.mu.Unlock()
		return InterruptNoTurn, nil
	}
	op := st.interrupt
	joined := op != nil
	if !joined {
		op = &streamInterruptOp{done: make(chan struct{})}
		st.interrupt = op
		c.interruptedBy = c.currentTurn.ID
	}
	c.mu.Unlock()
	if !joined {
		if _, err := c.streamControl(ctx, map[string]any{"subtype": "interrupt"}); err != nil {
			op.finish("", fmt.Errorf("%w: %w", ErrInterruptUnconfirmed, err))
		}
	}
	select {
	case <-op.done:
		return op.result, op.err
	case <-ctx.Done():
		return "", fmt.Errorf("%w: %w", ErrInterruptUnconfirmed, ctx.Err())
	}
}

// streamQuit closes claude's stdin: claude finishes the turn it is in, if
// any, and exits.
func (c *Conversation) streamQuit(ctx context.Context) error {
	release, err := c.queue.Acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	p := c.stream
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if !p.stdinClosed {
		p.stdinClosed = true
		return p.stdin.Close()
	}
	return nil
}

// stopStream ends the process: stdin closed, then SIGTERM to its process
// group, then SIGKILL, each after a grace or when ctx ends.
func (c *Conversation) stopStream(ctx context.Context) {
	p := c.stream
	p.wmu.Lock()
	if !p.stdinClosed {
		p.stdinClosed = true
		_ = p.stdin.Close()
	}
	p.wmu.Unlock()
	for _, kill := range []bool{false, true} {
		t := time.NewTimer(streamStopGrace)
		select {
		case <-c.done:
			t.Stop()
			return
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
		}
		signalStreamGroup(p.cmd, kill)
	}
	<-c.done
}

// streamState fills State's harness fields on the stream-json transport.
func (c *Conversation) streamState(st *State) {
	p := c.stream
	if p.cmd.Process != nil {
		st.PID = p.cmd.Process.Pid
	}
	st.Alive = st.Exit == nil
	if ns := p.lastFrameAt.Load(); ns != 0 {
		st.LastOutputAt = time.Unix(0, ns)
	}
	c.mu.Lock()
	st.Busy = c.currentTurn != nil && st.Alive
	if p.retrying != "" {
		st.Status, st.StatusReason, st.ClassifiedAt = wrapper.StatusAPIError, p.retrying, p.retryingSince
	}
	c.mu.Unlock()
}

// streamPendingInput is PendingInput on the stream-json transport.
func (c *Conversation) streamPendingInput() *InputRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if perm := c.stream.permission; perm != nil && perm.surfaced {
		r := perm.request
		return &r
	}
	return nil
}

// streamAnswer is Answer on the stream-json transport: a permission prompt
// is allowed or denied.
func (c *Conversation) streamAnswer(requestID string, ans InputAnswer) error {
	c.mu.Lock()
	perm := c.stream.permission
	switch {
	case perm == nil:
		c.mu.Unlock()
		return ErrNoInputPending
	case perm.requestID != requestID:
		c.mu.Unlock()
		return ErrStaleInputRequest
	}
	c.mu.Unlock()
	if len(ans.OptionIDs) > 0 {
		return ErrNotMultiSelect
	}
	if err := c.answerStreamPermission(perm, ans); err != nil {
		return err
	}
	c.mu.Lock()
	if c.stream.permission == perm {
		c.stream.permission = nil
	}
	c.mu.Unlock()
	c.emit(ConversationEvent{Type: EventInputResolved, Input: &InputRequest{ID: requestID}})
	return nil
}

// streamPermissionMode reads claude's current mode as a canonical rung.
func (c *Conversation) streamPermissionMode() (string, bool) {
	c.mu.Lock()
	mode := c.stream.mode
	c.mu.Unlock()
	if mode == "" {
		return "", false
	}
	return wrapper.EffectiveLaunchRung(c.opts.Harness, nil, mode), true
}

// streamSetPermissionMode switches claude's mode with a control request.
// The caller holds the control token, as for the TUI.
func (c *Conversation) streamSetPermissionMode(ctx context.Context, target string) (string, error) {
	native, err := wrapper.HarnessArgs(wrapper.Config{
		BinaryPath: c.opts.BinaryPath, Harness: c.opts.Harness, PermissionMode: target,
	})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidOptions, err)
	}
	mode := ""
	for i := 0; i+1 < len(native); i++ {
		if native[i] == "--permission-mode" {
			mode = native[i+1]
		}
	}
	if mode == "" {
		return "", fmt.Errorf("%w: permission mode %q", ErrInvalidOptions, target)
	}
	if _, err := c.streamControl(ctx, map[string]any{"subtype": "set_permission_mode", "mode": mode}); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.stream.mode = mode
	c.mu.Unlock()
	return wrapper.EffectiveLaunchRung(c.opts.Harness, nil, mode), nil
}

// tailBuffer keeps the last n bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

func newTailBuffer(n int) *tailBuffer { return &tailBuffer{n: n} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.n; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
