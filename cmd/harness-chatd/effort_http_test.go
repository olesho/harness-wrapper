package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEffort_InvalidRejectedWith400 is the end-to-end half of the effort error
// mapping. pkg/chat classifies wrapper.validateConfig's rejection as
// chat.ErrInvalidOptions (keeping wrapper.ErrInvalidConfig matchable through
// the multi-%w), so a bad effort is a 400 rather than the 500 "internal" that
// writeChatError's default arm would produce for an unclassified error.
//
// The code stays "invalid_config": writeChatError checks the more specific
// wrapper sentinel first, which is what the frozen conformance corpus
// (gateway/errorResponse.invalid_config.json) and docs/md/guide/gateway.md pin
// for an invalid effort.
//
// binary_path is deliberately nonexistent: resolveAdapter("codex") succeeds and
// validateConfig runs inside wrapper.Start ahead of startSession, so the
// rejection lands before anything is exec'd — a 200/500 here would mean the
// check moved after launch.
func TestEffort_InvalidRejectedWith400(t *testing.T) {
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
			name: "open/unknown-effort",
			path: "/v1/conversations",
			body: openRequest{
				Harness:    "codex",
				BinaryPath: "/nonexistent",
				Effort:     "hgih",
			},
		},
		{
			// "high" is a valid rung, but pi has no effort axis at all.
			name: "open/harness-without-effort-axis",
			path: "/v1/conversations",
			body: openRequest{
				Harness:    "pi",
				BinaryPath: "/nonexistent",
				Effort:     "high",
			},
		},
		{
			name: "turns/unknown-effort",
			path: "/v1/turns",
			body: runTurnRequest{
				Harness:       "codex",
				BinaryPath:    "/nonexistent",
				Prompt:        "hello",
				ExitAfterTurn: &exitAfter,
				Effort:        "hgih",
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

	// A rejected open publishes nothing: openConv only records an entry after
	// chat.Open returns.
	resp, err := http.Get(ts.URL + "/v1/conversations")
	if err != nil {
		t.Fatalf("GET /v1/conversations: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var convs []conversationSummary
	if err := json.NewDecoder(resp.Body).Decode(&convs); err != nil {
		t.Fatalf("decode conversation list: %v", err)
	}
	if len(convs) != 0 {
		t.Fatalf("conversations = %d, want 0", len(convs))
	}
}
