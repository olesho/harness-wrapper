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
the [interactive-input channel](../guide/chat.md#interactive-input-blocking-prompts). An interrupt is
not an event: chat reads it per turn through the `Interrupter` capability and ends the turn
`TurnStateInterrupted` ([ADR-007](decisions/adr-007-interrupt.md)).

![Turn lifecycle](../diagrams/turn-lifecycle.svg)

## Capability interfaces

An adapter opts into extra behavior by implementing any of these (the chat/wrapper layers
feature-detect with a type assertion):

| Interface | Method | Purpose |
|---|---|---|
| `SessionIDExtractor` | `ExtractSessionID(snap) (string, bool)` | Scrape the harness's resume UUID from the rendered screen (e.g. `codex resume <uuid>`). |
| `RawSessionIDExtractor` | `ExtractSessionIDFromLine(line) (string, bool)` | Recover the UUID from a raw PTY line — for hints that flash by as the TUI tears down on exit and never reach a rendered snapshot (claude-code prints `claude --resume <uuid>` on `/quit`). Consulted only while the id is unknown. |
| `SessionAssigner` | `NewSessionID() string`, `ValidSessionID(id) error`, `SessionIDArgs(id) []string` | Start a FRESH session under an id chosen before launch (claude-code, pi: `--session-id <uuid>`). `chat.Open` assigns one on every fresh open — `Options.HarnessSessionID` or a minted one — so the id is known from the first turn instead of learned from an exit hint. |
| `TranscriptReader` | `ReadTranscript(harnessSessionID, workingDir) ([]transcript.Turn, error)` | Locate + parse the harness's own JSONL log. |
| `Quitter` | `QuitSequence() []byte` | Bytes for a graceful exit (claude-code: the `/quit` command + enhanced Enter). |
| `MessageExtractor` | `ExtractMessage(snap) (string, bool)` | Isolate the assistant reply from TUI chrome. |
| `BusyDetector` | `Busy(snap) bool` | Distinguish "still working" from "idle at the prompt". |
| `Interrupter` | `InterruptSequence() []byte`, `InterruptOutcome(prompt, snap) (turns.InterruptOutcome, string)`, `ComposerText(snap) (string, bool)`, `ClearComposerSequence(composer) []byte` | Interrupt a turn and read what the harness did with it — `InterruptPending`, `InterruptStopped` (with the partial reply), `InterruptCancelled` (the prompt put back in the composer) or `InterruptFinished` — per turn: only below this turn's prompt echo, so an earlier turn's marker never speaks for it. `ComposerText` and `ClearComposerSequence` let `Send` type into an empty composer only. `chat.Conversation.Interrupt` drives it ([ADR-007](decisions/adr-007-interrupt.md)); claude-code only — codex returns `ErrInterruptUnsupported` until its interrupt is captured. |
| `PermissionModeDetector` | `PermissionMode(snap) (string, bool)` | Report the harness's posture on its **primary** permission axis, read off the rendered screen. The two implementations do **not** report the same kind of value: claude-code returns a canonical rung from `wrapper.PermissionRungs()`; codex returns a **COLLABORATION-axis** value (`"plan"` or `"default"`) which is *not* a rung — codex's permissions rung lives on a second axis this interface deliberately does not model. `false` means the screen carries **no readable signal** (onboarding wall, modal over the footer), never "readable, and not plan". Deliberately absent on opencode, pi and generic. |
| `PermissionPostureDetector` | `PermissionPosture(snap) (turns.PermissionPosture, bool)` | Report the FULL posture — canonical `Rung`, the harness's own `Native` spelling of it, and `OnRing` (can the harness's cycle key produce that spelling?). Exists because several natives share one rung: claude paints both `⏸ manual mode on` and `⏵⏵ don't ask on` for `manual`, and only the first is reachable by Shift+Tab, so a driver comparing rungs alone reads a `dontAsk` session as already-manual and writes no keystroke. `Rung` carries `PermissionModeDetector`'s contract exactly; `Native` is DIAGNOSTIC and must never be compared against `wrapper.PermissionRungs()`. `false` means no readable signal, same as `PermissionModeDetector`. Implemented by **claude-code only** — codex has no alias collision on its collaboration axis, so `pkg/chat` reads it through a rung-only fallback that is byte-identical to the old behaviour. |
| `SessionResumer` | `ResumeArgs(harnessSessionID) []string` | The argv fragment that resumes an existing harness session (e.g. `{"--resume", id}`). `chat.Open` returns `ErrResumeUnsupported` when `Options.Resume` is set and the adapter omits this. |
| `SessionControlFlags` | `SessionControlFlags() []string` | The session-control flags chat reserves (e.g. `--resume`, `--fork-session`) and callers must not pass in `Options.Args`; a collision is `ErrInvalidOptions`. An adapter that omits it declares no reserved flags. |

