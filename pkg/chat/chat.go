// Package chat is the Go-level chat-conversation API on top of
// harness-wrapper. A Conversation owns one PTY-supervised harness
// process (Codex, Claude Code, …) and exposes a small interface:
// acquire exclusive control, send a user message, observe turn-level
// state transitions.
//
// The package is the substrate that transport layers (HTTP, gRPC, …)
// import. Transport concerns — framing, streaming protocol, auth — are
// not part of this package and live in separate cmd/ binaries.
//
// Lifecycle:
//
//	conv, err := chat.Open(ctx, chat.Options{
//	    Harness:    "codex",
//	    BinaryPath: "/usr/local/bin/codex",
//	})
//	defer conv.Close(context.Background())
//
//	release, err := conv.AcquireControl(ctx)
//	defer release()
//
//	turnID, err := conv.Send(ctx, "hello")
//	for ev := range conv.Events() {
//	    if ev.Type == EventTurn && ev.Turn.ID == turnID && ev.Turn.State == TurnStateComplete {
//	        break
//	    }
//	}
//
// Concurrency: all Conversation methods are safe for concurrent use.
// Send specifically requires that the caller's goroutine has previously
// acquired control via AcquireControl; otherwise it returns
// ErrNoControl.
package chat

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Role identifies who produced a turn.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// TurnState is the lifecycle stage of a Turn.
type TurnState string

const (
	// TurnStatePending means the turn has been recorded but no output
	// has streamed yet. Applies to assistant turns from the moment
	// Send returns until the first byte is observed.
	TurnStatePending TurnState = "pending"

	// TurnStateStreaming means output is actively arriving. v1 does not
	// surface per-delta events, so most assistant turns transition
	// directly Pending → Complete.
	TurnStateStreaming TurnState = "streaming"

	// TurnStateComplete means the turn finished cleanly — the adapter
	// observed a turn-complete signal (Codex token footer, Claude Code
	// thinking summary, or wrapper waiting_for_input).
	TurnStateComplete TurnState = "complete"

	// TurnStateErrored means the turn ended in failure: harness exited,
	// the user interrupted, or the adapter reported an unrecoverable
	// error. Reason carries the detail.
	TurnStateErrored TurnState = "errored"
)

// ReasonAuthRequired is the canonical Turn.Reason recorded when a turn ended in
// failure because the harness CLI is logged out / its login has expired
// (claude-code "Not logged in · Please run /login"; codex "401 Unauthorized" /
// "Not logged in"). The stable "auth_required:" prefix is a machine token
// consumers match to tell "renew the harness login" apart from a genuine task
// failure — instead of re-scraping the rendered screen themselves. Set only when
// a turn errored AND the terminal screen showed a logout banner (see
// Conversation.handleTurnsEvent); it explains a failure, it never completes one.
const ReasonAuthRequired = "auth_required: harness login expired or re-authentication required — renew the harness login"

// ReasonUsageLimited is the canonical Turn.Reason recorded when a turn produced no
// assistant reply because the harness subscription's usage/session window is
// exhausted — claude-code renders a wall ("You've hit your session limit · resets
// 10:20pm …") in place of a reply. Like ReasonAuthRequired the stable
// "usage_limit:" prefix is a machine token consumers match to tell a TRANSIENT
// quota outage (retry once the window resets) apart from a genuine task failure —
// so an orchestrator can reopen the task blamelessly instead of counting it toward
// a runaway/block guard. The specific reset time rides along in a trailing "(…)"
// detail. Set only at a turn's terminal point when the "reply" was in fact the wall
// (see Conversation.usageLimitRelabel).
const ReasonUsageLimited = "usage_limit: harness usage or session limit reached — retry after the quota window resets"

// Turn is one message in the conversation.
type Turn struct {
	ID          string
	SessionID   string
	Role        Role
	State       TurnState
	Text        string // populated for user turns at send time, and for assistant turns from the screen extract once Complete
	Reason      string // non-empty for Errored turns; mirrors adapter event Reason
	StartedAt   time.Time
	CompletedAt time.Time

	// HTTPCode is the upstream API status code carried with a Blocked
	// transition when the wrapper recognized an api_error event
	// (claudecode "API Error: 529", Codex
	// "exceeded retry limit, last status: 503"). Zero for non-api
	// blocks and for transport-level errors with no numeric code.
	HTTPCode int

	// RetryAfter is the wait duration parsed from the harness's error
	// message (e.g. "Retry after 30 seconds"). Zero when no hint was
	// parseable. Consumers can read this to schedule their retry.
	RetryAfter time.Duration
}

// EventType discriminates the variants of a ConversationEvent.
type EventType string

const (
	// EventTurn carries a Turn state transition. Turn is populated.
	EventTurn EventType = "turn"

	// EventInputRequest signals the harness is blocked on an interactive
	// prompt that needs an out-of-band answer. Input is populated; answer it
	// with Conversation.Answer. Emitted only when no configured policy or
	// handler resolved the request server-side.
	EventInputRequest EventType = "input_request"

	// EventInputResolved signals a previously-requested prompt is no longer
	// pending (answered or dismissed). Input is populated with at least the
	// resolved request's ID.
	EventInputResolved EventType = "input_resolved"
)

