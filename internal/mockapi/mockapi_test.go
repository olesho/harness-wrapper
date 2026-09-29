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
	reqs := s.Requests()
	if len(reqs) == 0 || reqs[0].Scenario != "PING 7" || reqs[0].System != "PERSONA" {
		t.Errorf("requests: %+v", reqs)
	}
}