`Busy()` is what keeps the chat layer from reporting `complete` mid-turn; only claude-code implements
it today (its replies stream in multiple parts). See the [adapter matrix](../guide/adapters.md) for
which harness implements what.

### Interactive input

`turns.InputRequest` carries the server-side keystrokes the client never sees:

```go
type InputRequest struct {
	ID      string        // stable across redraws of the same prompt
	Kind    string        // "trust_prompt" | "bypass_acceptance" | "menu_select" | "confirm" | "text_input"
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

The Watcher has **no ticker, no polling and no debounce of its own** — it is purely edge-driven. Two
things stand in for those:

- **coalescing** happens in [`screen`](screen.md): subscriber channels are buffered 1 and sends are
  non-blocking, so a burst of writes collapses into a single wake-up;
- **timing** — the confirm and idle windows that decide *when a turn is done* — lives one layer up, in
  [`pkg/chat`](../guide/chat.md#how-a-turn-completes). Adapters report what they see; chat decides when
  that is enough.

Because both pumps call the adapter from independent goroutines, per-adapter state (the last
fingerprint, the last input request) must be mutex-guarded — which is why the contract demands
concurrency safety rather than merely suggesting it.

`scr.Subscribe()` is called **synchronously inside `Watch`**, before the pump goroutine starts, so no
snapshot can be missed in the gap.

The Watcher backfills `Event.At` when the adapter leaves it zero and enriches events with `HTTPCode` /
`RetryAfter` from the originating `SessionEvent`. `Events()` closes after both sources stop **and**
`Close()` is called; `Close` stops the screen pump but does **not** stop the `wrapper.Session` — the
caller owns `sess.Stop`.

## What each adapter keys on

The signals themselves are the part most likely to break on an upstream release, so they live in one
place per adapter and are pinned by [corpus replay](testing/corpus.md). The shapes, as of the
[current pins](versions-drift.md):

| Adapter | Turn complete | Busy | Interrupt | Session id | Blocking prompts |
|---|---|---|---|---|---|
| `claudecode` | a thinking-summary line ending the turn, **only when not busy** | its live status region only: the footer's "esc to interrupt", and the status line's spinner or retry countdown ("✻ API error · Retrying in 1s") above the composer box | Esc as `CSI 27 u`; "⎿  Interrupted · What should Claude do instead?" below this turn's prompt echo (stopped), or the prompt back in the composer (cancelled) | **assigned at launch** (`--session-id <uuid>`); the `--resume <uuid>` exit hint on the raw line stream only when an id was not assigned | folder trust, the alternate trust wording, and the bypass-permissions acceptance screen — all one kind |
| `codex` | a fresh end-of-turn footer, deduped by exact text | — (no busy model) | — | scraped from the resume hint, plus an on-disk lookup of the latest session for the working directory | startup interstitials (update, model migration, generic notice) and **approval dialogs** |
| `pi` | — (idle fallback) | a "Working…" / "Thinking…" spinner | — | **assigned at launch** (`--session-id <uuid>`) | — |
| `opencode` | — (idle fallback) | — | — | — | — |
| `generic` | wrapper status only | — | — | — | — |

Two deliberate asymmetries:

- **codex emits a balancing `InputResolved` before a replacing `InputRequested`** when one interstitial
  supersedes another, so a client's "current prompt" never silently changes identity. claude-code does
  not, because its dialogs do not chain that way.
- **codex's approval detection is mandatory-strict**: an anchor alone is not enough — it also requires
  a proceed row, a deny row, and a live selector parsed from the text *after* the anchor. A false
  positive here would deadlock a turn, so the recogniser is biased hard toward missing rather than
  inventing one.

A known gap on claude-code: its **per-tool permission dialog is not detected at all**. That is what
makes restrictive rungs stall an unattended turn — see
[Permissions](../guide/permissions.md#what-is-actually-enforced).

## Adding a harness

The full per-adapter workflow — find the turn-complete marker, session-id surfacing, cost/quota
patterns, and transcript schema; implement adapter + classifier + reader; record
[corpus](testing/corpus.md) scenarios (canonical **and** adversarial); wire into
`chat.resolveAdapter` and `harness-chatd`; add a [`versions.json`](versions-drift.md) pin — is
sequenced as item 1 in the [Roadmap](roadmap-v1.md). opencode is next.
