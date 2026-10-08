package mockapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// respond posts one Responses request whose input ends with items, and
// returns the status, the headers and the streamed events by type.
func respond(t *testing.T, s *Server, tools []string, items ...map[string]any) (int, http.Header, []map[string]any) {
	t.Helper()
	body := map[string]any{"model": "mock", "stream": true, "instructions": "SYSTEM", "input": items}
	var ts []map[string]string
	for _, name := range tools {
		ts = append(ts, map[string]string{"type": "function", "name": name})
	}
	body["tools"] = ts
	b, _ := json.Marshal(body)
	resp, err := http.Post(s.URL()+"/v1/responses", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var events []map[string]any
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("event %q: %v", data, err)
		}
		events = append(events, ev)
	}
	return resp.StatusCode, resp.Header, events
}

func user(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": text}}}
}

// replyOf is the text a streamed reply carries, and how it ended.
func replyOf(events []map[string]any) (text, end string) {
	for _, ev := range events {
		switch ev["type"] {
		case "response.output_text.delta":
			text += ev["delta"].(string)
		case "response.completed", "response.failed":
			end = ev["type"].(string)
		}
	}
	return text, end
}

func TestResponsesScenarios(t *testing.T) {
	s := Start()
	defer s.Close()
	s.ChunkDelay = 0

	code, h, events := respond(t, s, nil, user("<environment_context>ignored</environment_context>"), user("PING 3"))
	if text, end := replyOf(events); code != 200 || text != "PONG 3" || end != "response.completed" {
		t.Errorf("PING: %d %q %q", code, text, end)
	}
	if h.Get("x-codex-primary-used-percent") != "10.0" || h.Get("x-codex-primary-reset-at") == "" {
		t.Errorf("a reply carries no rate-limit headers: %v", h)
	}
	if r := s.Requests(); len(r) != 1 || r[0].Scenario != "PING 3" || r[0].System != "SYSTEM" {
		t.Errorf("requests %+v", r)
	}

	_, _, events = respond(t, s, nil, user("SLOW 3"))
	if text, _ := replyOf(events); text != "slow0 slow1 slow2 " {
		t.Errorf("SLOW: %q", text)
	}

	// ERR 529: server errors codex retries, the attempt after the retry budget
	// overloaded, then RECOVERED.
	var ends []string
	for range 4 {
		_, _, events = respond(t, s, nil, user("ERR 529 3"))
		for _, ev := range events {
			if ev["type"] == "response.failed" {
				ends = append(ends, ev["response"].(map[string]any)["error"].(map[string]any)["code"].(string))
			}
		}
		if text, end := replyOf(events); end == "response.completed" {
			ends = append(ends, text)
		}
	}
	if want := []string{"server_error", "server_error", "server_is_overloaded", "RECOVERED"}; strings.Join(ends, ",") != strings.Join(want, ",") {
		t.Errorf("ERR 529 3: %v, want %v", ends, want)
	}

	// FAILSLOW: a server error after the stall, every time.
	for range 2 {
		start := time.Now()
		_, _, events = respond(t, s, nil, user("FAILSLOW 1"))
		if _, end := replyOf(events); end != "response.failed" || time.Since(start) < time.Second {
			t.Errorf("FAILSLOW 1: %q after %v", end, time.Since(start))
		}
	}

	code, h, _ = respond(t, s, nil, user("LIMIT"))
	if code != http.StatusTooManyRequests || h.Get("x-codex-primary-used-percent") != "100.0" {
		t.Errorf("LIMIT: %d %v", code, h)
	}

	_, _, events = respond(t, s, []string{"exec_command"}, user("TOOL echo hi"))
	var fc map[string]any
	for _, ev := range events {
		if ev["type"] == "response.output_item.done" {
			fc = ev["item"].(map[string]any)
		}
	}
	if fc == nil || fc["type"] != "function_call" || fc["name"] != "exec_command" || !strings.Contains(fc["arguments"].(string), "echo hi") {
		t.Fatalf("TOOL: %v", fc)
	}
	_, _, events = respond(t, s, nil, user("TOOL echo hi"), map[string]any{
		"type": "function_call_output", "call_id": fc["call_id"], "output": "Chunk ID: 1\nProcess exited with code 0\nOutput:\nhi\n",
	})
	if text, _ := replyOf(events); text != "TOOL DONE: hi" {
		t.Errorf("after the tool: %q", text)
	}
	// pi offers its own bash, which takes the command as it is.
	_, _, events = respond(t, s, []string{"read", "bash"}, user("TOOL echo hi"))
	fc = nil
	for _, ev := range events {
		if item, ok := ev["item"].(map[string]any); ok && ev["type"] == "response.output_item.done" && item["type"] == "function_call" {
			fc = item
		}
	}
	if fc == nil || fc["name"] != "bash" || fc["arguments"] != `{"command":"echo hi"}` {
		t.Fatalf("TOOL for pi: %v", fc)
	}
}

// A goal's scenarios: MKGOAL has the model make a goal, and the goal's
// objective, which codex repeats in every turn it starts for it, steps a
// number of times and then has the model complete it.
func TestResponsesGoalScenarios(t *testing.T) {
	s := Start()
	defer s.Close()
	s.ChunkDelay = 0
	call := func(events []map[string]any) map[string]any {
		for _, ev := range events {
			if ev["type"] == "response.output_item.done" {
				if item := ev["item"].(map[string]any); item["type"] == "function_call" {
					return item
				}
			}
		}
		return nil
	}

	_, _, events := respond(t, s, []string{"create_goal"}, user("MKGOAL GOAL 2 3"))
	if fc := call(events); fc == nil || fc["name"] != "create_goal" || fc["arguments"] != `{"objective":"GOAL 2 3"}` {
		t.Fatalf("MKGOAL: %v", fc)
	}
	// codex's own message for a goal turn carries the objective on a line of
	// its own, between tags.
	goal := user("<codex_internal_context source=\"goal\">\nContinue working toward the active thread goal.\n\n<objective>\nGOAL 2 3\n</objective>\n</codex_internal_context>")
	for i, want := range []string{"goal step 1.0 goal step 1.1 goal step 1.2 ", "goal step 2.0 goal step 2.1 goal step 2.2 "} {
		_, _, events = respond(t, s, nil, goal)
		if text, end := replyOf(events); text != want || end != "response.completed" {
			t.Errorf("goal turn %d: %q %q, want %q", i+1, text, end, want)
		}
	}
	_, _, events = respond(t, s, []string{"update_goal"}, goal)
	if fc := call(events); fc == nil || fc["name"] != "update_goal" || fc["arguments"] != `{"status":"complete"}` {
		t.Errorf("the turn after the goal's steps: %v, want an update_goal call", fc)
	}
	_, _, events = respond(t, s, nil, goal)
	if text, _ := replyOf(events); text != "ok" {
		t.Errorf("a goal turn after the goal is complete: %q", text)
	}
	// An input sent while the goal is active is answered for itself.
	_, _, events = respond(t, s, nil, goal, user("PING 4"))
	if text, _ := replyOf(events); text != "PONG 4" {
		t.Errorf("an input after a goal turn: %q", text)
	}
}
