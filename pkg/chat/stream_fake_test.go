package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A fake claude for the stream-json transport: the test binary itself,
// re-executed with streamFakeEnv set (TestMain hands it to runStreamFake). It
// speaks the frames claude 2.1.281 was recorded speaking (agentd's P11
// captures; test/corpus/claude-code-stream): a receipt for each message's
// uuid, system/init with capabilities, streamed text, one result per turn,
// control responses. What it answers is keyed on the message text:
//
//	PING <x>     reply "PONG <x>"
//	SLOW <n>     n text chunks, 100 ms apart; interruptible
//	DELAY        2 s before the first token; interruptible
//	TOOL         a tool call, then 2 s "running"; interruptible
//	ERR529       two api_retry frames, then an error turn tagged server_error
//	LIMIT        a rejected rate_limit_event, then an error turn tagged
//	             rate_limit: "You've hit your session limit"
//	RATE <s>     a rate_limit_event with status s (default allowed), shaped as
//	             a real subscription account's, then reply "ok"
//	RATEBAD      a rate_limit_event without a status, then reply "ok"
//	PERM         a can_use_tool request; replies with what the host decided
//	REFUSE       the message is refused (no result)
//	EXIT         exit 3 mid-turn, with a line on stderr
//	anything     reply "ok"
//
// Stdin EOF finishes the turn in flight and exits 0, as claude does.
const streamFakeEnv = "HW_STREAM_FAKE"

// fakeResetsAt is the reset time the fake's rate_limit_event reports, in
// Unix seconds.
const fakeResetsAt = 1790272800

// streamFakeCapsEnv overrides the capabilities system/init advertises.
const streamFakeCapsEnv = "HW_STREAM_FAKE_CAPS"

// streamFakeArgvEnv names a file the fake writes its argv to.
const streamFakeArgvEnv = "HW_STREAM_FAKE_ARGV"

func runStreamFake() int {
	f := &streamFake{out: bufio.NewWriter(os.Stdout), session: "00000000-0000-4000-8000-000000000000", mode: "default"}
	args := os.Args[1:]
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--session-id", "--resume":
			f.session = args[i+1]
		case "--permission-mode":
			f.mode = args[i+1]
		}
	}
	if p := os.Getenv(streamFakeArgvEnv); p != "" {
		_ = os.WriteFile(p, []byte(strings.Join(args, "\n")), 0o600)
	}
	f.caps = []string{"interrupt_receipt_v1", "interrupt_cancel_queued_v1", "msg_lifecycle_v1"}
	if v, ok := os.LookupEnv(streamFakeCapsEnv); ok {
		f.caps = strings.FieldsFunc(v, func(r rune) bool { return r == ',' })
	}

	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadBytes('\n')
		if len(line) > 0 {
			f.handle(line)
		}
		if err != nil {
			break
		}
	}
	f.wg.Wait() // EOF: finish the turn in flight, then exit
	return 0
}

type streamFake struct {
	mu      sync.Mutex
	out     *bufio.Writer
	session string
	mode    string
	caps    []string
	wg      sync.WaitGroup

	turnMu  sync.Mutex
	abort   chan string // the turn in flight's interrupt, nil when none
	control map[string]chan json.RawMessage
}

func (f *streamFake) send(frame map[string]any) {
	frame["session_id"] = f.session
	b, _ := json.Marshal(frame)
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = f.out.Write(append(b, '\n'))
	_ = f.out.Flush()
}

func (f *streamFake) handle(line []byte) {
	var fr struct {
		Type      string          `json:"type"`
		UUID      string          `json:"uuid"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
		Response  *struct {
			RequestID string          `json:"request_id"`
			Response  json.RawMessage `json:"response"`
		} `json:"response"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &fr) != nil {
		return
	}
	switch fr.Type {
	case "user":
		f.send(map[string]any{"type": "command_lifecycle", "command_uuid": fr.UUID, "state": "queued"})
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.turn(fr.UUID, fr.Message.Content)
		}()
	case "control_request":
		var req struct {
			Subtype string `json:"subtype"`
			Mode    string `json:"mode"`
		}
		_ = json.Unmarshal(fr.Request, &req)
		ok := func(resp map[string]any) {
			f.send(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": fr.RequestID, "response": resp,
			}})
		}
		switch req.Subtype {
		case "initialize":
			ok(map[string]any{"pid": os.Getpid(), "account": map[string]any{}})
		case "interrupt":
			f.turnMu.Lock()
			if f.abort != nil {
				select {
				case f.abort <- "interrupt":
				default:
				}
			}
			f.turnMu.Unlock()
			ok(map[string]any{"still_queued": []string{}})
		case "set_permission_mode":
			f.mu.Lock()
			f.mode = req.Mode
			f.mu.Unlock()
			ok(map[string]any{"mode": req.Mode})
		default:
			f.send(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "error", "request_id": fr.RequestID, "error": "Unsupported control request subtype: " + req.Subtype,
			}})
		}
	case "control_response":
		if fr.Response == nil {
			return
		}
		f.turnMu.Lock()
		ch := f.control[fr.Response.RequestID]
		f.turnMu.Unlock()
		if ch != nil {
			ch <- fr.Response.Response
		}
	}
}

