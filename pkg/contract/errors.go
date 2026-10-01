package contract

import (
	"errors"
	"fmt"
)

// Code is an error's class, the set closed in 1.0. Callers act on the code,
// its Reason and its Certainty, never on its message.
type Code string

// Error codes.
const (
	// CodeProtocol: a malformed request, a bad version, an unknown enum value.
	CodeProtocol Code = "protocol"
	// CodeBatchTooLarge: the next observation cannot fit max_bytes;
	// RequiredBytes says what would.
	CodeBatchTooLarge Code = "batch_too_large"
	// CodeUnknownField: a request carries a field the receiver does not know.
	CodeUnknownField Code = "unknown_field"
	// CodeUnexpected: the request is not legal in this state.
	CodeUnexpected Code = "unexpected"
	// CodeUnsupported: the request needs a capability the harness lacks.
	CodeUnsupported Code = "unsupported"
	// CodeInvalidSpec: Provision's input violates the spec; Field names what.
	CodeInvalidSpec Code = "invalid_spec"
	// CodeInvalidInput: Send's content is empty, over its bound, or of an
	// undeclared type.
	CodeInvalidInput Code = "invalid_input"
	// CodeBusy, CodePromptPending and CodeBlocked: Send outside idle — busy,
	// awaiting_answer or blocked. Always not_submitted.
	CodeBusy          Code = "busy"
	CodePromptPending Code = "prompt_pending"
	CodeBlocked       Code = "blocked"
	// CodePromptGone and CodeInvalidChoice: Answer's failures.
	CodePromptGone    Code = "prompt_gone"
	CodeInvalidChoice Code = "invalid_choice"
	// CodeInterruptUnconfirmed: the interrupt's outcome was not established
	// within its deadline. The turn stays active until it ends.
	CodeInterruptUnconfirmed Code = "interrupt_unconfirmed"
	// CodeOpenFailed: Open failed; Reason says why.
	CodeOpenFailed Code = "open_failed"
	// CodeExited: the harness's session process has ended.
	CodeExited Code = "exited"
	// CodeClosed: Close has begun on this handle.
	CodeClosed Code = "closed"
	// CodeInternal: anything else. On Send, always maybe_submitted.
	CodeInternal Code = "internal"
)

// Values lists the set.
func (Code) Values() []string {
	return []string{
		"protocol", "batch_too_large", "unknown_field", "unexpected", "unsupported", "invalid_spec",
		"invalid_input", "busy", "prompt_pending", "blocked", "prompt_gone", "invalid_choice",
		"interrupt_unconfirmed", "open_failed", "exited", "closed", "internal",
	}
}

// Certainty is whether a failed Send may have reached the harness.
type Certainty string

// Certainties.
const (
	// NotSubmitted: the adapter guarantees the input will never run. Only a
	// refusal made before anything reached the harness qualifies.
	NotSubmitted Certainty = "not_submitted"
	// MaybeSubmitted: anything else — a timeout, a crash mid-send, an
	// unconfirmed submit. The Host treats it, and a lost result, as
	// outcome_unknown.
	MaybeSubmitted Certainty = "maybe_submitted"
)

// Values lists the set.
func (Certainty) Values() []string { return []string{"not_submitted", "maybe_submitted"} }

// OpenFailure is why Open failed.
type OpenFailure string

// Open failures.
const (
	OpenAdapterMissing     OpenFailure = "adapter_missing"
	OpenBinaryNotFound     OpenFailure = "binary_not_found"
	OpenVersionUnsupported OpenFailure = "version_unsupported"
	OpenCapabilityMissing  OpenFailure = "capability_missing"
	OpenAuthRequired       OpenFailure = "auth_required"
	OpenSessionNotFound    OpenFailure = "session_not_found"
	OpenSessionInUse       OpenFailure = "session_in_use"
	OpenConfigInvalid      OpenFailure = "config_invalid"
	// OpenStateMismatch: a loaded Session's conversation is where the
	// harness looks, but what the harness keeps of it outside its record — a
	// thread's name, its goal — is not what the saved Session had (1.1).
	OpenStateMismatch OpenFailure = "state_mismatch"
)

// Values lists the set.
func (OpenFailure) Values() []string {
	return []string{
		"adapter_missing", "binary_not_found", "version_unsupported", "capability_missing",
		"auth_required", "session_not_found", "session_in_use", "config_invalid", "state_mismatch",
	}
}

// Error is every error the interface returns.
type Error struct {
	Code Code `json:"code"`
	// Reason says why an open failed (CodeOpenFailed).
	Reason OpenFailure `json:"reason,omitempty"`
	// Certainty says whether a failed Send may have reached the harness.
	Certainty Certainty `json:"certainty,omitempty"`
	// Field names the offending field of an invalid or unsupported spec.
	Field string `json:"field,omitempty"`
	// RequiredBytes is the bound the next observation needs
	// (CodeBatchTooLarge).
	RequiredBytes int `json:"required_bytes,omitempty"`
	// Message is for people, at most MaxMessageBytes.
	Message string `json:"message,omitempty"`
}

func (e *Error) Error() string {
	s := string(e.Code)
	if e.Reason != "" {
		s += " (" + string(e.Reason) + ")"
	}
	if e.Field != "" {
		s += " " + e.Field
	}
	if e.Certainty != "" {
		s += " [" + string(e.Certainty) + "]"
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is makes errors.Is(err, &Error{Code: c}) match any *Error with code c.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code && (t.Reason == "" || t.Reason == e.Reason)
}

// Errorf is an *Error with a formatted message.
func Errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// CodeOf is err's code: its *Error's, or CodeInternal.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// CertaintyOf is whether a failed Send may have reached the harness: the
// *Error's certainty, and maybe_submitted for anything else — a lost result
// included.
func CertaintyOf(err error) Certainty {
	var e *Error
	if errors.As(err, &e) && e.Certainty == NotSubmitted {
		return NotSubmitted
	}
	return MaybeSubmitted
}
