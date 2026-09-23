package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// TestSendMessage_HarnessBusyIs409: a message whose request context ends while
// the harness is still working gets 409 harness_busy, and nothing is typed —
// the harness keeps working undisturbed. The context here is the request's own
// deadline; in a deployment it is whatever bounds the handler.
func TestSendMessage_HarnessBusyIs409(t *testing.T) {
	b := fakeharness.New("claude-code").ComposerBox().Idle()
	for range 40 {
		b = b.Working(100, "Working")
	}
	script := b.StayAliveUntilStopped().Build()
	envJSON, _ := json.Marshal(fakeScriptEnv(t, script))
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	status, raw := postRaw(t, ts.URL+"/v1/conversations",
		`{"harness":"claude-code","binary_path":"`+fakeHarnessBin(t)+`","env":`+string(envJSON)+`}`)
	if status != http.StatusCreated {
		t.Fatalf("open: status %d: %s", status, raw)
	}
	var open openResponse
	_ = json.Unmarshal(raw, &open)
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/conversations/"+open.ID, nil)
		_, _ = http.DefaultClient.Do(req)
	})
	status, raw = postRaw(t, ts.URL+"/v1/conversations/"+open.ID+"/control", "")
	if status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("control: status %d: %s", status, raw)
	}
	var ctrl controlResponse
	_ = json.Unmarshal(raw, &ctrl)

	srv.mu.RLock()
	entry := srv.convs[open.ID]
	srv.mu.RUnlock()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(entry.conv.ScreenSnapshot().Text, "Working…") {
		if time.Now().After(deadline) {
			t.Fatal("the harness never started working")
		}
		time.Sleep(10 * time.Millisecond)
	}

	body, _ := json.Marshal(sendRequest{Token: ctrl.Token, Text: "go"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/conversations/"+open.ID+"/messages", strings.NewReader(string(body)))
	req.SetPathValue("id", open.ID)
	rec := httptest.NewRecorder()
	srv.sendMessage(rec, req)
	var got errorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusConflict || got.Code != "harness_busy" {
		t.Fatalf("send while busy: status %d, body %s; want 409 harness_busy", rec.Code, rec.Body.String())
	}
}