// turn plays one turn. Turns run one at a time, as in claude.
func (f *streamFake) turn(uuid, text string) {
	f.turnMu.Lock()
	for f.abort != nil {
		f.turnMu.Unlock()
		time.Sleep(10 * time.Millisecond)
		f.turnMu.Lock()
	}
	abort := make(chan string, 1)
	f.abort = abort
	f.turnMu.Unlock()
	defer func() {
		f.turnMu.Lock()
		f.abort = nil
		f.turnMu.Unlock()
	}()

	f.send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "started"})
	f.mu.Lock()
	mode := f.mode
	f.mu.Unlock()
	f.send(map[string]any{"type": "system", "subtype": "init", "capabilities": f.caps, "permissionMode": mode, "claude_code_version": "fake"})

	delta := func(s string) {
		f.send(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": map[string]any{
			"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": s},
		}})
	}
	aborted := func(reason string) {
		f.send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "terminal_reason": reason})
		f.send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "completed"})
	}
	reply := func(s string) {
		delta(s)
		f.send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{
			"content": []map[string]any{{"type": "text", "text": s}},
		}})
		f.send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "terminal_reason": "completed", "result": s})
		f.send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "completed"})
	}
	errorTurn := func(tag, msg string, status int) {
		f.send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "error": tag, "message": map[string]any{
			"model": "<synthetic>", "content": []map[string]any{{"type": "text", "text": msg}},
		}})
		f.send(map[string]any{
			"type": "result", "subtype": "success", "is_error": true, "terminal_reason": "api_error",
			"api_error_status": status, "result": msg,
		})
		f.send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "completed"})
	}
	wait := func(d time.Duration) bool { // false when interrupted
		select {
		case <-abort:
			return false
		case <-time.After(d):
			return true
		}
	}

	word, arg, _ := strings.Cut(text, " ")
	switch word {
	case "PING":
		reply("PONG " + arg)
	case "SLOW":
		n, _ := strconv.Atoi(arg)
		var sb strings.Builder
		for i := 0; i < n; i++ {
			if !wait(100 * time.Millisecond) {
				aborted("aborted_streaming")
				return
			}
			chunk := fmt.Sprintf("chunk%d ", i)
			sb.WriteString(chunk)
			delta(chunk)
		}
		f.send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "terminal_reason": "completed", "result": sb.String()})
		f.send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "completed"})
	case "DELAY":
		if !wait(2 * time.Second) {
			aborted("aborted_streaming")
			return
		}
		reply("late")
	case "TOOL":
		f.send(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": map[string]any{
			"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": "Bash"},
		}})
		if !wait(2 * time.Second) {
			aborted("aborted_tools")
			return
		}
		reply("tool done")
	case "ERR529":
		for i := 1; i <= 2; i++ {
			f.send(map[string]any{
				"type": "system", "subtype": "api_retry", "attempt": i, "max_retries": 2,
				"retry_delay_ms": 50, "error_status": 529, "error": "overloaded",
			})
			time.Sleep(50 * time.Millisecond)
		}
		errorTurn("server_error", "API Error: Repeated 529 Overloaded errors.", 529)
	case "LIMIT":
		f.send(map[string]any{"type": "rate_limit_event", "uuid": "rl-2", "rate_limit_info": map[string]any{
			"status": "rejected", "resetsAt": fakeResetsAt, "rateLimitType": "five_hour",
			"overageStatus": "rejected", "isUsingOverage": false,
		}})
		errorTurn("rate_limit", "You've hit your session limit · resets 6:40pm (UTC)", 429)
	case "RATE":
		status := arg
		if status == "" {
			status = "allowed"
		}
		// The frame a claude.ai subscription account produced on claude
		// 2.1.281 (agentd P11, real account), with its figures.
		f.send(map[string]any{"type": "rate_limit_event", "uuid": "rl-1", "rate_limit_info": map[string]any{
			"status": status, "resetsAt": fakeResetsAt, "rateLimitType": "five_hour",
			"overageStatus": "rejected", "overageDisabledReason": "org_level_disabled", "isUsingOverage": false,
			"unifiedWindows": map[string]any{
				"five_hour": map[string]any{"utilization": 0.1, "resetsAt": fakeResetsAt},
				"seven_day": map[string]any{"utilization": 0.08, "resetsAt": fakeResetsAt + 511200},
			},
		}})
		reply("ok")
	case "RATEBAD":
		f.send(map[string]any{"type": "rate_limit_event", "uuid": "rl-3", "rate_limit_info": map[string]any{"resetsAt": fakeResetsAt}})
		reply("ok")
	case "PERM":
		id := "perm-1"
		ch := make(chan json.RawMessage, 1)
		f.turnMu.Lock()
		if f.control == nil {
			f.control = map[string]chan json.RawMessage{}
		}
		f.control[id] = ch
		f.turnMu.Unlock()
		f.send(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{
			"subtype": "can_use_tool", "tool_name": "Bash", "display_name": "Bash", "description": "touch x",
			"input": map[string]any{"command": "touch x"},
		}})
		var decision struct {
			Behavior string `json:"behavior"`
		}
		select {
		case resp := <-ch:
			_ = json.Unmarshal(resp, &decision)
		case <-abort:
			aborted("aborted_tools")
			return
		}
		reply("tool " + decision.Behavior)
	case "REFUSE":
		f.send(map[string]any{"type": "command_lifecycle", "command_uuid": uuid, "state": "refused"})
	case "EXIT":
		fmt.Fprintln(os.Stderr, "fake claude: exiting on request")
		os.Exit(3)
	default:
		reply("ok")
	}
}

// fakeStreamOptions opens the fake claude over the stream-json transport.
func fakeStreamOptions(extraEnv ...string) Options {
	return Options{
		Harness:          "claude-code",
		BinaryPath:       os.Args[0],
		Transport:        TransportStreamJSON,
		PermissionMode:   "bypass",
		HarnessSessionID: "",
		Env:              append(append(os.Environ(), streamFakeEnv+"=1"), extraEnv...),
	}
}

var _ = context.Background // keep the import for helpers in stream_test.go
