# Chat API

`pkg/chat` is the Go-level chat-conversation API built on top of [`pkg/wrapper`](../internal/wrapper.md).
A `Conversation` owns one PTY-supervised harness process (Codex, Claude Code, …) and exposes a small
interface: acquire exclusive control, send a user message, observe turn-level state transitions, read
history.

It is the substrate that transport layers import. Framing, streaming protocol, and auth are not part
of this package — they live in `cmd/` binaries like [`harness-chatd`](gateway.md).

## Lifecycle at a glance

The sequence below traces one full round-trip: `Open` starts the wrapper session, screen, and
[watcher](../internal/turns.md); `Send` writes to the PTY; the watcher detects turn completion and
fires an event; `History` reads the harness's own transcript.

![Conversation sequence](../diagrams/chat-sequence.svg)

```go
ctx := context.Background()
conv, err := chat.Open(ctx, chat.Options{
	Harness:    "codex", // "claude-code" | "opencode" | "generic"
	BinaryPath: "/usr/local/bin/codex",
	WorkingDir: "/path/to/project",
	Store:      memstore.New(),
})
if err != nil { return err }
defer conv.Close(ctx)

release, err := conv.AcquireControl(ctx) // FIFO queue, ctx-cancellable
if err != nil { return err }
defer release()

turnID, err := conv.Send(ctx, "summarize this project")
if err != nil { return err }

for ev := range conv.Events() {
	if ev.Type == chat.EventTurn && ev.Turn.ID == turnID && ev.Turn.State == chat.TurnStateComplete {
		break
	}
}

history, _ := conv.History(ctx)
```

## Open

```go
type Options struct {
	Harness     string   // "codex" | "claude-code" | "opencode" | "generic"  (required)
	BinaryPath  string   // harness executable                                                  (required)
	Args        []string // passed verbatim to the harness
	Resume      string   // harness session id to resume; Args must not carry any flag the
	                     // adapter reserves via turns.SessionControlFlags
	HarnessSessionID string // id for a FRESH session, where the adapter takes one
	                     // (claude-code); minted when empty — see History below
	Transport   Transport // TransportTUI (default) | TransportStreamJSON (claude-code) — see below
	WorkingDir  string
	Env         []string
	Effort      string   // reasoning effort ("" = harness default)
	Model       string   // model for this run ("" = harness default)
	PermissionMode string // launch-time permission rung ("" = harness default)
	KeepAliveOnClassification bool // never end the harness on a classification (see below)
	Cols, Rows  int      // default 120×40
	Store       Store    // required; use memstore.New() for the in-process default
	EventBuffer int      // default 32; Events() channel size

	InputPolicy               *InputPolicy                            // declarative answers (see below)
	OnInputRequest            func(InputRequest) (InputAnswer, bool)  // in-process answer callback
	DisableCodexAutoDismiss   bool                                    // keep Codex startup interstitials
	AutoSkipCodexUpdateNotice bool                                    // auto-Skip Codex's "Update available!" menu
}

func Open(ctx context.Context, opts Options) (*Conversation, error)
```

`Open` resolves the per-harness `turns.Adapter`, creates a `screen.Screen`, starts a
`wrapper.Session` pointed at the screen, claims the wrapper-level writer lock for the conversation's
lifetime, and spawns a `turns.Watcher` whose events drive turn-state transitions.

Returns `ErrInvalidOptions` when a required field is missing, when a required field is present but
invalid (`Cols`/`Rows` over `math.MaxUint16`), when `Resume` collides with an argument the adapter
reserves via `turns.SessionControlFlags`, or wrapping `wrapper.ErrInvalidConfig` for a knob the
wrapper rejects. `ErrResumeUnsupported` is a different failure: the resolved adapter does not
implement `turns.SessionResumer` at all, so `Resume` cannot be honoured. Also returns
`ErrUnknownHarness` (`Harness` not registered), or a wrapped wrapper/store error.

