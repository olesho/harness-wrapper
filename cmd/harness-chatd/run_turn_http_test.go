package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunTurnEndpoint_ClaudeStyleOneShot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness is Unix-only")
	}
	bin := writeHTTPRunTurnExecutable(t, `#!/bin/sh
echo "Fake Claude Code"
echo "❯"
IFS= read -r line
echo "assistant reply: $line"
echo "claude --resume 123e4567-e89b-12d3-a456-426614174000"
echo "✻ Baked for 1s"
`)

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	exitAfter := true
	body, _ := json.Marshal(runTurnRequest{
		Harness:       "claude",
		BinaryPath:    bin,
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

func writeHTTPRunTurnExecutable(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	return path
}
