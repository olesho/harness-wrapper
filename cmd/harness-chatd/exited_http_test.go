package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// TestEvents_ExitedIsTheLastFrame: a harness that dies mid-turn ends the turn
// on the stream, then an "exited" frame says how the process ended, and the
// stream ends (ADR-008).
func TestEvents_ExitedIsTheLastFrame(t *testing.T) {
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	id := openScripted(t, ts, "claude-code", fakeharness.New("claude-code").Idle().
		AwaitSubmit().Working(20, "Working").Exit(3).Build())

	events, err := http.Get(ts.URL + "/v1/conversations/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = events.Body.Close() }()
	status, raw := postRaw(t, ts.URL+"/v1/conversations/"+id+"/control", "")
	if status != http.StatusOK {
		t.Fatalf("control: status %d: %s", status, raw)
	}
	var ctrl controlResponse
	_ = json.Unmarshal(raw, &ctrl)
	body, _ := json.Marshal(sendRequest{Token: ctrl.Token, Text: "go"})
	if status, raw = postRaw(t, ts.URL+"/v1/conversations/"+id+"/messages", string(body)); status != http.StatusAccepted {
		t.Fatalf("send: status %d: %s", status, raw)
	}

	var frames []eventDTO
	sc := bufio.NewScanner(events.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev eventDTO
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("frame %q: %v", line, err)
		}
		frames = append(frames, ev)
	}
	// The stream ended: the last frame is the exit, after the errored turn.
	if n := len(frames); n < 2 || frames[n-1].Type != "exited" || frames[n-1].Exit == nil {
		t.Fatalf("frames = %+v, want the exited frame last", frames)
	}
	if exit := frames[len(frames)-1].Exit; exit.Status != "failed" || exit.ExitCode != 3 {
		t.Fatalf("exit = %+v, want failed with code 3", exit)
	}
	if prev := frames[len(frames)-2]; prev.Turn == nil || prev.Turn.State != "errored" {
		t.Fatalf("frame before the exit = %+v, want the errored turn", prev)
	}
}
