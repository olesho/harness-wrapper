package main

import (
	"encoding/json"
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
