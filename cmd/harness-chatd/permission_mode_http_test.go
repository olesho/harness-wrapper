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
