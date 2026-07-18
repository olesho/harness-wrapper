package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
)

// openConversation opens a conversation against the trust-mode mock and
// returns its id, registering cleanup.
func openConversation(t *testing.T, ts *httptest.Server, req openRequest) string {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(ts.URL+"/v1/conversations", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/conversations: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("open status = %d, body = %s", resp.StatusCode, raw)
	}
	var open openResponse
	if err := json.NewDecoder(resp.Body).Decode(&open); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	t.Cleanup(func() {
		dreq, _ := http.NewRequest("DELETE", ts.URL+"/v1/conversations/"+open.ID, nil)
		_, _ = http.DefaultClient.Do(dreq)
	})
	return open.ID
}

// readEventsUntil scans an SSE body, calling fn for each decoded event, until
// fn returns true or the deadline passes.
func readEventsUntil(t *testing.T, body io.Reader, deadline time.Time, fn func(eventDTO) bool) bool {
	t.Helper()
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev eventDTO
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if fn(ev) {
			return true
		}
	}
	return false
}

// matchTrustInputRequest reports whether ev is the trust-prompt input_request,
// validating its parsed options and returning the request id when it is.
func matchTrustInputRequest(t *testing.T, ev eventDTO) (string, bool) {
	t.Helper()
	if ev.Type != "input_request" || ev.Input == nil {
		return "", false
	}
	if ev.Input.Kind != "trust_prompt" {
		t.Errorf("input kind = %q, want trust_prompt", ev.Input.Kind)
	}
	if len(ev.Input.Options) != 2 {
		t.Errorf("options = %+v, want 2", ev.Input.Options)
	}
	for _, o := range ev.Input.Options {
		if len(o.Label) == 0 {
			t.Errorf("option missing label: %+v", o)
		}
	}
	return ev.Input.ID, true
}

// TestSSE_TrustDialogSurfacedAndAnswered exercises the full client interface:
// the trust dialog is surfaced as an input_request SSE frame, the client
// answers via POST /input, and the prompt resolves so a normal turn completes.
func TestSSE_TrustDialogSurfacedAndAnswered(t *testing.T) {
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	id := openConversation(t, ts, openRequest{
		Harness:    "claude-code",
		BinaryPath: mockHarnessBin,
		Args:       []string{"--mode", "trust"},
		// No InputPolicy → the request must be surfaced to the client.
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	streamReq, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/conversations/"+id+"/events", nil)
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = streamResp.Body.Close() }()

	// 1. The trust dialog surfaces as an input_request with parsed options.
	var reqID string
	ok := readEventsUntil(t, streamResp.Body, time.Now().Add(6*time.Second), func(ev eventDTO) bool {
		id, matched := matchTrustInputRequest(t, ev)
		if matched {
			reqID = id
		}
		return matched
	})
	if !ok {
		t.Fatal("never saw an input_request frame for the trust dialog")
	}

	// 2. Acquire control and answer "proceed" via POST /input.
	ctrlResp, err := http.Post(ts.URL+"/v1/conversations/"+id+"/control", "application/json", nil)
	if err != nil {
		t.Fatalf("acquire control: %v", err)
	}
	var ctrl controlResponse
	_ = json.NewDecoder(ctrlResp.Body).Decode(&ctrl)
	_ = ctrlResp.Body.Close()

	answerBody, _ := json.Marshal(answerRequest{Token: ctrl.Token, RequestID: reqID, OptionID: "proceed"})
	ansResp, err := http.Post(ts.URL+"/v1/conversations/"+id+"/input", "application/json", bytes.NewReader(answerBody))
	if err != nil {
		t.Fatalf("POST /input: %v", err)
	}
	if ansResp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(ansResp.Body)
		t.Fatalf("answer status = %d, body = %s", ansResp.StatusCode, raw)
	}
	_ = ansResp.Body.Close()

	// 3. The dialog resolves.
	if !readEventsUntil(t, streamResp.Body, time.Now().Add(6*time.Second), func(ev eventDTO) bool {
		return ev.Type == "input_resolved"
	}) {
		t.Fatal("never saw input_resolved after answering")
	}
}

// TestSSE_TrustDialogAutoAnsweredByPolicy verifies an open-time InputPolicy
// resolves the trust dialog without surfacing it, and a turn then completes.
func TestSSE_TrustDialogAutoAnsweredByPolicy(t *testing.T) {
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	id := openConversation(t, ts, openRequest{
		Harness:    "claude-code",
		BinaryPath: mockHarnessBin,
		Args:       []string{"--mode", "trust"},
		InputPolicy: &chat.InputPolicy{ByKind: map[string]chat.Disposition{
			"trust_prompt": {Kind: chat.DispositionAnswer, OptionID: "proceed"},
		}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	streamReq, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/conversations/"+id+"/events", nil)
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = streamResp.Body.Close() }()

	// Acquire control and send — Send waits through the policy auto-answer
	// (which clears the dialog) and then submits the prompt.
	ctrlResp, err := http.Post(ts.URL+"/v1/conversations/"+id+"/control", "application/json", nil)
	if err != nil {
		t.Fatalf("acquire control: %v", err)
	}
	var ctrl controlResponse
	_ = json.NewDecoder(ctrlResp.Body).Decode(&ctrl)
	_ = ctrlResp.Body.Close()

	sendBody, _ := json.Marshal(sendRequest{Token: ctrl.Token, Text: "hello"})
	sendResp, err := http.Post(ts.URL+"/v1/conversations/"+id+"/messages", "application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sendResp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(sendResp.Body)
		t.Fatalf("send status = %d, body = %s", sendResp.StatusCode, raw)
	}
	_ = sendResp.Body.Close()

	// The policy auto-answered (no input_request frame); the turn completes.
	sawInputRequest := false
	completed := readEventsUntil(t, streamResp.Body, time.Now().Add(6*time.Second), func(ev eventDTO) bool {
		if ev.Type == "input_request" {
			sawInputRequest = true
		}
		return ev.Type == "turn" && ev.Turn != nil && ev.Turn.State == "complete"
	})
	if sawInputRequest {
		t.Error("input_request was surfaced despite an auto-answer policy")
	}
	if !completed {
		t.Fatal("turn never completed after policy auto-answer")
	}
}
