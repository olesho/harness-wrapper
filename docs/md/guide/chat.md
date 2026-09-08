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
	Harness:    "codex", // "claude-code" | "opencode" | "pi" | "generic"
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
	Harness     string   // "codex" | "claude-code" | "opencode" | "pi" | "generic"  (required)
	BinaryPath  string   // harness executable                                                  (required)
	Args        []string // passed verbatim to the harness
	Resume      string   // harness session id to resume; Args must not carry any flag the
	                     // adapter reserves via turns.SessionControlFlags
	WorkingDir  string
	Env         []string
	Effort      string   // reasoning effort ("" = harness default)
	Model       string   // model for this run ("" = harness default)
	PermissionMode string // launch-time permission rung ("" = harness default)
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

### Reopen

```go
func Reopen(ctx context.Context, opts ReopenOptions) (*Conversation, error)
```

`Reopen` restarts a **stored** session: it loads the record from the `Store`, resumes the harness with
the recorded harness session id, and returns a fresh `Conversation`. `Harness`, `WorkingDir` and the
resume id come from the record; everything else (`BinaryPath`, `Args`, `Env`, the execution-mode knobs,
`Cols`/`Rows`, `InputPolicy`, …) you supply again.

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
queue. `Send` and `Answer` return `ErrNoControl` if no caller currently holds the token.

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

The prompt and its submit key go out in **one write**: no per-character typing, no inter-key delay, no
retry. The submit key is per-harness, because modern TUIs enable the enhanced keyboard protocol where a
bare carriage return only inserts a newline:

| Harness | Submit key |
|---|---|
| claude-code, codex | `CSI 13u` (`\x1b[13u`) |
| pi | `\r` |
| anything else | `\n` |

### Readiness: what `Send` waits for

For harnesses whose composer is detectable (claude-code, codex, pi), `Send` blocks until the screen
shows a ready prompt. This is what keeps a prompt from being typed into a boot screen or a modal and
silently lost. While waiting it can end in three other ways:

- `ErrInputPending` — a blocking prompt is waiting for **your** answer (a policy or callback that is
  answering one itself does not count; `Send` waits for it to clear).
- `ErrAuthRequired` — the harness is sitting on a login or onboarding wall. An onboarding wall
  short-circuits immediately; a softer "not logged in" banner must persist briefly before it counts,
  so a transient render cannot trip it. The prompt is deliberately **not** written — it would land in
  a sign-in menu. `Send` records an errored assistant turn carrying `ReasonAuthRequired` and returns
  its id with a nil error.
- `ctx.Err()` / `ErrClosed`.

There is **no internal send timeout**: your `ctx` is the only clock. Harnesses with no readiness
marker (opencode, generic) skip the gate entirely.

### Turn model

```go
type Role string      // RoleUser | RoleAssistant | RoleSystem
type TurnState string  // TurnStatePending | TurnStateStreaming | TurnStateComplete | TurnStateErrored

type Turn struct {
	ID, SessionID string
	Role          Role
	State         TurnState
	Text          string
	Reason        string
	StartedAt, CompletedAt time.Time
	HTTPCode      int            // upstream code when a turn errors on an API error
	RetryAfter    time.Duration  // wait hint parsed from the harness's error
}
```

There is exactly **one assistant turn per `Send`**; it ends in `TurnStateComplete` (the adapter saw
turn completion) or `TurnStateErrored` (the harness errored, was blocked, or exited).
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

Both windows are tuned per harness and are **not** part of the wire contract: treat them as
"eventually, quickly" rather than a guaranteed latency. They are overridable only from within the
package (tests), because a caller that needs a hard bound should use its own `ctx`.

## Events

```go
type EventType string
const (
	EventTurn          EventType = "turn"
	EventInputRequest  EventType = "input_request"
	EventInputResolved EventType = "input_resolved"
)

type ConversationEvent struct {
	Type  EventType     // which payload is set
	Turn  Turn          // affected turn (EventTurn; zero otherwise)
	Input *InputRequest // interactive prompt (EventInputRequest / EventInputResolved)
	Err   error         // non-nil only for chat-level errors (e.g. Store failures)
}

func (c *Conversation) Events() <-chan ConversationEvent
```

`EventTurn` fires on every turn-state change: the initial user turn, the initial assistant turn, and
the adapter-driven completion or error. Switch on `Type`; turn-only consumers can read `Turn`
directly (it is the zero `Turn` for input events).

The channel is closed after `Close()` drains. If the buffer (`EventBuffer`, default 32) fills, events
are **dropped** rather than blocking the watcher — slow consumers lose events, so drain promptly or
size the buffer for your workload.

## Interactive input (blocking prompts)

Some harnesses block at startup on a dialog the normal `Send` flow cannot satisfy — Claude Code's
folder-trust prompt, the `--dangerously-skip-permissions` acceptance screen, Codex's update/model
notices. The per-harness adapter detects these on the rendered screen and the `Conversation` surfaces
them as a request/answer channel. The client answers **semantically** (an option ID or alias); the
chat layer owns the keystrokes (see [ADR-002](../internal/decisions/adr-002-interactive-input.md)).

```go
type InputRequest struct {
	ID      string        // stable per prompt; correlates the answer
	Kind    string        // e.g. "trust_prompt", "bypass_acceptance", "update_menu", "model_migration"
	Prompt  string        // the question text
	Options []InputOption // menu choices (ID, Alias, Label); nil for free text
}
type InputAnswer struct { OptionID, Text string }

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
While a prompt awaits an external answer `Send` returns `ErrInputPending`; while a policy/handler is
auto-answering, `Send` waits for the prompt to clear.

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
for the on-disk paths. Otherwise it falls back to the `Store`'s recorded turns.

Harness session IDs are extracted opportunistically: after each `TurnComplete`, the `Conversation`
invokes the adapter's `SessionIDExtractor` (if any) on the current screen, persists the ID via
`Store.UpdateSession`, and stops re-querying. Adapters that surface the id only as the TUI tears down
(claude-code, on `/quit`) implement `turns.RawSessionIDExtractor` instead; `Open` taps the wrapper's
durable line stream and captures the id the moment the exit hint (`claude --resume <uuid>`) streams by.

## Graceful quit

```go
func (c *Conversation) Quit(ctx context.Context) error
```

`Quit` sends the adapter's graceful-quit sequence (claude-code: the `/quit` command) through the
writer the conversation already holds, so the harness exits cleanly. Combined with the raw session-id
capture above, a `History` read *after* the process has exited still returns transcript-backed
history — which is how the one-shot [`run`](cli.md) / [`POST /v1/turns`](gateway.md) paths end a turn
yet still hand back the harness session id. Returns `ErrQuitUnsupported` when the adapter implements no
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
