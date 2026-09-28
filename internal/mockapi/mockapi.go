// Package mockapi is a local stand-in for the Anthropic Messages API, for
// driving a real claude binary through scripted scenarios with no account and
// no network. It is a Go port of agentd's P11 mockapi.py, and routes on the
// text of the last user message that carries text (claude appends system
// reminders as separate blocks; tool results continue a scenario):
//
//	PING <n>        reply "PONG <n>"
//	SLOW <n>        reply "slow<i> " in <n> chunks, Server.ChunkDelay apart (default 40)
//	STALL <s>       send message_start, then only pings for <s> seconds (default 60)
//	TOOL <command>  a Bash tool_use running <command>; after its result, "TOOL DONE: <output>"
//	ERR <code> <k>  answer HTTP <code> the first <k> times this text is seen
//	                (529 overloaded_error, 429 rate_limit_error, else api_error),
//	                then "RECOVERED"
//	LIMIT           a usage wall: 429 with the unified limiter's rejected status and
//	                a reset an hour away, every time. Under an OAuth token claude
//	                reports it at once, and does not retry.
//	BIG <kib>       reply with <kib> KiB of text, in 64 KiB deltas
//	anything else   reply "ok"
//
// Every successful reply carries the unified limiter's allowed status, so a
// claude under an OAuth token reports its usage.
package mockapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is a running mock.
type Server struct {
	srv *httptest.Server
	// ChunkDelay is the pause between SLOW's chunks.
	ChunkDelay time.Duration

	mu       sync.Mutex
	seen     map[string]int
	requests []Request
	n        int
}

// Request is one Messages request the mock answered.
type Request struct {
	N        int
	Scenario string
	// System is the request's system prompt, joined.
	System string
	Stream bool
	Model  string
}

