package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestScreen_SnapshotOfLiveConversation opens a conversation against the
// always-alive "stuck" mock and asserts GET /v1/conversations/{id}/screen
// returns the rendered terminal. This is the diagnosis path: inspecting a
// hung harness's screen with a pure read (no control token).
func TestScreen_SnapshotOfLiveConversation(t *testing.T) {
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// "stuck" prints "Thinking..." then blocks forever, so the conversation
	// stays alive while we read its screen.
	id := openConversation(t, ts, openRequest{
		Harness:    "claude-code",
		BinaryPath: mockHarnessBin,
		Args:       []string{"--mode", "stuck"},
	})

	// The harness starts and renders asynchronously; poll until its startup
	// banner shows up on the emulated screen (or give up).
	var snap screenResponse
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		snap = getScreen(t, ts, id, http.StatusOK)
		if strings.Contains(snap.Text, "Mock Agent CLI") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !strings.Contains(snap.Text, "Mock Agent CLI") {
		t.Fatalf("screen never rendered the mock banner; got text:\n%s", snap.Text)
	}
	if snap.Cols <= 0 || snap.Rows <= 0 {
		t.Errorf("screen dimensions = %dx%d, want both > 0", snap.Cols, snap.Rows)
	}
	if snap.Generation == 0 {
		t.Errorf("generation = 0, want > 0 after the screen was written")
	}
}

// TestScreen_UnknownConversation404 verifies the endpoint 404s for an unknown
// id — the same shape a caller sees after the conversation has closed (the
// screen lives only while the harness is alive).
func TestScreen_UnknownConversation404(t *testing.T) {
	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	getScreen(t, ts, "does-not-exist", http.StatusNotFound)
}

func getScreen(t *testing.T, ts *httptest.Server, id string, wantStatus int) screenResponse {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/conversations/" + id + "/screen")
	if err != nil {
		t.Fatalf("GET /screen: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("screen status = %d, want %d, body = %s", resp.StatusCode, wantStatus, raw)
	}
	var out screenResponse
	if wantStatus == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode screen: %v", err)
		}
	}
	return out
}
