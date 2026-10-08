package mockapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The OpenAI Responses API (POST /v1/responses, streamed), which codex and pi
// speak to a model provider. The scenarios are the Messages API's, as a codex
// reports them:
//
//	PING <n>        reply "PONG <n>"
//	SLOW <n>        reply "slow<i> " in <n> deltas, Server.ChunkDelay apart
//	STALL <s>       open the response, then send nothing for <s> seconds
//	TOOL <command>  a call running <command> in a shell, after its output "TOOL
//	                DONE: <output>": codex's exec_command, pi's bash, or else a
//	                shell call, whichever the request offers
//	ERR <code> <k>  fail the response the first <k> times this text is seen,
//	                with a server error codex retries; for 529, the attempt
//	                after Server.RetryBudget such failures in a row answers
//	                overloaded, which codex does not retry. Then "RECOVERED".
//	FAILSLOW <s>    open the response, wait <s> seconds, then fail it with a
//	                server error codex retries; every time
//	LIMIT           a usage wall: 429 usage_limit_reached, the primary window
//	                at 100% and resetting an hour away, every time
//	BIG <kib>       reply with <kib> KiB of text, in 64 KiB deltas
//	MKGOAL <text>   a create_goal call with <text> as the goal's objective;
//	                after its output, "TOOL DONE: <output>"
//	GOAL <n> <k>    work on a goal: the first <n> times this text is seen,
//	                reply "goal step <i>" in <k> deltas (default 1),
//	                Server.ChunkDelay apart; the next time, an update_goal
//	                call that marks the goal complete. It is a goal's
//	                objective: codex repeats it in every turn it starts for
//	                the goal.
//	anything else   reply "ok"
//
// Every reply carries codex's rate-limit headers, so codex reports its usage.

type responsesBody struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions"`
	Stream       bool              `json:"stream"`
	Input        []json.RawMessage `json:"input"`
	Tools        []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tools"`
}

type responsesItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Output  json.RawMessage `json:"output"`
}

// responsesText is an item's text: its content parts' text, joined.
func responsesText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var out []string
	for _, p := range parts {
		if p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

// routeResponses finds the scenario: the output of the tool call the input
// ends with, or else the keyword line of the last user message. codex adds
// context of its own as user messages in tags, which never route. A message
// may leave out its type, as pi's do (the API's easy input message).
func routeResponses(b responsesBody) (scenario string, toolResult *string) {
	if n := len(b.Input); n > 0 {
		var last responsesItem
		if json.Unmarshal(b.Input[n-1], &last) == nil && strings.HasSuffix(last.Type, "call_output") {
			out := responsesText(last.Output)
			// exec_command's output leads with facts about the run.
			if _, after, ok := strings.Cut(out, "Output:\n"); ok {
				out = after
			}
			return "", &out
		}
	}
	for i := len(b.Input) - 1; i >= 0; i-- {
		var it responsesItem
		if json.Unmarshal(b.Input[i], &it) != nil || (it.Type != "message" && it.Type != "") || it.Role != "user" {
			continue
		}
		var lines []string
		for _, l := range strings.Split(responsesText(it.Content), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "<") {
				lines = append(lines, l)
			}
		}
		for j := len(lines) - 1; j >= 0; j-- {
			if w := strings.Fields(lines[j]); len(w) > 0 && keywords[w[0]] {
				return lines[j], nil
			}
		}
		if len(lines) > 0 {
			return lines[len(lines)-1], nil
		}
	}
	return "", nil
}

