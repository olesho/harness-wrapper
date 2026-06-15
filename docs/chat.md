# pkg/chat — Conversation API

`pkg/chat` is the Go-level chat-conversation API built on top of
`pkg/wrapper`. A `Conversation` owns one PTY-supervised harness
process (Codex, Claude Code, …) and exposes a small interface:
acquire exclusive control, send a user message, observe turn-level
state transitions, read history.

The package is the substrate that transport layers (HTTP, gRPC, …)
import. Transport concerns — framing, streaming protocol, auth — are
not part of this package and live in separate `cmd/` binaries.

## Lifecycle at a glance

```go
ctx := context.Background()
conv, err := chat.Open(ctx, chat.Options{
    Harness:    "codex",                  // or "claude-code", "gemini", "generic"
    BinaryPath: "/usr/local/bin/codex",
    WorkingDir: "/path/to/project",
    Store:      memstore.New(),
})
if err != nil { return err }
defer conv.Close(ctx)

release, err := conv.AcquireControl(ctx)   // FIFO queue, ctx-cancellable
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
    Harness     string   // "codex" | "claude-code" | "gemini" | "generic"  required
    BinaryPath  string   // harness executable                       required
    Args        []string // passed verbatim to the harness
    WorkingDir  string
    Env         []string
    Cols, Rows  int      // default 120×40
    Store       Store    // required; use memstore.New() for the in-process default
    EventBuffer int      // default 32; Events() channel size
}

func Open(ctx context.Context, opts Options) (*Conversation, error)
```

`Open`:
1. Resolves the per-harness `turns.Adapter` from `opts.Harness`.
2. Creates a `screen.Screen` of the configured size.
3. Starts a `wrapper.Session` with `Stdout` pointed at the screen.
4. Claims the wrapper-level writer lock for the conversation's lifetime
   (so no other code path interleaves keystrokes).
5. Spawns a `turns.Watcher` and a goroutine that maps watcher events
   onto Conversation turn state transitions.

Returns `ErrInvalidOptions` if required fields are missing,
`ErrUnknownHarness` if `Options.Harness` doesn't match a registered
adapter, or a wrapped `wrapper` / `store` error for downstream
failures.

## Control acquisition

```go
release, err := conv.AcquireControl(ctx)
```

`AcquireControl` is a FIFO mutex. The first caller gets the token
immediately; subsequent callers queue and are served in order. If
`ctx` cancels before the caller is served, `AcquireControl` returns
`ctx.Err()` and the waiter is removed from the queue.

`Send` returns `ErrNoControl` if no caller currently holds the token.

The wrapper-level writer lock is held continuously by `Conversation`
from `Open` until `Close`; `AcquireControl` is the chat-level token
that coordinates between multiple chat clients sharing one
`Conversation`.

## Sending messages

```go
func (c *Conversation) Send(ctx context.Context, text string) (turnID string, err error)
```

`Send`:
1. Verifies the control token is held (else `ErrNoControl`).
2. Verifies no prior assistant turn is in flight (else `ErrTurnInFlight`).
3. Records a `RoleUser` turn (immediately `TurnStateComplete`) and a
   `RoleAssistant` turn (`TurnStatePending`) in the `Store`.
4. Writes `text + "\r"` to the harness's PTY via
   `wrapper.Session.WriteStdin`.
5. Sets the "current assistant turn" pointer; the watcher loop
   completes it when the adapter reports `TurnComplete` (or
   `Errored` / `Blocked`).

Both Codex and Claude Code accept `\r` as the submit keystroke.
Senders that need richer input (multi-line, control characters) can
reach past the API via `conv.Wrapper().WriteStdin(...)`.

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
    Input *InputRequest // interactive prompt (EventInputRequest/Resolved)
    Err   error         // non-nil only for chat-level errors (e.g. Store failures)
}

func (c *Conversation) Events() <-chan ConversationEvent
```

`EventTurn` events fire on:
- Initial recording of a user turn (`TurnStateComplete`).
- Initial recording of an assistant turn (`TurnStatePending`).
- Adapter-driven completion / error (`TurnStateComplete` /
  `TurnStateErrored`).
- Write failures during `Send`.

`EventInputRequest` / `EventInputResolved` events drive the interactive-input
channel (see below). Switch on `Type`; for back-compat, turn-only consumers
may read `Turn` directly (it is the zero `Turn` for input events).

The channel is closed after `Close()` and the watcher has drained.

If the buffer fills (`EventBuffer`, default 32), additional events
are dropped rather than blocking the watcher pump — slow consumers
lose events. Consumers that need every event should drain promptly
or size the buffer for their workload.

## Interactive input (blocking prompts)

Some harnesses block at startup on a dialog the normal `Send` flow cannot
satisfy — Claude Code's folder-trust prompt ("Do you trust the files in this
folder?") and the `--dangerously-skip-permissions` acceptance screen. The
per-harness `turns.Adapter` detects these on the rendered screen and the
`Conversation` surfaces them as a request/answer channel. The client answers
**semantically** (an option ID or alias); the chat layer owns the keystrokes.

```go
type InputRequest struct {
    ID      string        // stable per prompt; correlates the answer
    Kind    string        // "trust_prompt" | "menu_select" | "confirm" | "text_input"
    Prompt  string        // the question text
    Options []InputOption // menu choices (ID, Alias, Label); nil for free text
}
type InputAnswer struct { OptionID string; Text string }

