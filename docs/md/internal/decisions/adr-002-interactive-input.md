# ADR-002: interactive input requests (trust dialogs & blocking prompts)

**Status:** Accepted — implemented 2026-06-15 (trust detector; general channel)

## Context

A client drives Claude Code's interactive TUI under a PTY through harness-wrapper. In an untrusted
worktree, Claude Code blocks at startup on a folder-trust dialog ("Do you trust the files in this
folder?") with a numbered menu (`1. Yes, proceed` / `2. No, exit`). Nothing downstream can make
progress until that menu is answered, and originally there was no way to either learn about it or
answer it through the interface.

The original flow **deadlocked silently**:

- `chat.Send` → `waitReadyForSend` blocks until the screen shows `"Claude Code"` **and** `"❯"`. The
  trust dialog never reaches that state, so `Send` / `RunTurn` hang until the context times out, with
  no signal explaining why.
- The wrapper's `Prompt`-pattern → `StatusWaitingForInput` is the wrong tool: it is gated on 15s of
  quiet, matches only the **trailing** screen line, and carries no structured choices.
- `--dangerously-skip-permissions` is **not** a reliable bypass: it replaces the folder-trust prompt
  with its own blocking "Bypass Permissions mode" acceptance screen on first run. The blocking dialog
  is a recurring *shape*, not a one-off — which is why the answer is an interaction channel, not a flag.

Two distinct consumers must be served:

1. **Interactive / remote** (the `harness-chatd` client, or any in-process driver): wants to be told a
   prompt is blocking and answer it live.
2. **One-shot** (`harness.RunTurn`): synchronous; needs the prompt resolved by a pre-supplied policy or
   it times out.

## Decision

Add an **out-of-band `InputRequest` / `Answer` channel** that rides alongside the existing turn flow.
Three principles:

1. **Semantic answers, keystrokes hidden.** The client answers `"Yes, proceed"` (an option id), never
   raw bytes. The wrapper owns the keystroke translation, exactly as `Send` already hides the
   submit-key. Per-harness keystroke knowledge stays server-side.
2. **Screen-based detection in the turns adapter.** The rendered `screen.Snapshot().Text` shows the
   question as clean text; the raw PTY stream is ANSI soup. Detection lives in the per-harness
   [`turns.Adapter`](../turns.md), which already has the snapshot and a fingerprint-dedup pattern.
3. **General channel, trust detector first.** The `InputRequest` abstraction is generic
   (`trust_prompt | menu_select | confirm | text_input`); v1 ships the Claude folder-trust +
   bypass-acceptance detectors. Other detectors (onboarding/theme, login/text, per-tool permission
   menus) are follow-ups.

### Two resolution mechanisms

A detected request is resolved by, in order:

1. **Declarative `InputPolicy`, pre-configured at open time** (works in-process and remotely, because
   it is JSON-serializable). The client pre-decides dispositions, e.g. "auto-answer `trust_prompt` →
   `proceed`". This is what makes one-shot `RunTurn` work unattended.
2. **Live hook** when the policy says `ask` (or doesn't match): in-process an `OnInputRequest`
   callback; remote, the request is emitted on `Events()` / SSE and the client answers via
   `Conversation.Answer` / `POST …/input`.

Default disposition is `ask` — the human/client stays in the loop unless they opt into auto-answering.

### Layer 1 — `pkg/turns`: event kinds carrying structured choices

`InputRequested` / `InputResolved` `Kind`s, an `InputRequest` (with per-option `Keys` and an `Alias`
for portable policy matching), and an `Event.Input` field. The claudecode adapter's `OnScreen` gains
an **anchored** matcher keyed on the stable question line, parsing the numbered options into `Options`
with `Keys: []byte("<n>\r")`. It emits `InputRequested` once per distinct dialog (fingerprinted on a
hash of `Kind + Prompt + option labels`, so redraws collapse and a genuinely new dialog gets a fresh
id) and `InputResolved` when the dialog clears.

### Layer 2 — `pkg/chat.Conversation`: the client interface

A client-facing [`InputRequest`](../../guide/chat.md#interactive-input-blocking-prompts) (no `Keys`),
the `ConversationEvent` envelope with an `EventType` discriminator, `Answer` (requires the control
token), and the open-time `InputPolicy` + `OnInputRequest`. `handleTurnsEvent` stores the pending
request (keeping `Keys` server-side), runs the resolution order, and — if unresolved — emits
`EventInputRequest`. `waitReadyForSend` gains an early `currentInput != nil → ErrInputPending` check,
so a client that calls `Send` before answering gets a clear error instead of a silent hang.

### Layer 3 — `cmd/harness-chatd`: remoted over HTTP + SSE

- **SSE** adopts a **typed envelope** with back-compat: every frame gains a `type` field; turn frames
  stay `{"type":"turn","turn":{…}}` (byte-for-byte the old payload), so existing consumers keep
  working. New frames `{"type":"input_request",…}` / `{"type":"input_resolved",…}`; the DTO omits
  `Keys`.
- **New endpoint** `POST /v1/conversations/{id}/input` with `{token, request_id, option_id, text}` →
  `Answer`, reusing the control-token guard.
- **Open** accepts an optional `input_policy`; **`harness.RunTurn`** gains `TurnConfig.InputPolicy`, so
  a one-shot run in an untrusted dir is unattended via
  `InputPolicy{ByKind:{"trust_prompt":{Kind:"answer",OptionID:"proceed"}}}`. With no policy an
  unresolved request surfaces as a turn error rather than hanging.

### End-to-end (remote, live answer)

```
open conv (input_policy: trust=ask) → GET /events (SSE)
SSE ─▶ {"type":"input_request","input":{kind:"trust_prompt",
        options:[{id:"yes_proceed",alias:"proceed",label:"Yes, proceed"},
                 {id:"no_exit",alias:"deny",label:"No, exit"}]}}
POST /control → token
POST /input {token, request_id, option_id:"yes_proceed"}
   └▶ Conversation.Answer → WriteStdin("1\r")
SSE ─▶ {"type":"input_resolved", …}   (dialog cleared; screen now "Claude Code … ❯")
POST /messages …                       (normal turn flow proceeds)
```

Multi-step onboarding (trust → theme → bypass-accept) falls out for free: each new dialog is a fresh
`InputRequest` with a new id.

## Consequences

- Reuses every existing extension point — adapter `OnScreen`, pending-state, the control queue, the SSE
  fanout, `WriteStdin` — and adds **no new transport**.
- `Conversation.Events()` changed element type to the `ConversationEvent` envelope (an **internal**
  break; external SSE wire compatibility preserved by the additive `type` discriminator).
- Keystroke knowledge stays localized to the per-harness adapter (one table per dialog), so adding a
  harness or a dialog is a self-contained change.
- A new failure mode: an unanswered request blocks the harness indefinitely — mitigated by
  `InputPolicy` for unattended runs and (follow-up) an optional `InputRequestTimeout`.

## Status

Fully implemented across `pkg/turns` (kinds + `InputRequest`/`InputOption`/`Event.Input`), the
claudecode adapter (anchored folder-trust + bypass detectors, dedup, numbered-menu parser),
`pkg/chat` (envelope, `Answer`, policy/handler/surface resolution, `ErrInputPending` guard, sentinel
errors), `cmd/harness-chatd` (typed SSE, `POST …/input`, `input_policy`), and `harness.RunTurn`.
Documented in the [Chat API](../../guide/chat.md#interactive-input-blocking-prompts).

**The open verification above is now closed — and it went the other way.** Confirmed live against
claude 2.1.251 (tmux, 2026-08-29): the folder-trust dialog is rendered with **no numbers at all**,
and the menu *does* need arrow navigation. See the amendment below. Later: `InputRequestTimeout`;
additional detectors (onboarding/theme, text/login, tool-permission menus); a structured headless
signal at the wrapper layer for non-chat consumers.

## Amendment (2026-08-29): the unnumbered selector menu, and four detection states

### What claude 2.1.251 actually renders

```
Quick safety check: Is this a project you created or one you trust? …
Claude Code'll be able to read, edit, and execute files here.
Security guide
 ❯ No, exit
   Yes, I trust this folder
Enter to confirm · Esc to cancel
```

Zero numbered lines, so `menuRE` yields nothing — and the default highlight is on **"No, exit"**.

### Two shapes, one entry point

`parseMenuOptions` now tries the **numbered** parser over the whole frame first and falls back to a
**selector** parser over the text *after* the anchor. Numbered-first is deliberate: a numbered menu
also paints a `❯`, and its digit keys are **absolute** (immune to a stale highlight), so it must win
whenever it parses. Selector scanning is anchor-scoped because `❯` is also the composer glyph.

A selector block is the contiguous run of lines that share the highlighted row's **label column**,
capped at 8 rows and requiring at least 2. That column rule is what keeps `Security guide`, the
workspace path and the `Enter to confirm · Esc to cancel` footer from becoming options — prose starts
at the box column, choice labels start ~3 columns in. Anything else is *unparseable*, never a guess.

### Positional keys

There are no digits, so selection is **relative to the highlight**: the highlighted row answers with
a bare CR, a row *below* it with `ESC [ B` repeated, one *above* with `ESC [ A`, each followed by CR.
Three properties are load-bearing, and each has its own test:

- **Never a bare CR for a non-highlighted row.** Claude highlights "No, exit", so a bare CR (or an
  "the affirmative row comes first" assumption) *quits claude at startup*. That is the whole bug.
- **The keys are ONE write.** `Conversation.write` passes the entire `opt.Keys` to a single
  `WriteStdin`. `ESC [ B` in one write parses as Down; split across writes it is a lone Esc, which
  **cancels** the dialog. Do not split it, do not sleep between arrows.
- **The highlight never enters `inputID`.** It hashes kind + prompt + option labels only, so moving
  the highlight does not mint a "new" request the policy answers a second time.

Option ids are the 0-based row index, a distinct namespace from the numbered form's 1-based digits.
That is fine — ids need only be unique within a request and nothing persists them — and `findOption`
matches id, alias *and* label, so alias-based policy (`proceed` / `deny`) spans both shapes.

### Four detection states, because a bool lied

`DetectInput`'s `(nil, false)` conflated "no dialog" with "a dialog I cannot read", and the second is
**permanent**: an unparseable blocking dialog was reported as no dialog at all, so no `InputRequested`
was emitted, `readyForInput` fell through to its bare `❯`-contains and called the dialog READY, and
`Send` typed the prompt into the menu and submitted it onto "No, exit". `DetectInputDetail` now
returns one of:

| state               | screen                                              | consumers do                        |
| ------------------- | --------------------------------------------------- | ----------------------------------- |
| `DetectNone`        | no anchor                                            | nothing                             |
| `DetectPending`     | anchor, nothing choice-shaped yet (mid-render frame) | not ready; stay silent              |
| `DetectUnparseable` | anchor **and** choice-shaped lines we cannot parse   | not ready; report loudly            |
| `DetectOK`          | anchor and a usable option set                       | emit `InputRequested`               |

`DetectInput` remains the two-value wrapper (`det == DetectOK`) for callers that only need "can I
answer this?". The readiness gate uses the detail form — that is what keeps this ADR's "one source of
truth" claim true — and treats **every** non-`DetectNone` state as blocking. `waitReadyForSend` adds
`ErrUnrecognizedDialog`, following the onboarding-wall precedent with one difference: it fires only
after the state survives a re-check of the live screen, because an unparseable frame can simply be a
half-painted one. The adapter additionally emits one `Errored` naming the anchor and the raw
candidate lines, deduped on a fingerprint of both.

**Rejected:** emitting an `InputRequested` with labels and empty `Keys`. The policy would match
`proceed` by alias, write zero bytes, and hang exactly as before — with a log line making it look
handled. Silence is bad; a fake answer is worse. Also rejected: a new `turns.Kind`. That vocabulary
is intentionally small; `Errored` plus a named `Event.Reason` is the sanctioned shape.
