# ADR-002: interactive input requests (trust dialogs & blocking prompts)

**Status:** Accepted — implemented 2026-06-15 (trust detector; general channel)

## Context

A client drives Claude Code's interactive TUI under a PTY through Harness
Wrapper. In an untrusted worktree, Claude Code blocks at startup on a
folder-trust dialog ("Do you trust the files in this folder?") with a
numbered menu (`1. Yes, proceed` / `2. No, exit`). Nothing downstream can
make progress until that menu is answered, and today there is no way to
either learn about it or answer it through our interface.

The current flow **deadlocks silently**:

- `chat.Send` → `waitReadyForSend` (`pkg/chat/ready.go:8`) blocks until the
  screen shows `"Claude Code"` **and** `"❯"`. The trust dialog never reaches
  that state, so `Send` / `harness.RunTurn` hang until the context times out,
  with no signal explaining why.
- The existing `Prompt`-pattern → `StatusWaitingForInput`
  (`pkg/wrapper/harness_adapter.go:76`) is the wrong tool: it is gated on 15s
  of quiet, matches only the **trailing** screen line (the dialog's last line
  is `2. No, exit` / `Enter to confirm · Esc to exit`, not the question), and
  carries no structured choices.
- `--dangerously-skip-permissions` is **not** a reliable bypass: it replaces
  the folder-trust prompt with its own blocking "Bypass Permissions mode —
  1. Yes, I accept / 2. No, exit" acceptance screen on first run. The blocking
  dialog is a recurring *shape*, not a one-off — which is why the answer is an
  interaction channel, not a flag.

There are two distinct consumers to serve:

1. **Interactive / remote** (the `harness-chatd` client, or any in-process
   driver): wants to be told a prompt is blocking and answer it live.
2. **One-shot** (`harness.RunTurn`): synchronous; needs the prompt resolved by
   a pre-supplied policy or it will time out.

## Decision

Add an **out-of-band `InputRequest` / `Answer` channel** that rides alongside
the existing turn flow. Three principles:

1. **Semantic answers, keystrokes hidden.** The client answers `"Yes, proceed"`
   (an option id), never raw bytes. The wrapper owns the keystroke translation,
   exactly as `Send` already hides `submitKeyForHarness`. Per-harness keystroke
   knowledge stays server-side.
2. **Screen-based detection in the turns adapter.** The rendered
   `screen.Snapshot().Text` shows the question as clean text; the raw PTY
   stream is ANSI soup. Detection therefore lives in the per-harness
   `turns.Adapter`, which already has the snapshot and a fingerprint-dedup
   pattern (`pkg/turns/harness/claudecode/claudecode.go:81`).
3. **General channel, trust detector first.** The `InputRequest` abstraction is
   generic (`trust_prompt | menu_select | confirm | text_input`); v1 ships only
   the Claude folder-trust + bypass-acceptance detectors. Other detectors
   (onboarding/theme, login/text, per-tool permission menus) are follow-ups.

### Two resolution mechanisms

A detected request is resolved by, in order:

1. **Declarative `InputPolicy`, pre-configured at open time** (works in-process
   and remotely over `harness-chatd`, because it is JSON-serializable). The
   client pre-decides dispositions, e.g. "auto-answer `trust_prompt` →
   `proceed`". This is what makes one-shot `RunTurn` work unattended.
2. **Live hook** when the policy says `ask` (or does not match). Two equivalent
   forms:
   - *in-process:* an `OnInputRequest` callback supplied at open;
   - *remote:* the request is emitted on `Events()` / the SSE stream and the
     client answers via `Conversation.Answer` / `POST …/input`.

Default disposition is `ask` — the human/client stays in the loop unless they
opt into auto-answering.

### Layer 1 — `pkg/turns`: new event kinds carrying structured choices

```go
const (
    // ...existing: TurnComplete, ToolCall, Blocked, Errored...
    InputRequested Kind = "input_requested" // harness blocked on an interactive prompt
    InputResolved  Kind = "input_resolved"  // that prompt is gone (answered/dismissed)
)

// InputRequest describes a blocking prompt the normal Send flow can't satisfy.
type InputRequest struct {
    ID      string        // stable across redraws of the SAME prompt; new prompt → new ID
    Kind    string        // "trust_prompt" | "menu_select" | "confirm" | "text_input"
    Prompt  string        // the question text
    Options []InputOption // menu choices; nil for free-text
}

type InputOption struct {
    ID    string // stable id the answer returns ("yes_proceed"); detector-defined
    Alias string // portable intent for policy matching ("proceed" | "deny" | "yes" | "no")
    Label string // human label ("Yes, proceed")
    Keys  []byte // bytes to write to choose it (e.g. "1\r"); SERVER-SIDE ONLY
}

type Event struct { /* ...existing fields... */ Input *InputRequest } // set for the two new kinds
```

The claudecode adapter's `OnScreen` gains an **anchored** matcher (keyed on the
stable question line, parsing the numbered options into `Options` with
`Keys: []byte("<n>\r")` and `Alias` from a small per-dialog table). It emits
`InputRequested` once per distinct dialog (fingerprinted on `ID`, like
`lastFingerprint` today) and `InputResolved` when the dialog clears. The `ID` is
a hash of `Kind + Prompt + option labels`, so redraws collapse and a genuinely
new dialog gets a fresh id.

