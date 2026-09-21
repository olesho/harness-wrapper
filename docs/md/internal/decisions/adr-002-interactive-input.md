# ADR-002: interactive input requests (trust dialogs & blocking prompts)

**Status:** Accepted — implemented 2026-06-15; amended 2026-08-29 (unnumbered selector menu, four
detection states) and 2026-09-06 (`bypass_acceptance` kind). This record states the decision as it
holds now; [History](#history) lists what each amendment changed.

**Intent:** principle 3, *normalize, don't leak*; principle 2, *a wrong verdict is worse than no
verdict*; principle 7, *say what is enforced, not what is intended*
([INTENT](../../../../INTENT.md#design-principles)). It also applies principle 1 (the dialog changed
shape upstream and was re-verified live), 5 (HTTP + SSE stay in `cmd/harness-chatd`) and 6 (the SSE
change is additive).

## Context

A client drives Claude Code's interactive TUI under a PTY through harness-wrapper. In an untrusted
worktree, Claude Code blocks at startup on a folder-trust dialog. Nothing downstream can make
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

1. **Semantic answers, keystrokes hidden.** The client answers an option id or alias (`proceed`,
   `deny`), never raw bytes. The wrapper owns the keystroke translation, exactly as `Send` already
   hides the submit-key. Per-harness keystroke knowledge stays server-side.
2. **Screen-based detection in the turns adapter.** The rendered `screen.Snapshot().Text` shows the
   question as clean text; the raw PTY stream is ANSI soup. Detection lives in the per-harness
   [`turns.Adapter`](../turns.md), which already has the snapshot and a fingerprint-dedup pattern.
3. **General channel, trust detector first.** The `InputRequest` abstraction is generic
   (`trust_prompt | bypass_acceptance | menu_select | confirm | text_input`); the Claude folder-trust
   and bypass-acceptance detectors ship first. Other detectors (onboarding/theme, login/text, per-tool
   permission menus) are follow-ups.

### Kinds: the bypass acceptance screen is not a trust prompt

`claudecode.DetectInput` stamps claude-code's `--dangerously-skip-permissions` acceptance screen
`bypass_acceptance` (`claudecode.KindBypassAcceptance`), alongside `trust_prompt`
(`claudecode.KindTrustPrompt`) for the folder-trust dialog. Every policy surface keys on `Kind`
alone, so under one shared kind "trust this folder, but never silently accept a
skip-all-permissions launch" was inexpressible. harness-wrapper's own unattended policies name both
kinds.

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
for portable policy matching), and an `Event.Input` field. The claudecode adapter's `OnScreen` has an
**anchored** matcher keyed on the stable question line, which parses the choices into `Options` (see
[Menu shapes](#menu-shapes-and-keys)). It emits `InputRequested` once per distinct dialog
(fingerprinted on a hash of `Kind + Prompt + option labels`, so redraws collapse and a genuinely new
dialog gets a fresh id) and `InputResolved` when the dialog clears.

### Layer 2 — `pkg/chat.Conversation`: the client interface

A client-facing [`InputRequest`](../../guide/chat.md#interactive-input-blocking-prompts) (no `Keys`),
the `ConversationEvent` envelope with an `EventType` discriminator, `Answer` (requires the control
token), and the open-time `InputPolicy` + `OnInputRequest`. `handleTurnsEvent` stores the pending
request (keeping `Keys` server-side), runs the resolution order, and — if unresolved — emits
`EventInputRequest`. `waitReadyForSend` checks `currentInput != nil → ErrInputPending` early, so a
client that calls `Send` before answering gets a clear error instead of a silent hang.

### Layer 3 — `cmd/harness-chatd`: remoted over HTTP + SSE

- **SSE** uses a **typed envelope** with back-compat: every frame carries a `type` field; turn frames
  stay `{"type":"turn","turn":{…}}` (byte-for-byte the old payload), so existing consumers keep
  working. Input frames are `{"type":"input_request",…}` / `{"type":"input_resolved",…}`; the DTO
  omits `Keys`.
- **Endpoint** `POST /v1/conversations/{id}/input` with `{token, request_id, option_id, text}` →
  `Answer`, reusing the control-token guard.
- **Open** accepts an optional `input_policy`; **`harness.RunTurn`** has `TurnConfig.InputPolicy`, so
  a one-shot run in an untrusted dir is unattended via
  `InputPolicy{ByKind:{"trust_prompt":{Kind:"answer",OptionID:"proceed"}}}`. With no policy an
  unresolved request surfaces as a turn error rather than hanging.

### Menu shapes and keys

Claude Code has rendered the same dialog two ways, and `parseMenuOptions` reads both. Confirmed live
against claude 2.1.251 (tmux, 2026-08-29), the folder-trust dialog has **no numbers at all**, needs
arrow navigation, and puts the default highlight on **"No, exit"**:

```
Quick safety check: Is this a project you created or one you trust? …
Claude Code'll be able to read, edit, and execute files here.
Security guide
 ❯ No, exit
   Yes, I trust this folder
Enter to confirm · Esc to cancel
```

Zero numbered lines, so the numbered parser's `menuRE` yields nothing. `parseMenuOptions` therefore
tries the **numbered** parser over the whole frame first and falls back to a **selector** parser over
the text *after* the anchor. Numbered-first is deliberate: a numbered menu also paints a `❯`, and
its digit keys are **absolute** (immune to a stale highlight), so it must win whenever it parses.
Selector scanning is anchor-scoped because `❯` is also the composer glyph.

- **Numbered form** (`1. Yes, proceed` / `2. No, exit`). Option ids are the 1-based digits; keys are
  the digit followed by CR (`Keys: []byte("<n>\r")`). The digit selects the row regardless of the
  initial highlight; the CR confirms.
- **Selector form.** A selector block is the contiguous run of lines that share the highlighted
  row's **label column**, capped at 8 rows and requiring at least 2. That column rule is what keeps
  `Security guide`, the workspace path and the `Enter to confirm · Esc to cancel` footer from
  becoming options — prose starts at the box column, choice labels start ~3 columns in. Anything
  else is *unparseable*, never a guess. Option ids are the 0-based row index.

The two id namespaces differ, and that is fine — ids need only be unique within a request and
nothing persists them — and `findOption` matches id, alias *and* label, so alias-based policy
(`proceed` / `deny`) spans both shapes.

Selector keys are **relative to the highlight**: the highlighted row answers with a bare CR, a row
*below* it with `ESC [ B` repeated, one *above* with `ESC [ A`, each followed by CR. Three properties
are load-bearing, and each has its own test:

- **Never a bare CR for a non-highlighted row.** Claude highlights "No, exit", so a bare CR (or an
  "the affirmative row comes first" assumption) *quits claude at startup*.
- **The keys are ONE write.** `Conversation.write` passes the entire `opt.Keys` to a single
  `WriteStdin`. `ESC [ B` in one write parses as Down; split across writes it is a lone Esc, which
  **cancels** the dialog. Do not split it, do not sleep between arrows.
- **The highlight never enters `inputID`.** It hashes kind + prompt + option labels only, so moving
  the highlight does not mint a "new" request the policy answers a second time.

### Four detection states

`DetectInput`'s two-value `(nil, false)` conflated "no dialog" with "a dialog I cannot read", and the
second is **permanent**: an unparseable blocking dialog was reported as no dialog at all, so no
`InputRequested` was emitted, `readyForInput` fell through to its bare `❯`-contains and called the
dialog READY, and `Send` typed the prompt into the menu and submitted it onto "No, exit".
`DetectInputDetail` returns one of:

| state               | screen                                              | consumers do                        |
| ------------------- | --------------------------------------------------- | ----------------------------------- |
| `DetectNone`        | no anchor                                            | nothing                             |
| `DetectPending`     | anchor, nothing choice-shaped yet (mid-render frame) | not ready; stay silent              |
| `DetectUnparseable` | anchor **and** choice-shaped lines we cannot parse   | not ready; report loudly            |
| `DetectOK`          | anchor and a usable option set                       | emit `InputRequested`               |

`DetectInput` remains the two-value wrapper (`det == DetectOK`) for callers that only need "can I
answer this?". The readiness gate uses the detail form — one source of truth for "is a dialog
blocking?" — and treats **every** non-`DetectNone` state as blocking. `waitReadyForSend` returns
`ErrUnrecognizedDialog`, following the onboarding-wall precedent with one difference: it fires only
after the state survives a re-check of the live screen, because an unparseable frame can simply be a
half-painted one. The adapter additionally emits one `Errored` naming the anchor and the raw
candidate lines, deduped on a fingerprint of both.

### End-to-end (remote, live answer)

```
open conv (input_policy: trust=ask) → GET /events (SSE)
SSE ─▶ {"type":"input_request","input":{kind:"trust_prompt",
        options:[{id:"0",alias:"deny",label:"No, exit"},
                 {id:"1",alias:"proceed",label:"Yes, I trust this folder"}]}}
POST /control → token
POST /input {token, request_id, option_id:"proceed"}
   └▶ Conversation.Answer → WriteStdin("\x1b[B\r")   (one write: Down, then Enter)
SSE ─▶ {"type":"input_resolved", …}   (dialog cleared; screen now "Claude Code … ❯")
POST /messages …                       (normal turn flow proceeds)
```

Multi-step onboarding (trust → theme → bypass-accept) falls out for free: each new dialog is a fresh
`InputRequest` with a new id.

## Alternatives

- **A flag instead of a channel** (`--dangerously-skip-permissions`): it swaps one blocking screen
  for another. See Context.
- **The wrapper's `Prompt` pattern / `StatusWaitingForInput`**: quiet-gated, trailing line only, no
  structured choices. See Context.
- **Emitting an `InputRequested` with labels and empty `Keys`** for a dialog that cannot be parsed.
  The policy would match `proceed` by alias, write zero bytes, and hang exactly as before — with a
  log line making it look handled. Silence is bad; a fake answer is worse.
- **A new `turns.Kind` for the unparseable case.** That vocabulary is intentionally small; `Errored`
  plus a named `Event.Reason` is the sanctioned shape.

## Consequences

- Reuses every existing extension point — adapter `OnScreen`, pending-state, the control queue, the SSE
  fanout, `WriteStdin` — and adds **no new transport**.
- `Conversation.Events()` changed element type to the `ConversationEvent` envelope — a deliberate,
  source-incompatible change to the Go API. External SSE wire compatibility is preserved by the
  additive `type` discriminator.
- Keystroke knowledge stays localized to the per-harness adapter (one table per dialog), so adding a
  harness or a dialog is a self-contained change.
- A new failure mode: an unanswered request blocks the harness until it is answered. Callers are not
  left hanging — `Send` fails with `ErrInputPending`, and a one-shot run with no policy returns a turn
  error — and `InputPolicy` covers unattended runs. There is no timeout on a pending request yet (see
  Follow-ups).

Implemented across `pkg/turns` (kinds + `InputRequest`/`InputOption`/`Event.Input`), the claudecode
adapter (anchored folder-trust + bypass detectors, dedup, numbered and selector menu parsers),
`pkg/chat` (envelope, `Answer`, policy/handler/surface resolution, `ErrInputPending` and
`ErrUnrecognizedDialog` guards, sentinel errors), `cmd/harness-chatd` (typed SSE, `POST …/input`,
`input_policy`), and `harness.RunTurn`. Documented in the
[Chat API](../../guide/chat.md#interactive-input-blocking-prompts).

## Follow-ups

- An optional `InputRequestTimeout`.
- Additional detectors: onboarding/theme, text/login, tool-permission menus.
- A structured headless signal at the wrapper layer for non-chat consumers.

## History

- **2026-06-15 — accepted and implemented.** The general channel with the folder-trust and
  bypass-acceptance detectors, both stamped `trust_prompt`, over a numbered-menu parser with
  `digit + "\r"` keys. One verification was left open: whether the live dialog takes a digit.
- **2026-08-29 — the unnumbered selector menu, and four detection states.** The open verification
  closed the other way: claude 2.1.251 renders the dialog with no numbers and needs arrow navigation.
  Added the selector parser with positional keys, `DetectInputDetail`'s four states,
  `ErrUnrecognizedDialog`, and the adapter's `Errored` for an unparseable dialog.
- **2026-09-06 — the bypass acceptance screen has its own kind.** `bypass_acceptance` split from
  `trust_prompt`. harness-wrapper's own unattended policies name both kinds, so their behaviour was
  unchanged; the release was expressive-only. See PUPPET-495 / PUPPET-507.
