package mockapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
}
