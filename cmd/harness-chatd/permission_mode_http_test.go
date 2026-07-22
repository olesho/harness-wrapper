package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPermissionMode_Decodes pins the wire name on both request structs: a
// client sends "permission_mode", not "permissionMode" or "permission-mode".
func TestPermissionMode_Decodes(t *testing.T) {
	var run runTurnRequest
	if err := json.Unmarshal([]byte(`{"permission_mode":"plan"}`), &run); err != nil {
		t.Fatalf("decode runTurnRequest: %v", err)
	}
	if run.PermissionMode != "plan" {
		t.Fatalf("runTurnRequest.PermissionMode = %q, want plan", run.PermissionMode)
	}

	var open openRequest
	if err := json.Unmarshal([]byte(`{"permission_mode":"bypass"}`), &open); err != nil {
		t.Fatalf("decode openRequest: %v", err)
	}
	if open.PermissionMode != "bypass" {
		t.Fatalf("openRequest.PermissionMode = %q, want bypass", open.PermissionMode)
	}
}

// TestPermissionMode_InvalidRejectedWith400 is the end-to-end half of the
// error mapping: an unusable permission mode must come back as 400
// invalid_config, not the 500 "internal" that writeChatError's default arm
// used to produce. The binary path is a real, launchable fake harness, so a
// 400 can only come from wrapper.validateConfig — which runs inside
// wrapper.Start ahead of startSession, meaning no process is spawned.
//
// Posting directly rather than through the openConversation helper is
// deliberate: that helper t.Fatalf's on any status != 201, which would turn
// the expected 400 into a confusing helper failure.
func TestPermissionMode_InvalidRejectedWith400(t *testing.T) {
	bin := fakeHarnessBin(t)

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	exitAfter := true
	for _, tc := range []struct {
		name string
		path string
		body any
	}{
		{
			name: "turns/unknown-mode",
			path: "/v1/turns",
			body: runTurnRequest{
				Harness:        "claude",
				BinaryPath:     bin,
				Prompt:         "hello",
				ExitAfterTurn:  &exitAfter,
				PermissionMode: "nonsense",
			},
		},
		{
			name: "open/unknown-mode",
			path: "/v1/conversations",
			body: openRequest{
				// chat.Open resolves its adapter by the harness's full name.
				Harness:        "claude-code",
				BinaryPath:     bin,
				PermissionMode: "nonsense",
			},
		},
		{
			// A non-bypass rung paired with an explicit bypass flag is a
			// contradiction the wrapper rejects rather than silently
			// suppressing.
			name: "open/mode-contradicts-args",
			path: "/v1/conversations",
			body: openRequest{
				Harness:        "claude-code",
				BinaryPath:     bin,
				Args:           []string{"--dangerously-skip-permissions"},
				PermissionMode: "plan",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			resp, err := http.Post(ts.URL+tc.path, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body = %s)", resp.StatusCode, raw)
			}
			var out errorResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode error response: %v (body = %s)", err, raw)
			}
			if out.Code != "invalid_config" {
				t.Fatalf("code = %q, want invalid_config (body = %s)", out.Code, raw)
			}
		})
	}

	// Nothing was opened, which is the observable form of "no process was
	// spawned": openConv only publishes an entry after chat.Open returns.
	resp, err := http.Get(ts.URL + "/v1/conversations")
	if err != nil {
		t.Fatalf("GET /v1/conversations: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var convs []conversationSummary
	if err := json.NewDecoder(resp.Body).Decode(&convs); err != nil {
		t.Fatalf("decode conversations: %v", err)
	}
	if len(convs) != 0 {
		t.Fatalf("conversations = %#v, want none", convs)
	}
}

// TestPermissionMode_ThreadedToConfig proves the decoded string actually reaches
// harness.TurnConfig (run-turn) and chat.Options (open) rather than being
// dropped in the handler: an unsupported rung is rejected by
// wrapper.validateConfig *before* the harness process is launched, so the
// wrapper's ErrInvalidConfig text — naming the exact value we sent — can only
// appear if the field was threaded through. (BinaryPath need not exist; config
// validation runs first.) No validation lives in chatd itself, so the failure
// surfaces on the normal error path.
func TestPermissionMode_ThreadedToConfig(t *testing.T) {
	const bogusMode = "definitely-not-a-rung"

	srv := NewServer()
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	exitAfter := true
	cases := []struct {
		name string
		path string
		body any
	}{
		{
			name: "run-turn",
			path: "/v1/turns",
			body: runTurnRequest{
				Harness:        "claude",
				BinaryPath:     "/nonexistent/claude",
				Prompt:         "hello",
				ExitAfterTurn:  &exitAfter,
				PermissionMode: bogusMode,
			},
		},
		{
			name: "open",
			path: "/v1/conversations",
			body: openRequest{
				// chat.Open resolves its adapter by the harness's full name
				// ("claude-code"), unlike harness.RunTurn's "claude".
				Harness:        "claude-code",
				BinaryPath:     "/nonexistent/claude",
				PermissionMode: bogusMode,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			if !bytes.Contains(body, []byte(`"permission_mode":"`+bogusMode+`"`)) {
				t.Fatalf("request body missing permission_mode: %s", body)
			}
			resp, err := http.Post(ts.URL+tc.path, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("POST %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, _ := io.ReadAll(resp.Body)

			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
				t.Fatalf("status = %d, want an error (body = %s)", resp.StatusCode, raw)
			}
			var out errorResponse
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode error response: %v (body = %s)", err, raw)
			}
			if !strings.Contains(out.Error, bogusMode) {
				t.Fatalf("error %q does not name the permission mode we sent (%q)", out.Error, bogusMode)
			}
			if !strings.Contains(out.Error, "PermissionMode") {
				t.Fatalf("error %q is not the wrapper's PermissionMode rejection", out.Error)
			}
		})
	}
}
