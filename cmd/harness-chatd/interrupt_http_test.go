package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// openScripted opens a conversation on the fake harness and returns its id.
func openScripted(t *testing.T, ts *httptest.Server, harness string, script fakeharness.Script) string {
	t.Helper()
	envJSON, _ := json.Marshal(fakeScriptEnv(t, script))
	status, raw := postRaw(t, ts.URL+"/v1/conversations",
		`{"harness":"`+harness+`","binary_path":"`+fakeHarnessBin(t)+`","env":`+string(envJSON)+`}`)
	if status != http.StatusCreated {
		t.Fatalf("open: status %d: %s", status, raw)
	}
	var open openResponse
	_ = json.Unmarshal(raw, &open)
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/conversations/"+open.ID, nil)
		_, _ = http.DefaultClient.Do(req)
	})
	return open.ID
}

// TestInterrupt_Route: POST .../interrupt takes no control token — the holder
// is mid-turn — answers what the interrupt did, and the interrupted turn
// arrives on the event stream with state "interrupted".
func TestInterrupt_Route(t *testing.T) {
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	id := openScripted(t, ts, "claude-code", fakeharness.New("claude-code").ComposerBox().Idle().
		AwaitSubmit().EchoWorking(20, "Writing").AwaitInterrupt().Stopped(30, "half a reply").
		StayAliveUntilStopped().Build())

	// No turn yet: nothing to interrupt, nothing written.
	status, raw := postRaw(t, ts.URL+"/v1/conversations/"+id+"/interrupt", "")
	if status != http.StatusOK || !strings.Contains(string(raw), `"result":"no_turn"`) {
		t.Fatalf("interrupt with no turn: status %d: %s", status, raw)
	}

	events, err := http.Get(ts.URL + "/v1/conversations/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = events.Body.Close() }()
	status, raw = postRaw(t, ts.URL+"/v1/conversations/"+id+"/control", "")
	if status != http.StatusOK {
		t.Fatalf("control: status %d: %s", status, raw)
	}
	var ctrl controlResponse
	_ = json.Unmarshal(raw, &ctrl)
	body, _ := json.Marshal(sendRequest{Token: ctrl.Token, Text: "tell me a story"})
	if status, raw = postRaw(t, ts.URL+"/v1/conversations/"+id+"/messages", string(body)); status != http.StatusAccepted {
		t.Fatalf("send: status %d: %s", status, raw)
	}
	srv.mu.RLock()
	entry := srv.convs[id]
	srv.mu.RUnlock()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(entry.conv.ScreenSnapshot().Text, "esc to interrupt") {
		if time.Now().After(deadline) {
			t.Fatal("the turn never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, raw = postRaw(t, ts.URL+"/v1/conversations/"+id+"/interrupt", "")
	var got interruptResponse
	_ = json.Unmarshal(raw, &got)
	if status != http.StatusOK || got.Result != "stopped" || got.Error != "" {
		t.Fatalf("interrupt: status %d: %s", status, raw)
	}

	sc := bufio.NewScanner(events.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev eventDTO
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Turn == nil || ev.Turn.Role != "assistant" {
			continue
		}
		if ev.Turn.State == "interrupted" {
			if ev.Turn.Text != "half a reply" {
				t.Fatalf("interrupted turn = %+v, want its partial reply", ev.Turn)
			}
			return
		}
	}
	t.Fatal("the event stream ended without the interrupted turn")
}

// An interrupt on a harness that has none is 501 interrupt_unsupported.
func TestInterrupt_RouteUnsupported(t *testing.T) {
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	id := openScripted(t, ts, "codex", fakeharness.New("codex").Idle().StayAliveUntilStopped().Build())
	status, raw := postRaw(t, ts.URL+"/v1/conversations/"+id+"/interrupt", "")
	var got errorResponse
	_ = json.Unmarshal(raw, &got)
	if status != http.StatusNotImplemented || got.Code != "interrupt_unsupported" {
		t.Fatalf("interrupt on codex: status %d: %s", status, raw)
	}
}