`Open` does **not** wait for the harness to finish booting — it returns as soon as the process is
supervised. Readiness is enforced later, inside [`Send`](#readiness-what-send-waits-for).

**Keep-alive.** By default the wrapper supervises the harness to completion: once its output has sat
quiet for a minute, a phrase in it such as "rate limit" or "please try again" ends the process, and so
does a real usage-limit wall. That suits a caller that runs one job and stops. A conversation you keep
open between messages sets `KeepAliveOnClassification`: nothing the harness prints ends it, silence is
never read as evidence, and walls are still reported — on the turn, where `Code` and `ResumeAt` say
what happened and when to retry ([ADR-006](../internal/decisions/adr-006-classification-and-lifetime.md)).
The gateway opens every conversation this way.

**Transport.** A conversation reads its harness's screen by default (`TransportTUI`). claude-code can
instead run on its machine protocol, `TransportStreamJSON`: one `claude -p --input-format stream-json
--output-format stream-json` process on pipes, whose frames state what the screen only shows — a
`result` per turn, `api_retry` while a turn retries, a receipt for each message and each interrupt
([ADR-009](../internal/decisions/adr-009-stream-json-transport.md)). The Conversation API is the same:
`Send` returns once claude has received the message, a turn ends on its result (text, or the API
error's status, reason and code), `Interrupt` reports `stopped`, `cancelled`, `no_turn` or `too_late`
as claude settled it, and events, `State`, `Quit`, `Close` and `History` behave as on the TUI. Nothing
is rendered: `ScreenSnapshot` is empty, `Resize` does nothing, `Wrapper` is nil, and `Containment` is
refused. Below the bypass rung, claude's permission prompts arrive as `InputRequest`s of kind
`permission_prompt` with the options `allow` and `deny`. On a claude.ai subscription account, claude
also reports the account's usage limit whenever it changes; it arrives as `EventRateLimit` and in
`State().RateLimit` (see [Events](#events)). Pass the same `Transport` to `Reopen`; the session record
does not store it.

### Reopen

```go
func Reopen(ctx context.Context, opts ReopenOptions) (*Conversation, error)
```

`Reopen` restarts a **stored** session: it loads the record from the `Store`, resumes the harness with
the recorded harness session id, and returns a fresh `Conversation`. `Harness`, `WorkingDir` and the
resume id come from the record; everything else (`BinaryPath`, `Args`, `Env`, the execution-mode knobs,
`KeepAliveOnClassification`, `Cols`/`Rows`, `InputPolicy`, …) you supply again.

A record with no harness session id cannot be resumed: that is `ErrNoHarnessSession`.

> Two open-time knobs are **not** carried on `ReopenOptions`: the codex update-notice auto-skip and the
> permission-mode render budget. A resumed codex session can therefore stop on the update menu where
> the original would have skipped it — supply an `InputPolicy` for `codex_update_notice` if that
> matters to you.

## Control acquisition

```go
release, err := conv.AcquireControl(ctx)
```

`AcquireControl` is a **FIFO mutex**. The first caller gets the token immediately; others queue and
are served in order. If `ctx` cancels before a waiter is served, it returns `ctx.Err()` and leaves the
queue. `Send`, `Answer` and `SetPermissionMode` return `ErrNoControl` if no caller currently holds the
token.

Within one process, control is **advisory**: the token is not bound to the caller that acquired it.
`Send` and friends check only that *some* caller holds it, so every goroutine that writes to the
harness must take it itself, and raw writes through `conv.Wrapper().WriteStdin` bypass it — hold the
token around them if anything else may be sending. [harness-chatd](gateway.md) is what binds a token
to one client across a process boundary.

The wrapper-level writer lock is held by the `Conversation` from `Open` to `Close`; `AcquireControl`
is the *chat-level* token coordinating multiple chat clients sharing one conversation.

## Sending messages

```go
func (c *Conversation) Send(ctx context.Context, text string) (turnID string, err error)
```

`Send` waits for the harness to be ready, records a `RoleUser` turn (immediately `TurnStateComplete`)
and a `RoleAssistant` turn (`TurnStatePending`) in the `Store`, then writes the prompt to the harness
PTY. The watcher advances the assistant turn as the harness works:

![Turn lifecycle](../diagrams/turn-lifecycle.svg)

`Send` returns `ErrNoControl` (no token held), `ErrTurnInFlight` (a prior assistant turn is still
pending/streaming), or `ErrInputPending` (an interactive prompt is awaiting an answer). For richer
input — control characters, a paste sequence, a slash command you want to type by hand — reach past
the API via `conv.Wrapper().WriteStdin(...)`.

`Send` types into an empty composer only. Where the adapter can read the composer (claude-code,
through `turns.Interrupter`), it first empties whatever the composer holds — a prompt a cancelled turn
put back, a draft typed at the terminal — and returns `ErrComposerNotCleared`, with nothing typed and
no turn recorded, if it will not clear. A composer it cannot read is typed into as before.

The prompt goes out as one write — no per-character typing, no inter-key delay, no retry — and its
submit key as a second, once the composer shows the prompt, so a harness assembling a paste cannot
take the key for pasted text. Nothing else is written between the two: an [interrupt](#interrupting-a-turn)
waits for the submit key. The submit key is per-harness, because modern TUIs enable the enhanced
keyboard protocol where a bare carriage return only inserts a newline:

| Harness | Submit key |
|---|---|
| claude-code, codex | `CSI 13u` (`\x1b[13u`) |
| anything else | `\n` |

### Readiness: what `Send` waits for

For harnesses whose composer is detectable (claude-code, codex), `Send` blocks until the screen
shows a ready prompt. This is what keeps a prompt from being typed into a boot screen or a modal and
silently lost. A harness keeps its composer painted while it works, so where the adapter can tell it
is busy (claude-code) `Send` also waits until it has been idle for the end-of-turn confirmation
window — nothing is ever typed into a turn that is still running, whoever started it. claude-code's
busy reading is its live status region only: the status line above the composer box (the spinner, or
the retry countdown while it backs off) and the footer below it, so a reply that quotes those markers
does not hold `Send`. While waiting it can end in four other ways:

- `ErrInputPending` — a blocking prompt is waiting for **your** answer (a policy or callback that is
  answering one itself does not count; `Send` waits for it to clear).
- `ErrAuthRequired` — the harness is sitting on a login or onboarding wall. An onboarding wall
  short-circuits immediately; a softer "not logged in" banner must persist briefly before it counts,
  so a transient render cannot trip it. The prompt is deliberately **not** written — it would land in
  a sign-in menu. `Send` records an errored assistant turn carrying `ReasonAuthRequired` and returns
  its id with a nil error.
- `ErrHarnessBusy` — `ctx` ended while the harness was still working. Nothing was typed and no turn
  was recorded; it also wraps `ctx.Err()`.
- `ctx.Err()` / `ErrClosed`.

There is **no internal send timeout**: your `ctx` is the only clock. Harnesses with no readiness
marker (opencode, generic) skip the gate entirely.

### Turn model

```go
type Role string      // RoleUser | RoleAssistant | RoleSystem
type TurnState string  // TurnStatePending | TurnStateStreaming | TurnStateComplete | TurnStateErrored | TurnStateInterrupted

type Turn struct {
	ID, SessionID string
	Role          Role
	State         TurnState
	Text          string
	Reason        string
	StartedAt, CompletedAt time.Time
	Code          TurnCode       // auth_required | usage_limited | billing_wall; "" for every other turn
	HTTPCode      int            // upstream code when a turn errors on an API error
	RetryAfter    time.Duration  // wait hint parsed from the harness's error
	ResumeAt      time.Time      // when a usage_limited turn's window reopens, from the wall's own text
}
```

There is exactly **one assistant turn per `Send`**; it ends in `TurnStateComplete` (the adapter saw
turn completion), `TurnStateErrored` (the harness errored, was blocked, or exited) or
`TurnStateInterrupted` (it was [interrupted](#interrupting-a-turn) and the harness said so — neither a
success nor a failure; `Text` holds the partial reply, if any).
`TurnStateStreaming` is reserved: v1 emits no per-delta events, so turns go pending → complete.

Two `Reason` values are **stable prefixes** you may match on, rather than free text:

| Constant | Meaning |
|---|---|
| `ReasonAuthRequired` | the harness needs a login / re-authentication before it can work |
| `ReasonUsageLimited` | a usage or session limit was hit; the harness's own message is appended in parentheses |

### How a turn completes

![How a turn is gated and completed](../diagrams/turn-completion.svg)

Three routes end a pending turn:

1. **The adapter recognises the harness's end-of-turn marker.** For a harness that streams a reply in
   several parts (claude-code), a marker alone does not complete the turn — it arms a **short confirm
   window**, and any repaint restarts it, so a mid-stream flicker cannot finish the turn early.
   Harnesses whose marker is emitted once (codex) complete immediately.
2. **Idle fallback.** With no marker, a turn completes when the composer is ready, the harness is not
   busy (for adapters that implement a busy detector), and the screen has been quiet for a longer
   window.
3. **The wrapper speaks.** A status transition — exit, signal, cost/quota, API error, or
   `waiting_for_input` — is mapped to a turn event by the [generic adapter](adapters.md#generic) that
   every adapter embeds.

In a [keep-alive](#open) conversation whose adapter reads the harness's transcript (claude-code),
a cost/quota or API-error transition does **not** end the turn: the harness may still be retrying. It
records `HTTPCode` and `RetryAfter` on the turn and holds it until the harness ends it by route 1, 2 or
its exit — and then the harness's own record decides: a tagged entry errors the turn with its tag, a
reply after the error completes it, and a turn the transcript cannot settle ends errored with the
transition it was held on ([ADR-006](../internal/decisions/adr-006-classification-and-lifetime.md)).

Both windows are tuned per harness and are **not** part of the wire contract: treat them as
"eventually, quickly" rather than a guaranteed latency. They are overridable only from within the
package (tests), because a caller that needs a hard bound should use its own `ctx`.

## Interrupting a turn

```go
func (c *Conversation) Interrupt(ctx context.Context) (InterruptResult, error)
```

`Interrupt` stops the turn in flight ([ADR-007](../internal/decisions/adr-007-interrupt.md)). It needs
**no control token** — the holder is typically waiting on the very turn, as `RunTurn` does — and it
never lands inside a submit: it waits for a `Send`'s submit key, then for the harness to show it has
taken the prompt, and writes the adapter's interrupt key once (claude-code: Esc, as `CSI 27 u`).
Concurrent calls for one turn share that key. The turn ends only when the harness says what it did:

| Result | What happened | The turn |
|---|---|---|
| `InterruptStopped` | the harness stopped the turn mid-reply or mid-tool | `TurnStateInterrupted`, `Text` = the partial reply (the transcript's once it records the interrupt, else the screen's; a tool call is not reply text) |
| `InterruptCancelled` | the harness cancelled it before its first token and put the prompt back in the composer; chat empties the composer | `TurnStateInterrupted`, no text |
| `InterruptTooLate` | the turn finished first; no key was written | keeps its own outcome |
| `InterruptNoTurn` | no turn was in flight; nothing was written | — |

`ErrInterruptUnconfirmed` (wrapping `ctx.Err()`) means `ctx` ended before the harness answered: the
key may have gone out, and the turn stays in flight and ends as the harness ends it. A cancelled turn
whose prompt will not clear from the composer returns `InterruptCancelled` with
`ErrComposerNotCleared`. An adapter without `turns.Interrupter` returns `ErrInterruptUnsupported`
(codex today).

An interrupt made at the harness's own terminal — someone pressing Esc — ends the turn the same way,
and its `Reason` says "(at the terminal)". The reading is per turn: an interrupt marker an earlier turn
left on screen never ends a later one. While an interrupt waits for the harness's answer, the idle
fallback does not complete the turn from the reply it cut short.

## Events

```go
type EventType string
const (
	EventTurn          EventType = "turn"
	EventInputRequest  EventType = "input_request"
	EventInputResolved EventType = "input_resolved"
	EventExited        EventType = "exited"
	EventRateLimit     EventType = "rate_limit" // stream-json transport only
)

type ConversationEvent struct {
	Type      EventType     // which payload is set
	Turn      Turn          // affected turn (EventTurn; zero otherwise)
	Input     *InputRequest // interactive prompt (EventInputRequest / EventInputResolved)
	Exit      *ExitInfo     // how the process ended (EventExited)
	RateLimit *RateLimit    // the account's usage limit (EventRateLimit)
	Err       error         // non-nil only for chat-level errors (e.g. Store failures)
}

func (c *Conversation) Events() <-chan ConversationEvent
```

`EventTurn` fires on every turn-state change: the initial user turn, the initial assistant turn
(`pending`, queued before the prompt is typed, so nothing about the turn can precede it) and its one
terminal event. Switch on `Type`; turn-only consumers can read `Turn` directly (it is the zero `Turn`
for other events). `EventExited` is the last event: the harness process ended, the turn that was in
flight has had its terminal event, and `Exit` says how — status, exit code, signal, reason, error
class.

`EventRateLimit` comes only from the stream-json transport, when claude reports that a claude.ai
subscription account's usage limit changed; an API-key account never sends one. `RateLimit` holds
claude's figures as reported: `Status` (`allowed`, `warning` near the limit, `rejected` over it, or
`unknown` for a status this build does not know), `ResetsAt`, the limiting `Window` (claude's name,
such as `five_hour`), `Utilization` when reported, the overage fields, and `Windows` — the usage of
every window claude tracks, from a part of its report claude marks internal, so a release may change
or drop it. The event ends no turn and blocks nothing: a turn a wall refused still ends with its own
`Code` and `ResumeAt`.

### Delivery: `OnEvent` and `Events()`

Every event goes through one bounded queue and one worker
([ADR-008](../internal/decisions/adr-008-event-delivery.md)), which hands it to `Options.OnEvent` —
in order, once each — and then offers it to `Events()`:

- **`OnEvent`** is the reliable consumer. It is called from one goroutine, outside every lock of the
  conversation. When its queue is full (`Options.EventQueue`, default 1024 events or 16 MiB), a
  producer waits for room rather than dropping — so a slow `OnEvent` slows the conversation, but never
  an `Interrupt`, the harness's exit or `Close`. It must not call back into the `Conversation` or wait
  for anything that is waiting on it. An event larger than the byte bound arrives without its text,
  carrying `ErrEventTooLarge`; `History` keeps the text.
- **`Events()`** is the best-effort view of the same stream: if its buffer (`EventBuffer`, default 32)
  is full, the event is dropped for it. It closes after `EventExited` has been delivered.

`Done()` closes when the harness process ends; that is not the same as delivery finishing, which is
when `Events()` closes. `Close(ctx)` stops the harness and waits, until `ctx` ends, for every event to
be delivered; when `ctx` ends first it drops the rest and returns `ErrUndelivered`.

A turn's terminal event can be delivered before `Send` returns, so do not move a turn's state back
when `Send`'s return arrives after it. After `EventExited`, `Send` returns `ErrExited`.

### State

```go
func (c *Conversation) State() State
```

One call reads the live conversation: whether the process is alive, its pid, the turn in flight, the
pending interactive prompt, whether the screen shows the harness working, when it last wrote, the
wrapper's latest classification and when it was made, the harness session id, how the process ended,
the account's usage limit as last reported (`RateLimit`, stream-json only), and the event queue's
pressure (`Delivery`: queued events and bytes, the longest a producer has
waited for room, how long the `OnEvent` call now running has taken, and the counts delivered, dropped
and oversized).

## Interactive input (blocking prompts)

Some harnesses block at startup on a dialog the normal `Send` flow cannot satisfy — Claude Code's
folder-trust prompt, the `--dangerously-skip-permissions` acceptance screen, Codex's update/model
notices. The per-harness adapter detects these on the rendered screen and the `Conversation` surfaces
them as a request/answer channel. The client answers **semantically** (an option ID or alias); the
chat layer owns the keystrokes (see [ADR-002](../internal/decisions/adr-002-interactive-input.md)).

```go
type InputRequest struct {
	ID          string        // stable per prompt; correlates the answer
	Kind        string        // e.g. "trust_prompt", "bypass_acceptance", "auto_mode_nudge", "update_menu", "model_migration", "question"
	Prompt      string        // the question text
	Header      string        // a short label for the prompt, e.g. a question's tab
	MultiSelect bool          // more than one option may be chosen
	Options     []InputOption // menu choices (ID, Alias, Label, Description); nil for free text
}
type InputAnswer struct {
	OptionID  string   // an option's ID, Alias or Label
	OptionIDs []string // a MultiSelect request's options
	Text      string   // free text: a text prompt's answer, or a question's "other" answer
}

func (c *Conversation) Answer(ctx context.Context, requestID string, ans InputAnswer) error
```

A detected prompt is resolved in this order:

1. **`Options.InputPolicy`** — declarative, JSON-serializable, set at open time. Auto-answers without
   a client:
   ```go
   InputPolicy{ByKind: map[string]Disposition{
       "trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
   }}
   ```
   claude-code's folder-trust dialog (`trust_prompt`) and its
   `--dangerously-skip-permissions` acceptance screen (`bypass_acceptance`) are **separate kinds**,
   so each needs its own entry — which is what makes "trust this folder, but refuse a
   skip-all-permissions launch" expressible:
   ```go
   InputPolicy{ByKind: map[string]Disposition{
       "trust_prompt":      {Kind: DispositionAnswer, OptionID: "proceed"},
       "bypass_acceptance": {Kind: DispositionDeny},
   }}
   ```
   `Disposition.Kind` is `DispositionAsk` (default) | `DispositionAnswer` | `DispositionDeny`.
2. **`Options.OnInputRequest`** — an in-process callback (Go only), consulted when the policy says
   `ask`.
3. **Surface to the client** — an `EventInputRequest` is emitted; answer it with `Answer` (requires
   the control token). When it clears, `EventInputResolved` fires.

`trust_prompt` and `bypass_acceptance` answer aliases are both `proceed` and `deny`, so a policy
need not know the exact wording.

claude 2.1.296 adds a third startup dialog, `auto_mode_nudge`: a one-time offer to make auto mode
the default permission mode, shown when the user settings name a `defaultMode`. Its `proceed`
answer ("Yes, set auto mode as my default permission mode", highlighted) rewrites the settings'
`defaultMode` to `auto`; `deny` ("No, keep …") leaves them alone. Answering either marks it seen
(`hasSeenAutoDefaultNudge` in `.claude.json`), so it does not come back. The configs hw writes set
that key, and the unattended policy (`oneshot.UnattendedInputPolicy`) answers `deny`. In a
config without the key, the nudge paints about 100 ms after the composer, so a prompt sent in that
window is lost to it: the turn fails rather than going through. That happens once per config.
While a prompt awaits an external answer `Send` returns `ErrInputPending`; while a policy/handler is
auto-answering, `Send` waits for the prompt to clear.

### Clarifying questions (claude-code)

When the model asks with its AskUserQuestion tool mid-turn, Claude Code's dialog arrives as a
request of kind **`question`**: `Prompt` is the question, `Header` its tab label, and each option
carries its `Description`. Two options are Claude Code's own: alias **`other`** ("Type something"),
answered with text, and alias **`chat`** ("Chat about this"), which declines the question; claude
then goes on to ask what you want to clarify. A dialog with several questions raises one request per
question, and a multi-question or multi-select one ends on a **`question_review`** request, the
Submit/Cancel pane, whose options alias `proceed` and `deny`. The turn stays in flight throughout,
and completes with claude's reply once the last answer lands.

```go
conv.Answer(ctx, req.ID, chat.InputAnswer{OptionID: "Blue"})                 // one option, by ID, alias or label
conv.Answer(ctx, req.ID, chat.InputAnswer{Text: "Green"})                    // text: the "other" option
conv.Answer(ctx, req.ID, chat.InputAnswer{OptionIDs: []string{"1", "3"}})    // a MultiSelect question
conv.Answer(ctx, req.ID, chat.InputAnswer{OptionIDs: []string{"1", "other"}, Text: "Green"})
conv.Answer(ctx, review.ID, chat.InputAnswer{OptionID: "proceed"})           // the review pane: submit
```

Text goes into a one-line field, so newlines and runs of spaces become single spaces. An answer the
dialog cannot take — text without the `other` option, `other` without text, `chat` with another
option — is refused before anything is written, with an error wrapping `turns.ErrInvalidAnswer`.

The chat layer writes an answer a key at a time, each once the screen shows the one before landed,
and only into the pane the answer was planned for; the first waits until the pane has been up for
300 ms, since Claude Code 2.1.283 drops a key written as its pane first paints. An answer that does
not land within the render budget fails with `ErrInputUnresolved`; no key is sent twice. A policy
answers questions the same way, e.g. `"question_review": {Kind: DispositionAnswer, OptionID:
"proceed"}` submits once a client has answered every question. The dialog is read by
pattern-matching, verified against Claude Code 2.1.283 and 2.1.296
([ADR-025](../internal/decisions/adr-025-planned-answers.md)); the stream-json transport does not
offer the tool at all, so there the model asks in plain text.

## History

```go
func (c *Conversation) History(ctx context.Context) ([]Turn, error)
func (c *Conversation) HistoryWithSource(ctx context.Context) ([]Turn, HistorySource, error)

type HistorySource string // HistorySourceTranscript ("transcript") | HistorySourceStore ("store")
```

`HistoryWithSource` tells you **which** source answered, which is the difference between "the model
said little" and "we lost the transcript and fell back to the screen". Turns projected from a
transcript carry no chat turn ID, and their start and completion timestamps are both the recorded
message time.

When the adapter implements `turns.TranscriptReader` **and** the harness session ID is known,
`History` reads the harness's own persisted JSONL log — the higher-fidelity source, since it records
exactly what the model said, not what the TUI rendered. See [Transcripts](../internal/transcript.md)
for the on-disk paths. Otherwise — and while the harness has not written its transcript yet — it
falls back to the `Store`'s recorded turns.

Harness session IDs are **assigned at launch** where the harness takes one. claude-code
implements `turns.SessionAssigner`, so every fresh `Open` starts them with `--session-id <uuid>` —
`Options.HarnessSessionID`, or a minted UUID when that is empty — and the stored `Session` carries the
id before the first turn. Everything that reads the harness's own record (`History`, the API-error
verdicts, the swallowed-prompt check) therefore works from turn 1. `Open` refuses an
`Options.HarnessSessionID` the adapter cannot take, one not in the harness's form, one alongside
`Resume`, and one whose transcript already exists (`ErrHarnessSessionInUse`, wrapped in
`ErrInvalidOptions` — resume that session instead). Whenever chat assigns or resumes an id,
`Options.Args` may not carry the adapter's session-control flags (`--session-id`, `--resume`,
`--continue`, …).

Other harnesses' IDs are extracted opportunistically: after each `TurnComplete`, the `Conversation`
invokes the adapter's `SessionIDExtractor` (if any) on the current screen, then its
`SessionIDLocator` (on-disk state), persists the ID via `Store.UpdateSession`, and stops re-querying.
An adapter that surfaces the id only as the TUI tears down implements `turns.RawSessionIDExtractor`;
`Open` taps the wrapper's durable line stream for it while the id is still unknown. The first id
recorded wins: no later capture replaces it, in memory or in the Store.

The locator is keyed on the harness's working directory and on when `Open` launched it, because the
directory holds every session ever run there. Codex's names the one rollout whose session started
since the launch, and none while there is no such rollout or more than one — two Codex
conversations started in one directory cannot be told apart on disk, and guessing would give one
the other's transcript. Such a conversation keeps no harness session id; its `History` comes from
the Store.

A Store that refuses the id does not lose it: the conversation writes it again at each turn's end
and when the harness exits. Until the write succeeds, `State().HarnessSessionIDErr` is
`ErrHarnessSessionIDNotSaved` wrapping the Store's error, and if the id never reaches the Store,
`EventExited` carries that error — the stored record cannot be resumed with `Reopen`.

## Graceful quit

```go
func (c *Conversation) Quit(ctx context.Context) error
```

`Quit` sends the adapter's graceful-quit sequence (claude-code: the `/quit` command) through the
writer the conversation already holds, so the harness exits cleanly and flushes its transcript. With
the session id known from launch, a `History` read *after* the process has exited still returns
transcript-backed history — which is how the one-shot [`run`](cli.md) / [`POST /v1/turns`](gateway.md)
paths end a turn yet still hand back the harness session id. Returns `ErrQuitUnsupported` when the adapter implements no
`turns.Quitter`.

## Permission mode at runtime

```go
func (c *Conversation) PermissionMode() (string, bool)
func (c *Conversation) SetPermissionMode(ctx context.Context, target string) (observed string, err error)
```

`PermissionMode` reads the harness's current posture off the rendered screen — no control token, no
readiness wait, valid mid-turn. `SetPermissionMode` drives the harness to a different one and
**returns the final observed posture on every path**, success or failure.

Both are covered in full — the rung vocabulary, the gates, the cycle-and-check discipline, the
blocked-by-modal recovery idiom, and what the resulting errors mean — in
[Permissions & sandboxing](permissions.md#switching-on-a-live-session). Two things to carry away here:

- the observed mode is **process-local and never persisted**, so `Reopen` starts from the launch rung
  again;
- `bypass` is reachable only if the session was *launched* bypass-enabled — you cannot cycle up to
  unrestricted.

## Model discovery

```go
func DiscoverModels(ctx context.Context, opts DiscoverModelsOptions) ([]models.Info, error)
```

Opens a throwaway conversation, types `/model`, and parses the picker — the only way to enumerate a
harness's models, since neither claude-code nor codex ships a machine-readable list. It selects
nothing and discards the session. Fails fast with `ErrPickerUnsupported` for a harness that has no
picker, and `ErrPickerTimeout` if the picker never renders. See [Discovery](../internal/discovery.md).

For an offline answer that costs no process launch, use `pkg/discovery/models` directly.

## Store interface

```go
type Store interface {
	CreateSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, id string) (*Session, error)
	UpdateSession(ctx context.Context, s *Session) error

	AppendTurn(ctx context.Context, t *Turn) error
	UpdateTurn(ctx context.Context, t *Turn) error
	ListTurns(ctx context.Context, sessionID string) ([]Turn, error)
}
```

`Store` holds **metadata only** — session ↔ harness-session mapping, turn IDs, state transitions,
timestamps. It does not duplicate transcript bodies. The shipped `pkg/chat/memstore` (`memstore.New()`)
suits testing, single-process gateways, and prototypes; for durability plug in your own (SQLite,
Postgres, …).

## Escape hatches

```go
func (c *Conversation) Resize(cols, rows uint16) error // resize the PTY and private screen together
func (c *Conversation) Wrapper() *wrapper.Session  // AttachOutput, RecentOutput, WriteStdin
func (c *Conversation) SessionID() string          // harness session id, once extracted
func (c *Conversation) Adapter() turns.Adapter     // the resolved per-harness adapter
func (c *Conversation) ScreenSnapshot() screen.Snapshot // current rendered screen
```

Always use `Conversation.Resize` for terminal size changes. Calling
`Conversation.Wrapper().Resize` directly updates only the PTY and leaves the
private screen emulator at the old dimensions. `Wrapper()` also bypasses the
control-token guard — use it with care.

## Sentinel errors

| Error | Returned by |
|---|---|
| `ErrInvalidOptions` | `Open`: required option missing or invalid, or a `Resume` that collides with a reserved `turns.SessionControlFlags` argument. Also wraps `wrapper.ErrInvalidConfig` for an invalid `Effort` — an unknown rung, or an effort on a harness with no effort axis — or an invalid `PermissionMode` — an unknown rung, a rung on a harness with no permission axis, a rung the target harness rejects, or a non-bypass rung contradicted by a bypass-enabling flag already in `Args` — so a bad option never surfaces as an internal error. `errors.Is` still matches `wrapper.ErrInvalidConfig` through the wrap |
| `ErrUnknownHarness` | `Open`: `Options.Harness` not registered |
| `ErrResumeUnsupported` | `Open` / `Reopen`: `Options.Resume` was set but the adapter does not implement `turns.SessionResumer` |
| `ErrNoControl` | `Send` / `Answer`: control token not held |
| `ErrTurnInFlight` | `Send`: previous assistant turn still pending |
| `ErrInputPending` | `Send`: a prompt is awaiting an external answer |
| `ErrHarnessBusy` | `Send` (and the other composer writes): `ctx` ended while the harness was still working; nothing typed |
| `ErrComposerNotCleared` | `Send`: the composer held text that would not clear; nothing typed, no turn recorded. `Interrupt`, beside `InterruptCancelled`: the prompt the harness put back would not clear |
| `ErrInterruptUnsupported` | `Interrupt`: the adapter cannot interrupt a turn (does not implement `turns.Interrupter`) |
| `ErrInterruptUnconfirmed` | `Interrupt`: `ctx` ended before the harness acknowledged; the turn stays in flight |
| `ErrExited` | `Send`: the harness process has ended (`EventExited`) |
| `ErrUndelivered` | `Close`: `ctx` ended before every event was delivered; the rest were dropped |
| `ErrEventTooLarge` | on an event, as `Err`: it exceeded the delivery queue's byte bound and arrives without its text |
| `ErrNoInputPending` | `Answer`: no prompt currently pending |
| `ErrStaleInputRequest` | `Answer`: request ID no longer current |
| `ErrUnknownOption` | `Answer`: option ID/alias matches no option |
| `ErrQuitUnsupported` | `Quit`: the adapter exposes no graceful-quit sequence |
| `ErrNotMultiSelect` | `Answer`: multiple option IDs given for a single-select request |
| `ErrConflictingAnswer` | `Answer`: single and multiple option IDs both set |
| `ErrAuthRequired` | `Send` / `DiscoverModels`: the harness is on a login or onboarding wall |
| `ErrNoHarnessSession` | `Reopen`: the stored session carries no harness session id |
| `ErrPickerUnsupported`, `ErrPickerTimeout` | `DiscoverModels`: no `/model` picker, or it never rendered |
| `ErrPermissionModeUnsupported`, `ErrPermissionModeUnreachable`, `ErrPermissionModeSwitchFailed`, `ErrPermissionModeIndeterminate`, `ErrPermissionModeBlockedByInput`, `ErrCodexPlanRefusedBusy` | `SetPermissionMode` — see [Permissions](permissions.md#if-it-doesnt-take) |
| `ErrClosed` | any method after `Close` |

All `Conversation` methods are safe for concurrent use; `Close` is idempotent. Discriminate with
`errors.Is`.