// Start starts a mock on a loopback port.
func Start() *Server {
	s := &Server{ChunkDelay: 100 * time.Millisecond, seen: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// URL is the base URL: claude's ANTHROPIC_BASE_URL.
func (s *Server) URL() string { return s.srv.URL }

// Close stops the mock, and every stream it is serving.
func (s *Server) Close() {
	s.srv.CloseClientConnections()
	s.srv.Close()
}

// Requests are the Messages requests answered so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type body struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	System   json.RawMessage `json:"system"`
	Messages []message       `json:"messages"`
}

func textOf(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	_ = json.Unmarshal(raw, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}

var keywords = map[string]bool{"PING": true, "SLOW": true, "STALL": true, "TOOL": true, "ERR": true, "BIG": true, "LIMIT": true}

// route finds the scenario in the last user message: its keyword line, or
// else the result of the tool a scenario ran.
func route(b body) (scenario string, toolResult *string) {
	var last *message
	for i := len(b.Messages) - 1; i >= 0; i-- {
		if b.Messages[i].Role == "user" {
			last = &b.Messages[i]
			break
		}
	}
	if last == nil {
		return "", nil
	}
	var lines []string
	for _, l := range strings.Split(textOf(last.Content), "\n") {
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "<") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if w := strings.Fields(lines[i]); len(w) > 0 && keywords[w[0]] {
			return lines[i], nil
		}
	}
	var blocks []block
	if json.Unmarshal(last.Content, &blocks) == nil {
		for i := len(blocks) - 1; i >= 0; i-- {
			if blocks[i].Type == "tool_result" {
				out := textOf(blocks[i].Content)
				return "", &out
			}
		}
	}
	if len(lines) > 0 {
		return lines[len(lines)-1], nil
	}
	return "", nil
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/messages") || strings.Contains(r.URL.Path, "count_tokens") {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":10}`))
		return
	}
	var b body
	_ = json.Unmarshal(raw, &b)
	scenario, toolResult := route(b)
	system := textOf(b.System)
	s.mu.Lock()
	s.n++
	s.requests = append(s.requests, Request{N: s.n, Scenario: scenario, System: system, Stream: b.Stream, Model: b.Model})
	n := s.n
	s.mu.Unlock()

	words := strings.Fields(scenario)
	arg := func(i, def int) int {
		if len(words) > i {
			if v, err := strconv.Atoi(words[i]); err == nil {
				return v
			}
		}
		return def
	}
	switch {
	case len(words) > 0 && words[0] == "LIMIT":
		reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
		h := w.Header()
		h.Set("anthropic-ratelimit-unified-status", "rejected")
		h.Set("anthropic-ratelimit-unified-reset", reset)
		h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
		h.Set("anthropic-ratelimit-unified-5h-utilization", "1.0")
		h.Set("anthropic-ratelimit-unified-5h-reset", reset)
		s.fail(w, n, 429, "rate_limit_error", "mock usage limit")
		return
	case len(words) > 0 && words[0] == "ERR":
		code, k := arg(1, 529), arg(2, 2)
		s.mu.Lock()
		s.seen[scenario]++
		c := s.seen[scenario]
		s.mu.Unlock()
		if c <= k {
			typ := map[int]string{429: "rate_limit_error", 529: "overloaded_error"}[code]
			if typ == "" {
				typ = "api_error"
			}
			if code == 429 {
				w.Header().Set("retry-after", "1")
			}
			s.fail(w, n, code, typ, fmt.Sprintf("mock %s %d/%d", typ, c, k))
			return
		}
		s.reply(w, b, reply{text: "RECOVERED"})
		return
	case toolResult != nil:
		out := strings.TrimSpace(*toolResult)
		if len(out) > 60 {
			out = out[:60]
		}
		s.reply(w, b, reply{text: "TOOL DONE: " + out})
		return
	}
	switch {
	case len(words) == 0:
		s.reply(w, b, reply{text: "ok"})
	case words[0] == "PING":
		s.reply(w, b, reply{text: strings.TrimSpace("PONG " + strings.Join(words[1:], " "))})
	case words[0] == "SLOW":
		k := arg(1, 40)
		chunks := make([]string, k)
		for i := range chunks {
			chunks[i] = fmt.Sprintf("slow%d ", i)
		}
		s.reply(w, b, reply{chunks: chunks, delay: s.ChunkDelay})
	case words[0] == "STALL":
		s.reply(w, b, reply{text: "after stall", stall: time.Duration(arg(1, 60)) * time.Second})
	case words[0] == "TOOL":
		cmd := strings.TrimSpace(strings.TrimPrefix(scenario, "TOOL"))
		s.reply(w, b, reply{tool: &tool{name: "Bash", input: map[string]string{"command": cmd, "description": "mock"}}})
	case words[0] == "BIG":
		kib := arg(1, 512)
		chunk := strings.Repeat("capacity ", 7282)[:64<<10]
		var chunks []string
		for left := kib; left > 0; left -= 64 {
			chunks = append(chunks, chunk[:min(left, 64)<<10])
		}
		s.reply(w, b, reply{chunks: chunks})
	default:
		s.reply(w, b, reply{text: "ok"})
	}
}

func (s *Server) fail(w http.ResponseWriter, n, code int, typ, msg string) {
	payload, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	w.Header().Set("content-type", "application/json")
	w.Header().Set("content-length", strconv.Itoa(len(payload)))
	w.Header().Set("request-id", "req_mock_"+strconv.Itoa(n))
	w.WriteHeader(code)
	_, _ = w.Write(payload)
}

type tool struct {
	name  string
	input map[string]string
}

type reply struct {
	text   string
	chunks []string
	delay  time.Duration
	stall  time.Duration
	tool   *tool
}

func (s *Server) reply(w http.ResponseWriter, b body, rp reply) {
	model := b.Model
	if model == "" {
		model = "mock"
	}
	id := "msg_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	h := w.Header()
	h.Set("anthropic-ratelimit-unified-status", "allowed")
	h.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(time.Now().Add(5*time.Hour).Unix(), 10))
	h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.1")
	text := rp.text
	if rp.chunks != nil {
		text = strings.Join(rp.chunks, "")
	}
	if !b.Stream {
		content := []map[string]any{{"type": "text", "text": text}}
		stop := "end_turn"
		if rp.tool != nil {
			content = []map[string]any{{"type": "tool_use", "id": "toolu_" + id[4:], "name": rp.tool.name, "input": rp.tool.input}}
			stop = "tool_use"
		}
		payload, _ := json.Marshal(map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model, "content": content,
			"stop_reason": stop, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
		})
		h.Set("content-type", "application/json")
		_, _ = w.Write(payload)
		return
	}
	h.Set("content-type", "text/event-stream")
	h.Set("cache-control", "no-cache")
	fl, _ := w.(http.Flusher)
	send := func(event string, data any) bool {
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
	if !send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
	}}) {
		return
	}
	if rp.stall > 0 {
		end := time.Now().Add(rp.stall)
		for time.Now().Before(end) {
			time.Sleep(time.Second)
			if !send("ping", map[string]string{"type": "ping"}) {
				return
			}
		}
	}
	stop := "end_turn"
	if rp.tool != nil {
		input, _ := json.Marshal(rp.tool.input)
		send("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_" + id[4:], "name": rp.tool.name, "input": map[string]any{}},
		})
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]string{"type": "input_json_delta", "partial_json": string(input)},
		})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		stop = "tool_use"
	} else {
		send("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		chunks := rp.chunks
		if chunks == nil {
			chunks = []string{rp.text}
		}
		for _, c := range chunks {
			if !send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]string{"type": "text_delta", "text": c},
			}) {
				return
			}
			if rp.delay > 0 {
				time.Sleep(rp.delay)
			}
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	}
	send("message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": 5},
	})
	send("message_stop", map[string]string{"type": "message_stop"})
}