### Layer 2 — `pkg/chat.Conversation`: the canonical client interface

```go
// Client-facing mirror of turns.InputRequest — NO Keys field.
type InputRequest struct { ID, Kind, Prompt string; Options []InputOption }
type InputOption  struct { ID, Alias, Label string }
type InputAnswer  struct { OptionID string /* menu/confirm/trust */; Text string /* text_input */ }

// Typed event envelope (replaces the bare TurnEvent on the channel).
type EventType string
const (
    EventTurn          EventType = "turn"
    EventInputRequest  EventType = "input_request"
    EventInputResolved EventType = "input_resolved"
)
type ConversationEvent struct {
    Type  EventType
    Turn  *Turn          // set when Type == EventTurn
    Input *InputRequest  // set when Type == EventInputRequest / EventInputResolved
    Err   error
}
func (c *Conversation) Events() <-chan ConversationEvent

// Answer responds to a pending request. Requires the control token (same guard
// as Send). Errors: ErrNoControl, ErrNoInputPending, ErrStaleInputRequest,
// ErrUnknownOption.
func (c *Conversation) Answer(ctx context.Context, requestID string, ans InputAnswer) error
```

Open-time configuration (`chat.Options`):

```go
// Mechanism 1: declarative, JSON-serializable, set at open.
InputPolicy *InputPolicy
// Mechanism 2 (in-process form): live callback; consulted only when the policy
// resolves to "ask". Remote clients omit this and answer over the wire instead.
OnInputRequest func(InputRequest) (InputAnswer, bool)

type InputPolicy struct {
    Default DispositionKind            // when nothing matches; default "ask"
    ByKind  map[string]Disposition     // keyed by InputRequest.Kind
}
type Disposition struct {
    Kind     DispositionKind // "ask" | "answer" | "deny"
    OptionID string          // for "answer": option id or Alias (e.g. "proceed")
    Text     string          // for "answer" on text_input
}
type DispositionKind string // "ask" | "answer" | "deny"
```

Wiring:

- `handleTurnsEvent` (`conversation.go:214`) stores the pending request (keeping
  `Keys` server-side in a `currentInput *turns.InputRequest`) on
  `InputRequested`, runs the **resolution order** above, and — if unresolved —
  emits an `EventInputRequest` envelope. `InputResolved` clears `currentInput`
  and emits `EventInputResolved`. `currentInput` is authoritative: it is
  replaced on any new `InputRequested` and cleared only on `InputResolved`, so a
  wrong answer that re-prompts (new id) is handled naturally.
- `Answer` validates `requestID == currentInput.ID`, resolves option id/alias →
  `InputOption`, and `c.sess.WriteStdin(opt.Keys)`. The client never sees keys.
- `waitReadyForSend` gains an early `currentInput != nil → ErrInputPending`
  check, so a client that calls `Send` before answering gets a clear error
  instead of a silent hang.

### Layer 3 — `cmd/harness-chatd`: remoted over the existing HTTP+SSE shape

- **SSE** (`GET /v1/conversations/{id}/events`) adopts a **typed envelope** with
  back-compat: every frame gains a `type` field; turn frames stay
  `{"type":"turn","turn":{…}}` where the `turn` sub-object is byte-for-byte the
  old `turnEventDTO.Turn`, so existing consumers reading `.turn` keep working.
  New frames: `{"type":"input_request","input_request":{…}}` and
  `{"type":"input_resolved",…}`. The `input_request` DTO omits `Keys`.
- **New endpoint** `POST /v1/conversations/{id}/input` with
  `{token, request_id, option_id, text}` → `entry.conv.Answer(...)`, returning
  202 — exactly parallel to `/messages` (`server.go:259`) and reusing the
  control-token guard.
- **`POST /v1/conversations` (open)** accepts an optional `input_policy` object
  (mechanism 1). The remote client uses mechanism 2 by default (subscribe +
  `POST …/input`); the in-process `OnInputRequest` callback is not remoteable.
- **`harness.RunTurn`** gains `TurnConfig.InputPolicy`; a one-shot run in an
  untrusted dir is made unattended by `InputPolicy{ByKind:{"trust_prompt":
  {Kind:"answer", OptionID:"proceed"}}}`. With no policy, an unresolved request
  surfaces as a turn error rather than hanging to the deadline.

### End-to-end sequence (remote, live answer)

```
open conv (input_policy: trust=ask) → GET /events (SSE)
SSE ─▶ {"type":"input_request","input_request":{kind:"trust_prompt",
        options:[{id:"yes_proceed",alias:"proceed",label:"Yes, proceed"},
                 {id:"no_exit",alias:"deny",label:"No, exit"}]}}
POST /control → token
POST /input {token, request_id, option_id:"yes_proceed"}
   └▶ Conversation.Answer → WriteStdin("1\r")
SSE ─▶ {"type":"input_resolved", …}   (dialog cleared; screen now "Claude Code … ❯")
POST /messages …                       (normal turn flow proceeds)
```

