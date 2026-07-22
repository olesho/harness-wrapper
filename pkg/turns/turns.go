// Package turns translates low-level harness signals — emulated screen
// state changes and wrapper-level status events — into a small
// vocabulary of chat-oriented turn events: turn complete, tool call,
// blocked, errored.
//
// Adapters implement the per-harness logic. A generic fallback adapter
// (pkg/turns/generic) maps wrapper.Status to turn events without
// looking at the screen at all; per-harness adapters live in
// pkg/turns/harness/<name>/ and add screen-derived signals such as
// prompt-region detection and tool-call markers.
//
// Watcher composes a wrapper.Session, a screen.Screen, and an Adapter
// into a single <-chan Event stream.
package turns

import (
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// Kind is the categorical type of a turn event.
//
// The vocabulary is intentionally small. Adapters with richer signals
// should attach details via Event.Reason and Event.Snap rather than
// growing the kind set.
type Kind string

const (
	// TurnComplete means the assistant has finished its turn and the
	// caller may send the next user message. This is the primary "ready
	// for next message" signal a chat client cares about.
	TurnComplete Kind = "turn_complete"

	// ToolCall means the harness is invoking a tool (shell, file edit,
	// HTTP, …). Informational; turn is still in progress.
	ToolCall Kind = "tool_call"

	// Blocked means the harness reported a transient block — cost,
	// quota, rate limit, or a "retry later" hint. Callers should back
	// off and retry; the turn did not complete.
	Blocked Kind = "blocked"

	// Errored means the harness reported a terminal failure: non-zero
	// exit, signal termination, classifier saw a fatal error. The turn
	// did not complete and is unlikely to recover without intervention.
	Errored Kind = "errored"

	// InputRequested means the harness is blocked on an interactive prompt
	// that the normal Send flow cannot satisfy — e.g. Claude Code's
	// folder-trust dialog at startup. The structured request rides on
	// Event.Input. The chat layer surfaces it (or auto-answers it from a
	// policy) and writes the chosen option's keystrokes back into the PTY.
	InputRequested Kind = "input_requested"

	// InputResolved means a previously-requested interactive prompt is no
	// longer on screen (answered or dismissed). Event.Input carries at
	// least the resolved request's ID.
	InputResolved Kind = "input_resolved"
)

// Event is one observation about the conversation flow.
type Event struct {
	// Kind categorizes the event.
	Kind Kind

	// At is when the event was observed. Watcher backfills this from
	// the originating wrapper.SessionEvent or time.Now() if the adapter
	// left it zero.
	At time.Time

	// Reason is a short human-readable description, surfacing whatever
	// the adapter or wrapper classifier wrote. Not stable for parsing.
	Reason string

	// Snap is the screen snapshot at the moment the event fired, if
	// the event originated from a screen change. nil for events that
	// came from wrapper status transitions only.
	Snap *screen.Snapshot

	// HTTPCode is the upstream API status code when the originating
	// wrapper event carried one (e.g. StatusAPIError with HTTPCode=529).
	// Zero for non-API-error events and for adapter-synthesized events
	// without an upstream code. The Watcher copies this from the
	// SessionEvent automatically; adapters do not need to populate it.
	HTTPCode int

	// RetryAfter is the wait duration the wrapper parsed from the
	// harness's error message. Zero when no hint was available.
	// Watcher-populated like HTTPCode.
	RetryAfter time.Duration

	// Input is the structured interactive prompt for InputRequested /
	// InputResolved events. nil for every other kind.
	Input *InputRequest
}

// InputRequest describes a blocking interactive prompt detected on screen
// that must be answered out-of-band — the normal Send message flow cannot
// satisfy it. Adapters produce it from a screen snapshot; the chat layer
// either auto-answers it from a configured policy or surfaces it to the
// client, then writes the chosen option's Keys back into the PTY.
type InputRequest struct {
	// ID is stable across redraws of the SAME prompt and changes for a
	// genuinely new prompt. Derived from the prompt content so consecutive
	// snapshots of one dialog collapse to a single request.
	ID string

	// Kind categorizes the prompt: "trust_prompt", "menu_select",
	// "confirm", or "text_input". It is the key a declarative policy
	// matches on.
	Kind string

	// Prompt is the question text shown to the user.
	Prompt string

	// Header is a short chip/label for the prompt (e.g. a clarifying
	// question's category). Empty for prompts that carry no header.
	Header string

	// MultiSelect reports whether more than one option may be chosen. When
	// true, each option's Keys is a TOGGLE-ONLY sequence (see InputOption.Keys)
	// and the chat layer appends a single submit key after toggling all
	// selected options. Every prompt produced in this repo today is
	// single-select (false).
	MultiSelect bool

	// Options are the selectable choices for menu/confirm/trust prompts.
	// nil for free-text ("text_input") prompts.
	Options []InputOption
}

// InputOption is one selectable choice in an InputRequest.
type InputOption struct {
	// ID is a stable identifier the answer references (e.g. "1"). Unique
	// within an InputRequest.
	ID string

	// Alias is a portable, harness-agnostic intent a policy can target
	// without knowing the concrete option id: "proceed" | "deny" | "yes" |
	// "no". Empty when the option carries no recognized intent.
	Alias string

	// Label is the human-readable choice text ("Yes, proceed").
	Label string

	// Description is optional longer help text for the option (a
	// clarifying-question choice's explanation). Empty when the option has
	// no description.
	Description string

	// Keys are the bytes to write to the PTY to select this option.
	// SERVER-SIDE ONLY — never surfaced to a client; the client answers
	// semantically via ID or Alias.
	//
	// The meaning of Keys FORKS on the enclosing InputRequest.MultiSelect:
	//   - MultiSelect == false (every prompt in this repo today): Keys is a
	//     full SELECT-AND-SUBMIT sequence — it both picks this option and
	//     confirms the menu.
	//   - MultiSelect == true: Keys is a TOGGLE-ONLY sequence for this one
	//     option — it must NOT include a submit key. The chat layer toggles
	//     each selected option's Keys and then appends the harness submit key
	//     exactly once.
	//
	// NOTHING enforces the toggle-only invariant at runtime: a producer that
	// bakes a submit into a multi-select option's Keys yields a corrupt
	// toggle+submit+toggle+submit+submit stream. Until an adapter DetectInput
	// path emits multi-select prompts, the ONLY guard is the multi-select
	// answer unit test in pkg/chat.
	Keys []byte

	// Highlighted is true when the menu rendered this row as the currently
	// selected choice (codex's "›" marker). SERVER-SIDE ONLY — never surfaced
	// to a client and excluded from the request id hash. The codex approval
	// gate requires it so a quoted-prose spoof cannot false-positive.
	Highlighted bool
}

// Adapter is the per-harness contract that translates raw signals
// (screen state + wrapper status) into turn events.
//
// Implementations must be safe for concurrent calls to OnScreen and
// OnWrapperStatus — Watcher calls them from independent goroutines.
//
// Implementations should be stateless or guard internal state with a
// mutex; the Watcher does not serialize calls.
type Adapter interface {
	// Name identifies the adapter ("generic", "codex", "claude-code", …).
	Name() string

	// OnScreen is called after every successful screen.Write. Returns
	// any turn events the adapter wishes to emit. nil/empty slice means
	// "no events for this snapshot."
	OnScreen(snap screen.Snapshot) []Event

	// OnWrapperStatus is called on every wrapper.SessionEvent. Returns
	// any turn events the adapter wishes to emit.
	OnWrapperStatus(status wrapper.Status, reason string) []Event
}

// SessionIDExtractor is an optional capability adapters may implement
// to surface the harness's own session ID by scraping the rendered
// screen. The chat layer calls this opportunistically; once a non-empty
// ID is returned it is persisted and no longer queried.
type SessionIDExtractor interface {
	// ExtractSessionID returns the harness-assigned session UUID if it
	// appears in the snapshot. Returns ("", false) when no ID is yet
	// visible.
	ExtractSessionID(snap screen.Snapshot) (string, bool)
}

// RawSessionIDExtractor is an optional capability adapters may implement to
// surface the harness's own session ID from a single RAW PTY output line,
// rather than from the rendered screen. Some harnesses (Claude Code) only print
// their session UUID — e.g. the "claude --resume <uuid>" hint — to the normal
// screen as the TUI tears down on exit, where it never lands in the vt100
// snapshot a SessionIDExtractor would scrape. The chat layer feeds every raw
// line of the harness's output (via the wrapper's durable line tap) to this
// extractor; once a non-empty ID is returned it is persisted and no longer
// queried. Lines carry raw ANSI/control bytes, so implementations must tolerate
// non-matching/polluted lines by returning ("", false).
type RawSessionIDExtractor interface {
	// ExtractSessionIDFromLine returns the harness-assigned session UUID if it
	// appears in line, else ("", false).
	ExtractSessionIDFromLine(line string) (string, bool)
}

// SessionIDLocator is an optional capability adapters may implement to recover
// the harness session ID from on-disk state, keyed on the working directory,
// rather than from the rendered screen (SessionIDExtractor) or the raw output
// stream (RawSessionIDExtractor). The chat layer calls this as a fallback at
// TurnComplete when the screen-scrape extractor has not yielded an ID — some
// harnesses (Codex 0.142+) stopped printing the "resume <uuid>" hint to the
// screen, leaving the persisted session log's metadata as the only anchor.
// Because it touches disk it must stay version-independent and tolerate junk
// files by returning ("", false).
type SessionIDLocator interface {
	// LocateSessionID returns the harness session UUID associated with
	// workingDir, or ("", false) if none can be found.
	LocateSessionID(workingDir string) (string, bool)
}

// TranscriptReader is an optional capability adapters may implement to
// provide access to the harness's persisted conversation log. The chat
// layer uses this to hydrate Conversation.History() once a harness
// session ID is known.
type TranscriptReader interface {
	// ReadTranscript locates and parses the harness's session log for
	// the given session ID + working directory and returns the ordered
	// turns. Different harnesses index transcripts differently; some
	// ignore workingDir.
	ReadTranscript(harnessSessionID, workingDir string) ([]transcript.Turn, error)
}

// Quitter is an optional capability adapters may implement to surface the key
// sequence that makes the interactive harness exit gracefully (so it can flush
// state / persist its transcript), instead of being SIGTERM'd. RunTurn sends
// this after a one-shot turn completes and waits briefly for a clean exit
// before escalating to a signal.
type Quitter interface {
	// QuitSequence returns bytes to write to the harness PTY to request a
	// graceful exit (e.g. double Ctrl-C for Claude Code). Empty means the
	// adapter has no graceful-quit sequence; the caller falls back to a signal.
	QuitSequence() []byte
}

// SessionResumer is an optional capability adapters may implement to surface
// the argv fragment that resumes a prior harness session. The chat layer builds
// a resume invocation by splicing this fragment into the launch args; an adapter
// that does NOT implement it is treated as non-resumable (chat returns
// ErrResumeUnsupported — this is exactly why the opencode adapter deliberately
// omits it). Mirrors the TS turns.SessionResumer (src/turns/types.ts).
//
// Deliberate duplication with pkg/harness.Resumer (pkg/harness/harness.go): the
// two layers describe the same shape ({"--resume", id}) but must NOT be merged.
//
//  1. Registry-key mismatch — pkg/harness/claude registers under "claude" while
//     chat identifies this harness as "claude-code", so harness.For("claude-code")
//     fails to find the pkg/harness resumer; the turns adapter is keyed the way
//     chat looks it up.
//  2. Capability divergence — pkg/harness/opencode implements Resumer, but chat
//     treats opencode as non-resumable; only the turns layer (where opencode
//     omits SessionResumer) expresses that policy correctly.
//  3. Composition context — pkg/harness/codex's resumer is a fragment for the
//     headless `codex exec resume` invocation, whereas the turns codex adapter
//     prepends {"resume", uuid} to the interactive TUI argv. Same words,
//     different call sites.
//
// The TS SessionForkResumer counterpart (src/turns/types.ts) is intentionally
// left unported here; porting it is deferred to a follow-up ticket.
type SessionResumer interface {
	// ResumeArgs returns the argv fragment that resumes harnessSessionID (e.g.
	// {"--resume", id}).
	ResumeArgs(harnessSessionID string) []string
}

// SessionControlFlags is an optional capability adapters may implement to list
// the chat-managed session-control flags a caller must not pass in Options.args
// (chat owns session identity/resume/fork, so caller-supplied duplicates of
// these flags would fight it). Mirrors the TS turns.SessionControlFlags
// (src/turns/types.ts). An adapter that omits it declares no reserved flags.
type SessionControlFlags interface {
	// SessionControlFlags returns the flags (e.g. "--resume", "--fork-session")
	// that chat reserves and callers must not supply.
	SessionControlFlags() []string
}

// MessageExtractor is an optional capability adapters may implement to recover
// the assistant's reply text from the rendered screen, stripped of the
// harness's TUI chrome (banner, the echoed prompt, the thinking-summary
// footer, box borders, the input prompt). The chat layer calls it when a turn
// completes to populate Turn.Text with clean output instead of the raw screen
// scrape — the difference between a parseable one-shot reply and a full-screen
// dump. Returns ("", false) when the adapter can't isolate the message, in
// which case the caller falls back to the raw snapshot text.
type MessageExtractor interface {
	ExtractMessage(snap screen.Snapshot) (string, bool)
}

// BusyDetector is an optional capability adapters may implement to report, from
// the rendered screen, whether the harness is still working on the current turn
// (mid-generation or running a tool) versus sitting idle at the prompt. The
// chat layer's idle-completion fallback consults it so it never declares a turn
// complete while the harness is still busy — the harness's input prompt is
// often painted even while it works, so prompt-readiness alone is not enough to
// distinguish "done" from "thinking". Adapters that can't tell report false.
type BusyDetector interface {
	Busy(snap screen.Snapshot) bool
}

// PermissionModeDetector is an optional capability adapters may implement to
// report the harness's permission posture as painted on the rendered screen.
// It is the same shape of question as BusyDetector — a per-screen
// "ask the adapter, if it knows how to answer" consult, in the mould of
// pkg/chat/conversation.go:798's
// `if bd, ok := c.adapter.(turns.BusyDetector); ok && bd.Busy(snap)`.
//
// Implemented by the claude-code and codex adapters; deliberately absent on
// opencode, pi and generic, which paint no marker this can read.
type PermissionModeDetector interface {
	// PermissionMode reports the harness's current posture on its PRIMARY
	// permission axis, read from the rendered screen:
	//
	//   - claude-code: a canonical rung from wrapper.PermissionRungs()
	//     (plan|manual|ask|auto|bypass).
	//   - codex: a COLLABORATION-axis value ("plan" or "default"), which is
	//     NOT a rung. codex's permissions rung lives on a second axis that
	//     this interface deliberately does not model.
	//
	// false means the screen carries NO readable signal — an onboarding
	// wall, a modal covering the footer, a harness that paints no marker.
	// It never means "readable, and not plan": callers can therefore tell
	// an unreadable screen from a healthy session. Callers that need "is
	// this a canonical rung?" test membership in wrapper.PermissionRungs().
	PermissionMode(snap screen.Snapshot) (string, bool)
}
