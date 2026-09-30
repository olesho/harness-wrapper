package mockapi

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func post(t *testing.T, s *Server, text string, stream bool) *http.Response {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"model": "m", "stream": stream, "system": "PERSONA",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]string{"type": "text", "text": "<system-reminder>ignore</system-reminder>"},
			map[string]string{"type": "text", "text": text},
		}}},
	})
	resp, err := http.Post(s.URL()+"/v1/messages?beta=true", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// deltas reads a streamed reply's text.
func deltas(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var out strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &ev) == nil && ev.Delta.Type == "text_delta" {
			out.WriteString(ev.Delta.Text)
		}
	}
	return out.String()
}

func TestScenarios(t *testing.T) {
	s := Start()
	defer s.Close()
	s.ChunkDelay = 0
	if got := deltas(t, post(t, s, "PING 7", true)); got != "PONG 7" {
		t.Errorf("PING: %q", got)
	}
	if got := deltas(t, post(t, s, "SLOW 3", true)); got != "slow0 slow1 slow2 " {
		t.Errorf("SLOW: %q", got)
	}
	if got := deltas(t, post(t, s, "BIG 100", true)); len(got) != 100<<10 {
		t.Errorf("BIG 100: %d bytes", len(got))
	}
	for i := 1; i <= 2; i++ {
		resp := post(t, s, "ERR 529 2", true)
		_ = resp.Body.Close()
		if resp.StatusCode != 529 {
			t.Errorf("ERR attempt %d: %d, want 529", i, resp.StatusCode)
		}
	}
	if got := deltas(t, post(t, s, "ERR 529 2", true)); got != "RECOVERED" {
		t.Errorf("ERR after its failures: %q", got)
	}
	resp := post(t, s, "LIMIT", true)
	_ = resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("anthropic-ratelimit-unified-status") != "rejected" {
		t.Errorf("LIMIT: %d %q", resp.StatusCode, resp.Header.Get("anthropic-ratelimit-unified-status"))
	}
	// AGENT hands its prompt to a subagent, in the foreground.
	resp = post(t, s, "AGENT PING 9", false)
	var msg struct {
		Content []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(msg.Content) != 1 || msg.Content[0].Name != "Agent" || msg.Content[0].Input["prompt"] != "PING 9" || msg.Content[0].Input["run_in_background"] != false {
		t.Errorf("AGENT: %+v", msg.Content)
	}
	reqs := s.Requests()
	if len(reqs) == 0 || reqs[0].Scenario != "PING 7" || reqs[0].System != "PERSONA" {
		t.Errorf("requests: %+v", reqs)
	}
}

// A request is kept with the credential it came with, and with its body only
// when the server is asked to keep bodies: on both APIs.
func TestRequestsKeepTheCredentialAndTheBody(t *testing.T) {
	send := func(s *Server, path, header, value, body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, s.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(header, value)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	const (
		messages  = `{"model":"m","messages":[{"role":"user","content":"PING 1"}]}`
		responses = `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":"PING 2"}]}`
	)

	plain := Start()
	defer plain.Close()
	send(plain, "/v1/messages", "x-api-key", "key-a", messages)
	if r := plain.Requests(); len(r) != 1 || r[0].Auth != "key-a" || r[0].Body != nil {
		t.Errorf("without KeepBodies: %+v, want the credential and no body", r)
	}

	keep := Start()
	defer keep.Close()
	keep.KeepBodies = true
	send(keep, "/v1/messages", "Authorization", "Bearer tok-b", messages)
	send(keep, "/v1/responses", "Authorization", "Bearer tok-c", responses)
	r := keep.Requests()
	if len(r) != 2 {
		t.Fatalf("requests: %+v", r)
	}
	if r[0].Auth != "Bearer tok-b" || string(r[0].Body) != messages {
		t.Errorf("messages request: auth %q body %q", r[0].Auth, r[0].Body)
	}
	if r[1].Auth != "Bearer tok-c" || string(r[1].Body) != responses || r[1].Scenario != "PING 2" {
		t.Errorf("responses request: auth %q body %q scenario %q", r[1].Auth, r[1].Body, r[1].Scenario)
	}
}
