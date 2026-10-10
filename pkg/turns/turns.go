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
	"errors"
	"time"

	wrapper "github.com/olesho/harness-wrapper/internal/wrapcore"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/transcript"
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

	// Kind categorizes the prompt: "trust_prompt", "bypass_acceptance",
	// "menu_select", "confirm", "text_input", "question" or
	// "question_review". It is the key a declarative policy matches on.
	// "trust_prompt" is the folder-trust dialog and "bypass_acceptance"
	// claude-code's --dangerously-skip-permissions acceptance screen; they are
	// separate kinds so a policy can answer one without answering the other.
	// "question" is a clarifying question the model asks mid-turn, and
	// "question_review" the Submit/Cancel pane after the last answer of a
	// multi-question or multi-select one.
	Kind string

	// Prompt is the question text shown to the user.
	Prompt string

	// Header is a short chip/label for the prompt (e.g. a clarifying
	// question's category). Empty for prompts that carry no header.
	Header string

	// MultiSelect reports whether more than one option may be chosen. When
	// true, each option's Keys is a TOGGLE-ONLY sequence (see InputOption.Keys)
	// and the chat layer appends a single submit key after toggling all
	// selected options — unless the adapter plans the answer itself
	// (AnswerPlanner), as claude-code does for its multi-select questions.
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
	// "no" | "other" | "chat". Empty when the option carries no recognized
	// intent. A question's "other" option takes the answer's text as the
	// answer; its "chat" option declines the question, to talk it over
	// instead.
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
	//   - MultiSelect == false: Keys is a full SELECT-AND-SUBMIT sequence —
	//     it both picks this option and confirms the menu.
	//   - MultiSelect == true: Keys is a TOGGLE-ONLY sequence for this one
	//     option — it must NOT include a submit key. The chat layer toggles
	//     each selected option's Keys and then appends the harness submit key
	//     exactly once.
	//
	// NOTHING enforces the toggle-only invariant at runtime: a producer that
	// bakes a submit into a multi-select option's Keys yields a corrupt
	// toggle+submit+toggle+submit+submit stream. The guards are the
	// multi-select answer unit tests in internal/chatcore and, for claude-code's
	// questions (which an AnswerPlanner answers step by step), the plan tests
	// in pkg/turns/harness/claudecode.
	Keys []byte

	// Highlighted is true when the menu rendered this row as the currently
	// selected choice (codex's "›" marker). SERVER-SIDE ONLY — never surfaced
	// to a client and excluded from the request id hash. The codex approval
	// gate requires it so a quoted-prose spoof cannot false-positive.
	Highlighted bool

	// Checked is true when a multi-select row's checkbox is ticked.
	// SERVER-SIDE ONLY, like Highlighted, and excluded from the request id
	// hash: it changes with every toggle of the same dialog.
	Checked bool

	// Typed is the text typed into an "other" row so far. SERVER-SIDE ONLY
	// and excluded from the request id hash; the row's Label stays its
	// placeholder while the user types.
	Typed string
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
// snapshot a SessionIDExtractor would scrape. While the id is unknown, the chat
// layer feeds every raw line of the harness's output (via the wrapper's
// durable line tap) to this extractor; once a non-empty ID is returned it is
// persisted and no longer queried. An id assigned at launch (SessionAssigner)
// or resumed is known from the start, and the tap is not wired. Lines carry raw ANSI/control bytes, so implementations must tolerate
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
//
// A directory holds every session anyone ran there, so a locator must name
// the session THIS launch started or none: the latest one in the directory
// may be an earlier conversation's, or a concurrent one's.
type SessionIDLocator interface {
	// LocateSessionID returns the UUID of the harness session launched in
	// workingDir at launchedAt: one that started there no earlier than the
	// launch. It returns ("", false) when none can be found, and when more
	// than one could be the launch's — an ambiguous directory is never
	// settled by guessing. A zero launchedAt sets no start bound.
	LocateSessionID(workingDir string, launchedAt time.Time) (string, bool)
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

// EnvConfigurable is an optional capability an adapter implements when its
// on-disk lookups depend on the harness's launch ENVIRONMENT (e.g. Claude
// Code's CLAUDE_CONFIG_DIR, Codex's CODEX_HOME) rather than on $HOME alone.
// A profiled agent is launched with its own config root, so an adapter that
// only knows $HOME reads the OPERATOR's transcripts, or none at all.
//
// The chat layer calls this once at Open with the environment the harness is
// actually launched with (Options.Env) — never os.Environ(), which is a
// DIFFERENT process's view when one process drives several profiled
// conversations. Implementations must treat an absent, empty or whitespace
// value as "unset" (leaving the $HOME default in place) and must apply
// last-occurrence-wins for duplicate keys, matching exec semantics.
type EnvConfigurable interface {
	// ConfigureFromEnv applies the harness launch environment (an
	// os.Environ()-style "K=V" slice) to the adapter's on-disk roots.
	ConfigureFromEnv(env []string)
}

// InterruptOutcome is what the screen says a harness did with the turn in
// flight, as Interrupter.InterruptOutcome reads it.
type InterruptOutcome int

const (
	// InterruptPending: the turn is still running, or the screen does not say
	// yet — a mid-paint frame, a dialog.
	InterruptPending InterruptOutcome = iota
	// InterruptStopped: the harness stopped the turn after it had produced
	// output. Its interrupt marker sits below this turn's prompt, and the
	// partial reply is the text above the marker.
	InterruptStopped
	// InterruptCancelled: the harness cancelled the turn before it produced
	// anything and put the prompt back in the composer.
	InterruptCancelled
	// InterruptFinished: the turn ended on its own — its end-of-turn marker is
	// below this turn's prompt.
	InterruptFinished
)

// Interrupter is an optional capability adapters implement when the harness can
// stop a turn in flight from the keyboard, and the screen says what it did.
// The chat layer's Conversation.Interrupt writes InterruptSequence and reads
// InterruptOutcome; an interrupt made at the terminal is read the same way
// (ADR-007).
type Interrupter interface {
	// InterruptSequence returns the keys that interrupt the turn in flight,
	// written as one write.
	InterruptSequence() []byte

	// InterruptOutcome reads, from one screen, what the harness did with the
	// turn whose prompt is prompt: the reading is per turn, so an interrupt
	// marker left by an earlier turn never speaks for this one. partial is the
	// reply the turn had painted when it was stopped, set with
	// InterruptStopped when the screen shows one. It must only be asked about
	// a turn the harness has taken — the screen showed it working — since
	// before that the composer still holds the prompt as typed, which is also
	// what a cancelled turn leaves there.
	InterruptOutcome(prompt string, snap screen.Snapshot) (outcome InterruptOutcome, partial string)

	// ComposerText returns what the composer holds, as painted. ok is false
	// when the screen shows no composer to read — a dialog, a picker, a
	// mid-paint frame — which never means empty.
	ComposerText(snap screen.Snapshot) (text string, ok bool)

	// ClearComposerSequence returns the keys that empty a composer holding
	// composer (as ComposerText read it), from wherever its cursor is, as one
	// write. Keys that empty a longer text are fine: the caller re-reads the
	// composer until it is empty.
	ClearComposerSequence(composer string) []byte
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

// SessionAssigner is an optional capability adapters implement when the harness
// accepts a caller-chosen id for a FRESH session at launch — claude-code and pi
// both take --session-id <uuid> and name the session's transcript after it.
//
// The chat layer assigns an id on every fresh Open with such an adapter, so the
// id is known from the moment the harness starts. Without one it is learned
// only from what the harness prints — claude's "claude --resume <uuid>" exit
// hint, which recent releases print rarely if at all — and every reading that
// needs it (the transcript verdicts, History) is off until then, which for a
// live conversation is its whole life. Mirrors meta-harness's
// SessionInitializer (src/turns/types.ts), which mints the id the same way.
type SessionAssigner interface {
	// NewSessionID mints a fresh id in the form the harness accepts.
	NewSessionID() string

	// ValidSessionID reports why id cannot name a session of this harness, or
	// nil when it can. It checks the form only; whether the id is already in
	// use is the chat layer's check.
	ValidSessionID(id string) error

	// SessionIDArgs returns the argv fragment that starts a fresh session named
	// id (e.g. {"--session-id", id}).
	SessionIDArgs(id string) []string
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
// (mid-generation, running a tool, or backing off before a retry) versus sitting
// idle at the prompt. The chat layer's idle-completion fallback consults it so it
// never declares a turn complete while the harness is still busy, and Send waits
// on it so nothing is typed into a working harness — the harness's input prompt
// is often painted even while it works, so prompt-readiness alone is not enough
// to distinguish "done" from "thinking". Adapters that can't tell report false.
type BusyDetector interface {
	Busy(snap screen.Snapshot) bool
}

// SwallowedPromptDetector is an optional capability adapters may implement to
// report, from a settled screen, that the harness never accepted the prompt at
// all — as opposed to accepting it and answering.
//
// The two are indistinguishable to the rest of the chat layer: a swallowed
// prompt leaves the harness sitting at a ready prompt with no assistant output,
// which is exactly what a completed turn also looks like once the reply has
// scrolled off. Without this verdict such a run completes "successfully" with
// the raw ready screen as its reply, and a caller that pays per run cannot tell
// the difference (observed in loom's daemon: eight consecutive paid agent runs
// reported complete while producing zero assistant output).
//
// sentScreenText is the screen as it looked when the prompt was submitted, so
// an implementation can answer "nothing changed at all". Adapters that cannot
// tell simply do not implement this.
//
// Ported from meta-harness (turns.SwallowedPromptDetector); the two
// implementations are kept in step.
type SwallowedPromptDetector interface {
	PromptNotAccepted(snap screen.Snapshot, sentScreenText string) bool
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

// PermissionPosture is a FULL reading of the harness's permission posture: the
// canonical rung, the harness's own spelling of it, and whether that spelling
// is one the harness's cycle key can produce.
//
// It exists because a rung alone is not enough to decide "are we there?".
// Several native spellings can share one rung — claude paints both
// "⏸ manual mode on" and "⏵⏵ don't ask on" for the manual rung — and only one
// of the two is a posture a Shift+Tab cycle can actually put the session in.
// A driver that compares rungs alone therefore reads a dontAsk session as
// already-manual and writes no keystroke, leaving the session auto-DENYING
// where the caller asked for per-tool approvals.
type PermissionPosture struct {
	// Rung carries exactly PermissionModeDetector.PermissionMode's contract
	// for this adapter, so the two readers can never disagree about the rung
	// (claude-code's PermissionMode is expressed in terms of this one).
	Rung string

	// Native is the HARNESS's own spelling of the posture — claude's
	// --permission-mode vocabulary ("plan", "default", "acceptEdits",
	// "auto", "bypassPermissions", "dontAsk"). It is DIAGNOSTIC: it belongs
	// in error messages and in a caller that wants to know it is in dontAsk
	// specifically. It must NEVER be compared against
	// wrapper.PermissionRungs() — the two vocabularies overlap by accident
	// ("plan", "auto") and disagree where it matters (claude spells the
	// manual rung "default").
	Native string

	// OnRing reports whether Native is a spelling the harness's cycle key can
	// produce. It is the DECISION field, and the reason this struct exists:
	//
	//   - true  → Rung == target really does mean "the session is in the
	//     posture a cycle to that rung would produce". claude's "acceptEdits"
	//     is a second spelling of the ask rung and is on the ring, so it must
	//     keep satisfying a request for "ask" with zero keystrokes.
	//   - false → an off-ring alias (claude's launch-only "dontAsk"). It
	//     reports its Rung truthfully, but the session is NOT in the posture
	//     a cycle would produce, so a driver asked for that rung must cycle
	//     rather than return early.
	//
	// Phrasing the test as ring membership rather than "is this the canonical
	// native for this rung" means a future off-ring alias gets the right
	// behaviour the moment the footer parser learns its word.
	OnRing bool
}

// PermissionPostureDetector is an optional capability adapters may implement to
// report the full posture (rung + native spelling + ring membership) rather
// than the rung alone. Same consult idiom as BusyDetector and
// PermissionModeDetector: a type assertion on the adapter, answered per screen.
//
// Implemented by the claude-code adapter ONLY. codex deliberately does not:
// its collaboration axis has no alias collision ("plan" and "default" are
// distinct postures, both on its 2-cycle), so implementing it there would add
// a second source of truth for no behaviour change. Drivers must therefore
// keep a fallback for adapters that implement PermissionModeDetector alone.
type PermissionPostureDetector interface {
	// PermissionPosture reports the harness's current posture read from the
	// rendered screen.
	//
	// false means the screen carries NO readable signal — an onboarding wall,
	// a modal covering the footer, a release that renamed the modes — exactly
	// as in PermissionModeDetector. It never means "readable, and not the
	// posture you asked about".
	PermissionPosture(snap screen.Snapshot) (PermissionPosture, bool)
}

// ReadinessDetector is an optional capability adapters implement when their
// harness paints its composer before it will take a message — during startup,
// behind a blocking dialog, or while a turn runs. The chat layer then types a
// message only once ReadyForInput holds, and completes a turn by idleness only
// at a ready prompt. An adapter without it is always ready.
type ReadinessDetector interface {
	// ReadyForInput reports whether the rendered screen shows a composer that
	// will take a message now.
	ReadyForInput(text string) bool
}

// DialogState is what an adapter's DialogDetector sees on a screen. The states
// are distinct because "no dialog" and "a dialog whose choices cannot be read"
// look alike to a yes/no reading, and only the first ever clears on its own.
type DialogState int

const (
	// DialogNone: no blocking dialog on screen.
	DialogNone DialogState = iota
	// DialogPending: a dialog's anchor is up but its choices have not painted
	// yet — a mid-render frame.
	DialogPending
	// DialogUnparseable: a dialog's anchor and choice-shaped lines are up, but
	// no usable option set could be read. It never clears on its own.
	DialogUnparseable
	// DialogReadable: a dialog whose choices were read; the adapter reports it
	// as an InputRequest.
	DialogReadable
)

// DialogDetector is an optional capability adapters implement to tell the
// chat layer which DialogState a screen is in, so that a dialog nothing can
// answer fails a send quickly instead of waiting out its deadline.
type DialogDetector interface {
	DialogState(text string) DialogState
}

// DialogAnchorer is an optional capability adapters implement to name the
// literal lines their harness's blocking dialogs paint, for a caller telling a
// modal from a login wall.
type DialogAnchorer interface {
	DialogAnchors() []string
}

// DialogReader is an optional capability adapters implement when a dialog can
// be read again from any later screen. The chat layer uses it to confirm that
// an answer landed — the dialog left, or its highlight moved — instead of
// trusting the write.
type DialogReader interface {
	// ReadDialog parses the answerable dialog on the screen; false when none
	// can be answered.
	ReadDialog(text string) (*InputRequest, bool)
	// DialogAnchorPresent reports whether a dialog's anchor is still painted,
	// including a dialog whose choices have not rendered yet.
	DialogAnchorPresent(text string) bool
}

// AnswerPlanner is an optional capability adapters implement when an answer to
// one of their dialogs takes several keystroke writes, each of which must be
// seen to land before the next is sent. claude-code's clarifying questions
// are such dialogs: a burst of plain keys reaches claude as one paste, a
// digit only moves the highlight onto a text row, and a multi-select answer
// is toggles, then a commit.
//
// The chat layer runs the plan in order: it writes a step's Keys, then reads
// the dialog back through DialogReader until the step's Until holds, and
// fails the answer, bounded and without sending another key, when it does
// not.
type AnswerPlanner interface {
	// PlanAnswer returns the steps that answer req with the options optionIDs
	// (option IDs, already resolved) and text, for an "other" option. ok is
	// false when the adapter does not plan answers to req's kind; the chat
	// layer then answers it as before. An answer the dialog cannot take —
	// text without an "other" option, an "other" option without text, two
	// options on a single-select question — is an error.
	PlanAnswer(req *InputRequest, optionIDs []string, text string) (steps []AnswerStep, ok bool, err error)
}

// ErrInvalidAnswer is the error PlanAnswer wraps for an answer its dialog
// cannot take.
var ErrInvalidAnswer = errors.New("turns: the answer does not fit the dialog")

// AnswerStep is one write of a planned answer and the evidence that it landed.
type AnswerStep struct {
	// After is how long the dialog must have been up before Keys are
	// written, counted from when the chat layer first saw the request; zero
	// writes at once. It is for a dialog that paints before it takes input:
	// claude-code 2.1.283 drops a key written within 50 ms of its question
	// appearing.
	After time.Duration
	// Keys are the bytes this step writes.
	Keys []byte
	// Until is what the screen must show before the next step is written.
	Until AnswerEvidence
}

// AnswerEvidence is a condition on the request's own dialog, read back
// through DialogReader.
type AnswerEvidence struct {
	// Kind is what to wait for.
	Kind EvidenceKind
	// OptionID names the option the evidence is about (EvidenceHighlighted,
	// EvidenceChecked, EvidenceTyped).
	OptionID string
	// Text is the text EvidenceTyped waits for.
	Text string
}

// EvidenceKind is what an AnswerStep waits for.
type EvidenceKind int

const (
	// EvidenceGone: the request's dialog has left the screen, or another
	// dialog — the next question, the review pane — has replaced it.
	EvidenceGone EvidenceKind = iota
	// EvidenceHighlighted: the option is the highlighted row.
	EvidenceHighlighted
	// EvidenceUnhighlighted: the option is no longer the highlighted row —
	// the highlight moved on to a row that is not an option, such as a
	// multi-select question's Submit row.
	EvidenceUnhighlighted
	// EvidenceChecked: the option's checkbox is ticked.
	EvidenceChecked
	// EvidenceTyped: the option's row shows Text as typed.
	EvidenceTyped
)

// InterstitialDismisser is an optional capability adapters implement when
// their harness paints startup interstitials: screens that block input but
// carry no decision for the caller, which the chat layer clears itself.
type InterstitialDismisser interface {
	// DismissKeys returns the keystrokes that safely dismiss req, and whether
	// req is such an interstitial. updates says whether an update prompt counts
	// as one; by default it is the caller's choice to make.
	DismissKeys(req *InputRequest, updates bool) ([]byte, bool)
}