// ConversationEvent is a discriminated event observed on
// Conversation.Events(). Inspect Type to learn which payload is set: Turn
// for EventTurn, Input for EventInputRequest / EventInputResolved.
type ConversationEvent struct {
	// Type selects the populated payload. For back-compat, code that only
	// cares about turns may read Turn directly: it is the zero Turn (empty
	// ID) for non-turn events.
	Type EventType

	// Turn is the turn state transition for EventTurn. Zero for input events.
	Turn Turn

	// Input is the interactive prompt for EventInputRequest /
	// EventInputResolved. nil for EventTurn.
	Input *InputRequest

	// Err is non-nil if the event represents an out-of-band error, e.g.
	// Store failures. It is independent of Turn.State == TurnStateErrored
	// (which represents harness-side failures).
	Err error
}

// Session is the chat-level session record. Distinct from
// wrapper.Session: this is the persistence/metadata view, owned by Store.
type Session struct {
	ID         string
	Harness    string
	WorkingDir string
	CreatedAt  time.Time
	// HarnessSessionID is the ID the underlying harness assigned to its
	// own session (Codex's resume UUID, Claude Code's session UUID). It is
	// populated once the adapter surfaces it — from the rendered screen
	// (turns.SessionIDExtractor) or the raw output line stream
	// (turns.RawSessionIDExtractor, e.g. Claude Code's "claude --resume <uuid>"
	// exit hint). Empty until then, and for harnesses with no extractor.
	HarnessSessionID string
}

// Sentinel errors.
var (
	// ErrInvalidOptions is returned by Open when Options is incomplete
	// or inconsistent.
	ErrInvalidOptions = errors.New("chat: invalid options")

	// ErrUnknownHarness is returned by Open when Options.Harness names
	// no registered adapter.
	ErrUnknownHarness = errors.New("chat: unknown harness")

	// ErrNoControl is returned by Send when no caller has acquired
	// control. Acquire via AcquireControl first.
	ErrNoControl = errors.New("chat: control token not held")

	// ErrTurnInFlight is returned by Send when a previous assistant
	// turn is still Pending or Streaming. Wait for it to complete (or
	// error) before sending the next message.
	ErrTurnInFlight = errors.New("chat: previous turn still in flight")

	// ErrClosed is returned by methods called after Close.
	ErrClosed = errors.New("chat: conversation closed")

	// ErrInputPending is returned by Send when the harness is blocked on an
	// interactive prompt awaiting an external answer. Answer it (or wait for
	// EventInputResolved) before sending. Not returned when a policy or
	// handler is auto-answering the prompt — in that case Send waits.
	ErrInputPending = errors.New("chat: blocked on interactive input request")

	// ErrAuthRequired is returned by waitReadyForSend when the harness cannot
	// reach a ready prompt because it is sitting in a logged-out / not-onboarded
	// screen (a sign-in wizard, login-method picker, or re-auth banner) that
	// never clears on its own. Send catches it and records a terminal assistant
	// turn carrying ReasonAuthRequired, so the onboarding case surfaces the same
	// canonical signal as the completion- and error-path cases instead of
	// hanging to the run deadline.
	ErrAuthRequired = errors.New("chat: harness requires authentication / onboarding")

	// ErrNoInputPending is returned by Answer when no interactive prompt is
	// currently awaiting an answer.
	ErrNoInputPending = errors.New("chat: no input request pending")

	// ErrStaleInputRequest is returned by Answer when the supplied request
	// ID does not match the prompt currently on screen (it changed or was
	// already resolved).
	ErrStaleInputRequest = errors.New("chat: stale input request id")

	// ErrUnknownOption is returned by Answer when the supplied option id or
	// alias matches none of the request's options.
	ErrUnknownOption = errors.New("chat: unknown input option")

	// ErrNotMultiSelect is returned by Answer when OptionIDs is supplied for a
	// request whose MultiSelect flag is false.
	ErrNotMultiSelect = errors.New("chat: option_ids given for a single-select request")

	// ErrConflictingAnswer is returned by Answer when both OptionID and
	// OptionIDs are set (the singular and plural selection channels conflict).
	ErrConflictingAnswer = errors.New("chat: option_id and option_ids both set")

	// ErrQuitUnsupported is returned by Quit when the harness adapter exposes
	// no graceful-quit sequence (it does not implement turns.Quitter). The
	// caller should fall back to Close, which signals the process.
	ErrQuitUnsupported = errors.New("chat: harness has no graceful-quit sequence")

	// ErrResumeUnsupported is returned by Open/Reopen when the harness adapter
	// cannot build resume args (it does not implement turns.SessionResumer).
	// Call sites wrap it with the harness name; errors.Is still matches.
	ErrResumeUnsupported = errors.New("chat: harness has no resume sequence")

	// ErrNoHarnessSession is returned by Reopen when the stored session carries
	// no harness session id (never captured, so there is nothing to resume).
	// Call sites wrap it with the session id; errors.Is still matches.
	ErrNoHarnessSession = errors.New("chat: session has no harness session id")
)

// newID returns a fresh 16-byte hex ID. Used for chat-level Session
// and Turn IDs.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
