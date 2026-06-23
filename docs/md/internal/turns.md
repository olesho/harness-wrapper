# Turns & Adapters

`pkg/turns` sits between the wrapper/screen and the chat layer. It defines the per-harness **Adapter**
contract, a set of optional **capability interfaces**, and a **Watcher** that composes a
`wrapper.Session` + `screen.Screen` + `Adapter` into one event stream.

## The Adapter contract

Every harness implements `Adapter`. It's deliberately tiny — two observers plus a name:

```go
type Adapter interface {
	Name() string                                              // "generic" | "codex" | "claude-code" | …
	OnScreen(snap screen.Snapshot) []Event                     // after every screen write
	OnWrapperStatus(status wrapper.Status, reason string) []Event // on every wrapper status event
}
```

Implementations must be safe for concurrent calls (stateless or mutex-guarded). The
[`generic`](../guide/adapters.md#generic) adapter implements `OnWrapperStatus` by mapping wrapper
status straight to events, and returns `nil` from `OnScreen`; most real adapters embed it for the
status half and add screen-marker detection on top.

## Turn events

```go
type Kind string
const (
	TurnComplete   Kind = "turn_complete"   // assistant finished; caller may send the next message
	ToolCall       Kind = "tool_call"       // invoking a tool (informational; turn ongoing)
	Blocked        Kind = "blocked"         // transient block (cost/quota/rate-limit) — back off
	Errored        Kind = "errored"         // terminal failure (non-zero exit, signal, fatal error)
	InputRequested Kind = "input_requested" // blocked on an interactive prompt
	InputResolved  Kind = "input_resolved"  // that prompt is gone
)

type Event struct {
	Kind       Kind
	At         time.Time         // backfilled by the Watcher if zero
	Reason     string
	Snap       *screen.Snapshot  // screen at event time (nil for wrapper-only events)
	HTTPCode   int               // Watcher-populated from the SessionEvent
	RetryAfter time.Duration
	Input      *InputRequest     // set for InputRequested / InputResolved
}
```

The chat layer maps these onto its [turn model](../guide/chat.md#turn-model): `TurnComplete` →
`TurnStateComplete`; `Errored`/`Blocked` → `TurnStateErrored`; `InputRequested`/`InputResolved` drive
the [interactive-input channel](../guide/chat.md#interactive-input-blocking-prompts).

![Turn lifecycle](../diagrams/turn-lifecycle.svg)

## Capability interfaces

An adapter opts into extra behavior by implementing any of these (the chat/wrapper layers
feature-detect with a type assertion):

| Interface | Method | Purpose |
|---|---|---|
| `SessionIDExtractor` | `ExtractSessionID(snap) (string, bool)` | Scrape the harness's resume UUID from the rendered screen (e.g. `codex resume <uuid>`). |
| `RawSessionIDExtractor` | `ExtractSessionIDFromLine(line) (string, bool)` | Recover the UUID from a raw PTY line — for hints that flash by as the TUI tears down on exit and never reach a rendered snapshot (claude-code prints `claude --resume <uuid>` on `/quit`). |
| `TranscriptReader` | `ReadTranscript(harnessSessionID, workingDir) ([]transcript.Turn, error)` | Locate + parse the harness's own JSONL log. |
| `Quitter` | `QuitSequence() []byte` | Bytes for a graceful exit (claude-code: the `/quit` command + enhanced Enter). |
| `MessageExtractor` | `ExtractMessage(snap) (string, bool)` | Isolate the assistant reply from TUI chrome. |
| `BusyDetector` | `Busy(snap) bool` | Distinguish "still working" from "idle at the prompt". |

`Busy()` is what keeps the chat layer from reporting `complete` mid-turn; only claude-code implements
it today (its replies stream in multiple parts). See the [adapter matrix](../guide/adapters.md) for
which harness implements what.

### Interactive input

`turns.InputRequest` carries the server-side keystrokes the client never sees:

```go
type InputRequest struct {
	ID      string        // stable across redraws of the same prompt
	Kind    string        // "trust_prompt" | "menu_select" | "confirm" | "text_input"
	Prompt  string
	Options []InputOption
}
type InputOption struct {
	ID, Alias, Label string
	Keys             []byte // bytes to write to choose it ("1\r"); SERVER-SIDE ONLY
}
```

The adapter parses the on-screen dialog into options (with `Keys` and a portable `Alias` like
`proceed`/`deny`), fingerprints it on `ID` so redraws collapse, and emits `InputRequested` /
`InputResolved`. The chat layer keeps `Keys` private and exposes only the semantic
[`chat.InputRequest`](../guide/chat.md#interactive-input-blocking-prompts). The design is recorded in
[ADR-002](decisions/adr-002-interactive-input.md).

## The Watcher

```go
func Watch(sess *wrapper.Session, scr *screen.Screen, adapter Adapter) *Watcher
func (w *Watcher) Events() <-chan Event
func (w *Watcher) Close() error
```

`Watch` runs two background pumps:

1. **status pump** — `sess.Events()` → `adapter.OnWrapperStatus(...)`.
2. **screen pump** — `scr.Subscribe()` → `adapter.OnScreen(...)` (pass `nil` for `scr` to skip).

The Watcher backfills `Event.At` when the adapter leaves it zero and enriches events with `HTTPCode` /
`RetryAfter` from the originating `SessionEvent`. `Events()` closes after both sources stop **and**
`Close()` is called; `Close` stops the screen pump but does **not** stop the `wrapper.Session` — the
caller owns `sess.Stop`.

## Adding a harness

The full per-adapter workflow — find the turn-complete marker, session-id surfacing, cost/quota
patterns, and transcript schema; implement adapter + classifier + reader; record
[corpus](testing/corpus.md) scenarios (canonical **and** adversarial); wire into
`chat.resolveAdapter` and `harness-chatd`; add a [`versions.json`](versions-drift.md) pin — is
sequenced as item 1 in the [Roadmap](roadmap-v1.md). opencode is next.