func (s *Server) serveResponses(w http.ResponseWriter, raw []byte, auth string) {
	var b responsesBody
	_ = json.Unmarshal(raw, &b)
	scenario, toolResult := routeResponses(b)
	var input []string
	for _, raw := range b.Input {
		var it responsesItem
		if json.Unmarshal(raw, &it) == nil {
			input = append(input, responsesText(it.Content))
		}
	}
	s.record(Request{Scenario: scenario, System: b.Instructions, Input: strings.Join(input, "\n"), Stream: b.Stream, Model: b.Model, Auth: auth}, raw)

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
		payload, _ := json.Marshal(map[string]any{"error": map[string]any{
			"type": "usage_limit_reached", "message": "mock usage limit",
			"resets_at": time.Now().Add(time.Hour).Unix(), "resets_in_seconds": 3600,
		}})
		limitHeaders(w.Header(), 100)
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(payload)
		return
	case len(words) > 0 && words[0] == "FAILSLOW":
		s.stream(w, response{failed: "server_error", text: "mock failure after a stall", stall: time.Duration(arg(1, 2)) * time.Second})
		return
	case len(words) > 0 && words[0] == "ERR":
		code, k := arg(1, 529), arg(2, 2)
		s.mu.Lock()
		s.seen[scenario]++
		c := s.seen[scenario]
		budget := s.RetryBudget
		s.mu.Unlock()
		if c <= k {
			kind := "server_error"
			if code == 529 && budget >= 0 && c%(budget+1) == 0 {
				kind = "server_is_overloaded"
			}
			s.stream(w, response{failed: kind, text: fmt.Sprintf("mock %s %d/%d", kind, c, k)})
			return
		}
		s.stream(w, response{text: "RECOVERED"})
		return
	case toolResult != nil:
		out := strings.TrimSpace(*toolResult)
		if len(out) > 60 {
			out = out[:60]
		}
		s.stream(w, response{text: "TOOL DONE: " + out})
		return
	}
	switch {
	case len(words) == 0:
		s.stream(w, response{text: "ok"})
	case words[0] == "PING":
		s.stream(w, response{text: strings.TrimSpace("PONG " + strings.Join(words[1:], " "))})
	case words[0] == "SLOW":
		k := arg(1, 40)
		chunks := make([]string, k)
		for i := range chunks {
			chunks[i] = fmt.Sprintf("slow%d ", i)
		}
		s.stream(w, response{chunks: chunks, delay: s.ChunkDelay})
	case words[0] == "STALL":
		s.stream(w, response{text: "after stall", stall: time.Duration(arg(1, 60)) * time.Second})
	case words[0] == "TOOL":
		cmd := strings.TrimSpace(strings.TrimPrefix(scenario, "TOOL"))
		fc := &call{name: "shell", args: map[string]any{"command": []string{"bash", "-lc", cmd}}}
		for _, t := range b.Tools {
			switch t.Name {
			case "exec_command":
				fc = &call{name: "exec_command", args: map[string]any{"cmd": cmd, "yield_time_ms": 5000}}
			case "bash":
				fc = &call{name: "bash", args: map[string]any{"command": cmd}}
			}
		}
		s.stream(w, response{call: fc})
	case words[0] == "BIG":
		kib := arg(1, 512)
		chunk := strings.Repeat("capacity ", 7282)[:64<<10]
		var chunks []string
		for left := kib; left > 0; left -= 64 {
			chunks = append(chunks, chunk[:min(left, 64)<<10])
		}
		s.stream(w, response{chunks: chunks})
	case words[0] == "MKGOAL":
		objective := strings.TrimSpace(strings.TrimPrefix(scenario, "MKGOAL"))
		s.stream(w, response{call: &call{name: "create_goal", args: map[string]any{"objective": objective}}})
	case words[0] == "GOAL":
		n, k := arg(1, 1), arg(2, 1)
		s.mu.Lock()
		s.seen[scenario]++
		c := s.seen[scenario]
		s.mu.Unlock()
		switch {
		case c <= n:
			chunks := make([]string, k)
			for i := range chunks {
				chunks[i] = fmt.Sprintf("goal step %d.%d ", c, i)
			}
			s.stream(w, response{chunks: chunks, delay: s.ChunkDelay})
		case c == n+1:
			s.stream(w, response{call: &call{name: "update_goal", args: map[string]any{"status": "complete"}}})
		default:
			s.stream(w, response{text: "ok"})
		}
	default:
		s.stream(w, response{text: "ok"})
	}
}

// limitHeaders are codex's rate-limit headers: the primary window used to
// used percent, the secondary lightly, each resetting in its time.
func limitHeaders(h http.Header, used float64) {
	now := time.Now()
	h.Set("x-codex-primary-used-percent", strconv.FormatFloat(used, 'f', 1, 64))
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-at", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	h.Set("x-codex-secondary-used-percent", "3.0")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-at", strconv.FormatInt(now.Add(72*time.Hour).Unix(), 10))
}

type call struct {
	name string
	args map[string]any
}

type response struct {
	text   string
	chunks []string
	delay  time.Duration
	stall  time.Duration
	call   *call
	// failed ends the response with response.failed carrying this code.
	failed string
}

func (s *Server) stream(w http.ResponseWriter, rp response) {
	h := w.Header()
	limitHeaders(h, 10)
	h.Set("content-type", "text/event-stream")
	h.Set("cache-control", "no-cache")
	fl, _ := w.(http.Flusher)
	send := func(event string, data map[string]any) bool {
		data["type"] = event
		b, _ := json.Marshal(data)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		if fl != nil {
			fl.Flush()
		}
		return true
	}
	id := strconv.FormatInt(time.Now().UnixNano(), 36)
	rid, mid := "resp_"+id, "msg_"+id
	if !send("response.created", map[string]any{"response": map[string]any{"id": rid}}) {
		return
	}
	if rp.stall > 0 {
		time.Sleep(rp.stall)
	}
	if rp.failed != "" {
		send("response.failed", map[string]any{"response": map[string]any{
			"id": rid, "status": "failed", "error": map[string]string{"code": rp.failed, "message": rp.text},
		}})
		return
	}
	if c := rp.call; c != nil {
		args, _ := json.Marshal(c.args)
		item := map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": "call_" + id, "name": c.name, "arguments": string(args)}
		send("response.output_item.added", map[string]any{"output_index": 0, "item": item})
		send("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	} else {
		send("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{
			"type": "message", "role": "assistant", "id": mid, "content": []any{},
		}})
		chunks := rp.chunks
		if chunks == nil {
			chunks = []string{rp.text}
		}
		for _, c := range chunks {
			if !send("response.output_text.delta", map[string]any{"item_id": mid, "output_index": 0, "content_index": 0, "delta": c}) {
				return
			}
			if rp.delay > 0 {
				time.Sleep(rp.delay)
			}
		}
		send("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{
			"type": "message", "role": "assistant", "id": mid,
			"content": []any{map[string]any{"type": "output_text", "text": strings.Join(chunks, ""), "annotations": []any{}}},
		}})
	}
	send("response.completed", map[string]any{"response": map[string]any{"id": rid, "usage": map[string]any{
		"input_tokens": 10, "input_tokens_details": map[string]int{"cached_tokens": 0},
		"output_tokens": 5, "output_tokens_details": map[string]int{"reasoning_tokens": 0}, "total_tokens": 15,
	}}})
}
