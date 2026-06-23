package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

func TestRunTurnEndpoint_ClaudeStyleOneShot(t *testing.T) {
	const sessionID = "123e4567-e89b-12d3-a456-426614174000"
	bin := fakeHarnessBin(t)
	env := fakeScriptEnv(t, fakeharness.New("claude-code").
		Session(sessionID).
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
		StayAliveUntilStopped().
		Build())

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	exitAfter := true
	body, _ := json.Marshal(runTurnRequest{
		Harness:       "claude",
		BinaryPath:    bin,
		Env:           env,
		Prompt:        "ship the HTTP turn API",
		ExitAfterTurn: &exitAfter,
	})
	resp, err := http.Post(ts.URL+"/v1/turns", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /v1/turns: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}

	var out runTurnResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Turn.State != "complete" {
		t.Fatalf("turn.state = %q, want complete (response: %+v)", out.Turn.State, out)
	}
	if !strings.Contains(out.Turn.Text, "assistant reply: ship the HTTP turn API") {
		t.Fatalf("turn text missing assistant reply: %q", out.Turn.Text)
	}
	if out.Session.HarnessSessionID != "123e4567-e89b-12d3-a456-426614174000" {
		t.Fatalf("harness_session_id = %q", out.Session.HarnessSessionID)
	}
	if !out.ProcessStoppedAfterTurn {
		t.Fatal("process_stopped_after_turn = false, want true")
	}
	if out.WrapperStatus != "interrupted" && out.WrapperStatus != "idle" {
		t.Fatalf("wrapper_status = %q, want interrupted or idle", out.WrapperStatus)
	}
}

// fakeHarnessBin builds the scriptable fake harness once per process (skipping
// when the Go toolchain is unavailable); fakeScriptEnv writes a script to a temp
// file and returns the env pointing the fake at it. Shared by the chatd HTTP
// integration tests so they drive the fake over a real PTY with CSI-13u submit.
func fakeHarnessBin(t *testing.T) string {
	t.Helper()
	p, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}
	return p
}

func fakeScriptEnv(t *testing.T, s fakeharness.Script) []string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	p := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return append(os.Environ(), fakeharness.EnvVar+"="+p)
}