func (c *Conversation) Answer(ctx context.Context, requestID string, ans InputAnswer) error
```

A detected prompt is resolved in this order:

1. **`Options.InputPolicy`** — a declarative, JSON-serializable pre-configuration
   set at open time. `{ByKind: {"trust_prompt": {Kind: "answer", OptionID: "proceed"}}}`
   auto-answers without a client. `Kind` is `ask` (default) | `answer` | `deny`.
2. **`Options.OnInputRequest`** — an in-process callback (Go only) consulted when
   the policy says `ask`.
3. **Surface to the client** — an `EventInputRequest` is emitted on `Events()`;
   answer it with `Answer` (requires the control token, like `Send`). When it
   clears, an `EventInputResolved` fires.

`trust_prompt` answer aliases are `proceed` and `deny` (so a policy need not
know the concrete wording). While a prompt is awaiting an external answer,
`Send` returns `ErrInputPending` rather than blocking; while a policy/handler
is auto-answering, `Send` waits for the prompt to clear.

## History

```go
func (c *Conversation) History(ctx context.Context) ([]Turn, error)
```

When the adapter implements `turns.TranscriptReader` **and** the
harness's own session ID is known, `History` reads the harness's
persisted JSONL log (Codex: `~/.codex/sessions/`, Claude Code:
`~/.claude/projects/<encoded-cwd>/`, Gemini:
`~/.gemini/tmp/<project>/chats/`) and returns its parsed contents.
This is the higher-fidelity source — the harness records exactly what
the model said, not what the TUI rendered.

When transcript reading isn't possible (adapter has no reader, or the
harness session ID hasn't been extracted yet), `History` falls back to
the `Store`'s recorded turns.

Harness session IDs are extracted opportunistically: after each
`TurnComplete` event, `Conversation` invokes the adapter's
`SessionIDExtractor` (if implemented) on the current screen. Once
detected, the ID is persisted via `Store.UpdateSession` and not
queried again.

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

`Store` holds **metadata only**: session ↔ harness-session mapping,
turn IDs, state transitions, timestamps. It does **not** duplicate
transcript bodies — harnesses persist their own conversation logs and
`pkg/transcript` reads them on demand.

The shipped `pkg/chat/memstore` is suitable for testing,
single-process gateways, and prototypes. Production deployments that
need durability should plug in an alternate implementation
(e.g. SQLite, Postgres).

## Concurrency contract

All `Conversation` methods are safe for concurrent use. Specifically:
- `AcquireControl` queues callers; the FIFO order is preserved across
  goroutines.
- `Send` requires the control token to be held by *some* caller; chat
  does not enforce goroutine-local ownership.
- `Events()` returns the same channel on every call; multiple
  consumers competing for the same channel is supported but unfair.
- `Close` is idempotent.

## Reaching past the API

```go
func (c *Conversation) Wrapper() *wrapper.Session
func (c *Conversation) SessionID() string
```

`Wrapper()` returns the underlying `wrapper.Session` for callers that
need to `Resize`, `AttachOutput`, read `RecentOutput`, or inspect
wrapper-level state. Use with care — writing directly to stdin
bypasses the control-token guard.

## Sentinel errors

| Error                  | Returned by                                      |
|------------------------|--------------------------------------------------|
| `ErrInvalidOptions`    | `Open`: required option missing or invalid       |
| `ErrUnknownHarness`    | `Open`: `Options.Harness` not registered         |
| `ErrNoControl`         | `Send`/`Answer`: control token not currently held |
| `ErrTurnInFlight`      | `Send`: previous assistant turn still pending    |
| `ErrInputPending`      | `Send`: a prompt is awaiting an external answer  |
| `ErrNoInputPending`    | `Answer`: no prompt currently pending            |
| `ErrStaleInputRequest` | `Answer`: request ID no longer current           |
| `ErrUnknownOption`     | `Answer`: option ID/alias matches no option      |
| `ErrClosed`            | Any method after `Close` (or before re-use)      |

Use `errors.Is` to discriminate.
