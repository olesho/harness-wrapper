package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// writeChatError maps each chat sentinel to a stable HTTP status + code string.
// The multi-select answer sentinels (ErrNotMultiSelect, ErrConflictingAnswer)
// are checked alongside the pre-existing ErrUnknownOption so a rename or a
// missing case fails loudly.
func TestWriteChatError_Codes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{chat.ErrInvalidOptions, 400, "invalid_options"},
		{chat.ErrUnknownOption, 400, "unknown_option"},
		{chat.ErrNotMultiSelect, 400, "not_multi_select"},
		{chat.ErrConflictingAnswer, 400, "conflicting_answer"},
		{chat.ErrNoInputPending, 409, "no_input_pending"},
		{chat.ErrStaleInputRequest, 409, "stale_input_request"},
		// Not a chat sentinel: wrapper.validateConfig's rejection reaches
		// writeChatError wrapped by chat.Open, and must not fall through to
		// the 500 "internal" default — a rejected permission mode is a
		// security control failing opaquely if it does.
		{wrapper.ErrInvalidConfig, 400, "invalid_config"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeChatError(rec, tc.err)
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			var body errorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Code != tc.code {
				t.Errorf("code = %q, want %q", body.Code, tc.code)
			}
			if body.Error != tc.err.Error() {
				t.Errorf("error = %q, want %q", body.Error, tc.err.Error())
			}
		})
	}
}

// pkg/chat classifies a rejected wrapper.Config as chat.ErrInvalidOptions with
// a multi-%w wrap, so a real chat.Open error satisfies errors.Is for BOTH
// sentinels and the switch order alone decides the code. The wrapper arm is the
// more specific one and must win — invalid_config is the code the frozen
// conformance corpus pins for an invalid effort / permission_mode.
func TestWriteChatError_InvalidConfigWinsOverInvalidOptions(t *testing.T) {
	err := fmt.Errorf("%w: chat: wrapper start: %w", chat.ErrInvalidOptions, fmt.Errorf("%w: Effort must be one of low, medium, high, xhigh, max", wrapper.ErrInvalidConfig))
	if !errors.Is(err, chat.ErrInvalidOptions) || !errors.Is(err, wrapper.ErrInvalidConfig) {
		t.Fatalf("test fixture no longer matches both sentinels: %v", err)
	}

	rec := httptest.NewRecorder()
	writeChatError(rec, err)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	var body errorResponse
	if decErr := json.Unmarshal(rec.Body.Bytes(), &body); decErr != nil {
		t.Fatalf("decode body: %v", decErr)
	}
	if body.Code != "invalid_config" {
		t.Errorf("code = %q, want invalid_config", body.Code)
	}
}
