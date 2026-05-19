package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mockHarnessBin is built once for the SSE integration tests in this
// package. Distinct from the pkg/wrapper TestMain-built copy; this
// package can't reuse that path.
var mockHarnessBin string

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	tmp, err := os.MkdirTemp("", "harness-chatd-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to make temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmp)

	mockHarnessBin = filepath.Join(tmp, "mock")
	cmd := exec.Command("go", "build", "-o", mockHarnessBin,
		"github.com/olesho/harness-wrapper/test/fakeharness/mock")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build mock: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

// TestSSE_APIErrorPropagates is the W2 row of the plan: open a
// conversation against the api-error mock, subscribe to
// /v1/conversations/{id}/events, read one SSE frame, and assert the
// JSON payload carries state=errored, http_code=429, retry_after=30s.
func TestSSE_APIErrorPropagates(t *testing.T) {
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// Open a conversation against the mock in api-error mode.
	openBody, _ := json.Marshal(openRequest{
		Harness:    "claude-code",
		BinaryPath: mockHarnessBin,
		Args: []string{
			"--mode", "api-error",
			"--api-error-msg", "API Error: 429 Too Many Requests. Retry after 30 seconds.",
		},
	})
	resp, err := http.Post(ts.URL+"/v1/conversations", "application/json", bytes.NewReader(openBody))
	if err != nil {
		t.Fatalf("POST /v1/conversations: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("open status = %d, body = %s", resp.StatusCode, raw)
	}
	var open openResponse
	if err := json.NewDecoder(resp.Body).Decode(&open); err != nil {
		t.Fatalf("decode open response: %v", err)
	}
	_ = resp.Body.Close()
	t.Cleanup(func() {
		req, _ := http.NewRequest("DELETE", ts.URL+"/v1/conversations/"+open.ID, nil)
		_, _ = http.DefaultClient.Do(req)
	})

	// Subscribe to SSE.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	streamReq, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/conversations/"+open.ID+"/events", nil)
	streamReq.Header.Set("Accept", "text/event-stream")
	streamResp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer streamResp.Body.Close()

	// pkg/chat only emits a TurnEvent when there is a current
	// assistant turn — the wrapper's mid-run StatusAPIError flows into
	// the watcher and is dropped because no Send was issued. We need
	// to acquire control and send a message so the conversation has a
	// pending turn for the api_error to attach to.
	ctrlResp, err := http.Post(ts.URL+"/v1/conversations/"+open.ID+"/control", "application/json", nil)
	if err != nil {
		t.Fatalf("acquire control: %v", err)
	}
	var ctrl controlResponse
	if err := json.NewDecoder(ctrlResp.Body).Decode(&ctrl); err != nil {
		t.Fatalf("decode control: %v", err)
	}
	_ = ctrlResp.Body.Close()

	sendBody, _ := json.Marshal(sendRequest{Token: ctrl.Token, Text: "trigger"})
	sendResp, err := http.Post(ts.URL+"/v1/conversations/"+open.ID+"/messages",
		"application/json", bytes.NewReader(sendBody))
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sendResp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(sendResp.Body)
		t.Fatalf("send status = %d, body = %s", sendResp.StatusCode, raw)
	}
	_ = sendResp.Body.Close()

	// Read SSE frames until we see one with http_code populated.
	scanner := bufio.NewScanner(streamResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	deadline := time.Now().Add(6 * time.Second)
	var matched turnEventDTO
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev turnEventDTO
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Logf("non-JSON SSE data line %q: %v", payload, err)
			continue
		}
		if ev.Turn.HTTPCode != 0 || ev.Turn.RetryAfter != "" {
			matched = ev
			break
		}
	}
	if scanner.Err() != nil {
		t.Logf("scanner err (likely deadline): %v", scanner.Err())
	}

	if matched.Turn.HTTPCode != 429 {
		t.Errorf("turn.http_code = %d, want 429 (full event: %+v)", matched.Turn.HTTPCode, matched)
	}
	if matched.Turn.RetryAfter != "30s" {
		t.Errorf("turn.retry_after = %q, want %q", matched.Turn.RetryAfter, "30s")
	}
	if matched.Turn.State != "errored" {
		t.Errorf("turn.state = %q, want errored", matched.Turn.State)
	}
	if !strings.Contains(matched.Turn.Reason, "api error 429") {
		t.Errorf("turn.reason = %q, want substring %q", matched.Turn.Reason, "api error 429")
	}
}