Multi-step onboarding (trust → theme → bypass-accept) falls out for free: each
new dialog is a fresh `InputRequest` with a new id.

## Consequences

- The design reuses every existing extension point — adapter `OnScreen`,
  `currentTurn`-style pending state, the control queue, the SSE fanout,
  `WriteStdin` — and adds **no new transport**.
- `Conversation.Events()` changes element type from `TurnEvent` to the
  `ConversationEvent` envelope. This is an **internal** break: the only
  consumers are the `harness-chatd` fanout (`cmd/harness-chatd/sse.go`) and
  tests. External SSE **wire** compatibility is preserved by the additive
  `type` discriminator and the unchanged `turn` payload.
- Keystroke knowledge stays localized to the per-harness adapter (one table per
  dialog), so adding a harness or a dialog is a self-contained change.
- The wrapper's coarse `StatusWaitingForInput` (`harness_adapter.go:76`) is left
  unchanged for non-chat consumers; the structured channel supersedes it for
  the chat path. We accept this minor redundancy rather than threaten the
  wrapper's stable surface.
- A new failure mode: an unanswered request blocks the harness indefinitely.
  Mitigated by `InputPolicy` for unattended runs and (see open work) an optional
  `InputRequestTimeout`.

## Status of work

- [x] **turns:** `InputRequested`/`InputResolved` kinds, `InputRequest` /
      `InputOption`, and the `Event.Input` field. (`pkg/turns/turns.go`)
- [x] **claudecode adapter:** anchored folder-trust + bypass-acceptance
      detector with id-fingerprint dedup + `InputResolved` on clear; option
      `Keys`/`Alias` tables; numbered-menu parser tolerant of box borders.
      Unit-tested (`input_test.go`). (`pkg/turns/harness/claudecode/`)
- [x] **chat:** `ConversationEvent` envelope + `Events()` retype;
      `InputRequest`/`InputOption`/`InputAnswer`; `currentInput`/`inputSurfaced`
      state in `handleTurnsEvent`; `Answer`; the `ErrInputPending` guard in
      `waitReadyForSend` (+ `inputStateCh` wake signal so a blocked `Send` is
      woken between redraws); new sentinel errors. (`pkg/chat`)
- [x] **chat:** `InputPolicy` + `OnInputRequest` in `Options`; resolution order
      policy → handler → surface. `readyForInput` consults the detector so a
      dialog's own `❯` doesn't read as ready. (`pkg/chat`)
- [x] **harness-chatd:** typed SSE envelope (back-compat `type`, `turn` payload
      unchanged), `POST …/input`, `input_policy` on open + `/v1/turns`, DTOs
      without `Keys`. Integration-tested (`sse_input_test.go`, `trust` mock
      mode). (`cmd/harness-chatd`)
- [x] **harness.RunTurn:** `TurnConfig.InputPolicy` + `OnInputRequest`; an
      unresolved prompt surfaces as `ErrInputPending` (chatd → 409) instead of
      hanging. (`pkg/harness/run_turn.go`)
- [x] **docs:** channel documented in `docs/chat.md`; `trust_prompt` answer
      aliases are `proceed` / `deny`.

### Verification notes / follow-ups

- The per-option keystroke is **digit + `\r`** (`"1\r"`). This is the one part
  not validated against a live Claude Code build in this environment; it is an
  isolated constant in `parseMenuOptions` (`pkg/turns/harness/claudecode`).
  Confirm against a real folder-trust dialog and adjust if a digit alone
  auto-confirms (the trailing `\r` would then hit an empty REPL, a harmless
  no-op) or if the menu needs arrow navigation instead.
- **(later)** `InputRequestTimeout`; additional detectors (onboarding/theme,
  text/login, tool-permission menus); a structured headless signal at the
  wrapper layer for non-chat consumers.
- Fixed two pre-existing `cmd/harness-chatd` failures uncovered while testing
  (both red at HEAD before this change, unrelated to the feature):
  - `TestSSE_APIErrorPropagates` hung because commit `14cf42e` added
    `requiresPromptReadiness("claude-code")` but the generic `mock` never
    rendered the `"Claude Code"`/`"❯"` ready prompt, so `Send` blocked forever.
    Fixed by a reusable `--ready-prompt` mock flag (emit the ready prompt +
    consume one line before the mode), modelling a real "ready → send →
    mid-turn error" flow.
  - `TestRunTurnEndpoint_ClaudeStyleOneShot` raced a clean exit
    (`StatusIdle` → `Errored{"harness exited"}`, `generic.go:54`) against the
    screen-derived `TurnComplete`. Fixed by keeping the fake harness alive
    after the marker (a real interactive harness stays up; RunTurn stops it).
  - Latent robustness gaps these exposed (follow-ups): `waitReadyForSend`
    blocks until ctx if a harness never becomes ready (e.g. rate-limited at
    startup); a clean exit immediately after a completed turn can still race
    the turn-complete signal for non-interactive harnesses.
- Out of scope / pre-existing: `pkg/discovery` tests fail in this tree (the
  branch author's in-progress probe work; those files were already modified at
  session start and are untouched by this change).
