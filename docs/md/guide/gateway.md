# HTTP Gateway

`cmd/harness-chatd` exposes [`pkg/chat`](chat.md) over HTTP + Server-Sent Events so non-Go clients can
drive multi-turn conversations across a process boundary. It is a thin transport: the conversation
semantics (control token, turn model, interactive input) are exactly those of `pkg/chat`.

```bash
go run ./cmd/harness-chatd --bind 127.0.0.1:8080
```

> **Auth: none in v1.** Bind to localhost only. The default bind is `127.0.0.1:8080`.

## Endpoints

All conversation routes are under `/v1`. Path parameters are `{id}` (conversation id) and `{token}`
(control token).

| Method | Path | Body | Response |
|---|---|---|---|
| `GET` | `/healthz` | — | `{"ok": true}` |
| `POST` | `/v1/turns` | `{harness, turn_harness?, binary_path, prompt, args?, working_dir?, env?, exit_after_turn?, cols?, rows?, input_policy?, timeout_seconds?, effort?, model?, permission_mode?}` | one-shot turn result |
| `POST` | `/v1/conversations` | `{harness, binary_path, args?, working_dir?, env?, cols?, rows?, input_policy?, effort?, model?, permission_mode?, disable_codex_auto_dismiss?, auto_skip_codex_update_notice?}` | `{id}` |
| `GET` | `/v1/conversations` | — | `[{id, harness, session_id?, permission_mode?}]` |
| `DELETE` | `/v1/conversations/{id}` | — | `204` |
| `POST` | `/v1/conversations/{id}/control` | — | `{token}` |
| `DELETE` | `/v1/conversations/{id}/control/{token}` | — | `204` |
| `POST` | `/v1/conversations/{id}/messages` | `{token, text}` | `{turn_id}` |
| `POST` | `/v1/conversations/{id}/input` | `{token, request_id?, option_id?, text?}` | `204` |
| `GET` | `/v1/conversations/{id}/events` | — | **SSE** stream |
| `GET` | `/v1/conversations/{id}/history` | — | `{turns: [...]}` |
| `GET` | `/v1/conversations/{id}/screen` | — | `{text, cols, rows, cursor_col, cursor_row, generation}` |

`exit_after_turn` on `/v1/turns` is **required in effect**: it defaults to `true` when omitted, and an
explicit `false` is rejected with 400 `unsupported` — the route is one-shot by construction.

Errors come back as `{error, code}` with the HTTP status mapped from the `pkg/chat`
[sentinel errors](chat.md#sentinel-errors) (e.g. `ErrNoControl` → 409, `ErrInputPending` → 409,
`ErrUnknownHarness` → 400), plus `wrapper.ErrInvalidConfig` → 400 `invalid_config` — the first
non-`pkg/chat` sentinel in that map. The routes and wire DTOs are frozen as golden snapshots (Layer 0
of the [testing tiers](../internal/testing/README.md)).

### `effort` and `model` semantics

Both fields are accepted on `POST /v1/conversations` and `POST /v1/turns`, and both are threaded to
`wrapper.Config`. They do **not** behave symmetrically:

1. **`effort` is validated and hard-fails; `model` is never validated.** `wrapper.validateConfig`
   rejects an effort outside the enum `low`, `medium`, `high`, `xhigh`, `max`, and rejects *any*
   effort on a harness with no effort axis. `model` has no validation at all: on a harness the
   wrapper has no model flag for, the request is accepted and the value is **silently dropped**. So
   `{"harness":"pi","model":"…"}` is a silent no-op, while `{"harness":"pi","effort":"…"}` is an
   error. Do not assume the two knobs fail the same way.
2. **The effort-capable harness names on this gateway are exactly `"codex"` and `"claude-code"`,
   case-sensitively.** `effort` against `opencode`, `pi`, or `generic`/`""` is rejected — the exact
   opposite of `model`, which is a silent no-op on those same three. The gateway resolves the
   adapter by raw string match, so it accepts only `codex`, `claude-code`, `opencode`, `pi`, and
   `generic`/`""`: plain `"claude"` is a 400 `unknown_harness` *before* effort is ever considered
   (even though `pkg/wrapper` itself accepts that spelling), and `"Codex"` 400s as
   `unknown_harness` too, because the adapter lookup does not lowercase or trim. The CLI's name set
   differs — see [`harness-wrapper --effort`](cli.md#flags).
3. **An explicit flag already in `args` wins over the typed field.** If `args` already carries
   `--effort` / `--model` (claude-code) or a `-c model_reasoning_effort=…` / `-c model=…` key
   (codex), the wrapper skips injection and the typed field is silently ignored. A caller migrating
   off the raw-`args` escape hatch will plausibly set both; the old `args` wins.
4. **codex remaps `max` → `xhigh`.** `{"harness":"codex","effort":"max"}` reaches the harness as
   `-c model_reasoning_effort="xhigh"`.

**Rejection mapping.** An invalid `effort` — a bad enum value, or any effort against an accepted
harness that has no effort axis — is a **400 `invalid_config`**. `pkg/chat` classifies the rejected
`wrapper.Config` as [`chat.ErrInvalidOptions`](chat.md#sentinel-errors) (wrapping
`wrapper.ErrInvalidConfig`, so `errors.Is` matches both) rather than letting it fall through as an
internal error; `writeChatError` then checks the more specific `wrapper.ErrInvalidConfig` arm first,
so the wire code is `invalid_config`, not `invalid_options`. Both are 400. Because the check lives
in `pkg/chat`, `POST /v1/conversations`, `POST /v1/turns`, `Reopen`, and
[`harness-wrapper run`](cli.md#flags) all reject an invalid effort identically, before the harness
process launches.

### `permission_mode` semantics

**Requested-at-open, not observed.** The `permission_mode` in the conversation listing reports the
mode the conversation was **opened with**, not a mode read back out of the harness. For every value
chatd accepts, requested == effective at launch, because `pkg/wrapper`'s `validateConfig` *rejects*
(rather than silently no-ops) an unknown mode, a cross-harness native spelling, `plan` on codex, a
non-empty mode on a harness that has no permission axis, and mode/argv contradictions — all surfaced
as 400 `invalid_config`. Two residual gaps remain:

1. **Explicit-flag-wins.** An explicit permission-axis flag in `args` wins over `permission_mode`, in
   any of the three spellings the harnesses accept (bare token `-s`, attached long
   `--sandbox=read-only`, clap's attached short `-sread-only`); the summary still reports what was
   *requested*. The suppression sets are claude/claude-code = `--permission-mode` and
   `--dangerously-skip-permissions`; codex = `-s`, `--sandbox`, `-a`, `--ask-for-approval`, and
   `--dangerously-bypass-approvals-and-sandbox`. The two `--dangerously-*` arms are reachable only for
   a bypass-class mode — any other mode paired with them is rejected with 400 rather than dropped.
2. **In-band mutation.** A client holding the control token can send arbitrary text via
   `POST /v1/conversations/{id}/messages` (claude's `/permissions`, codex's `/approvals`) and flip the
   mode inside the TUI. Nothing validates or observes that; the listing keeps reporting the open-time
   value.

The one-shot response does not echo the mode; only the conversation listing reports it.

**The same key means something different on `StructuredTurnResult`.** `permission_mode` on the
[`structured-run` result](cli.md#the-reported-permission-mode) — not a gateway response — reports the
**effective canonical rung** resolved from the final launch arguments, and never a harness-native
spelling. The gateway's key reports what was *requested* and can legitimately carry `acceptEdits`,
`dontAsk`, or `read-only`. Two readings, one key name, by design: do not share a parser or a schema
between them.

**codex has no launch-time plan mode.** `permission_mode: "plan"` against codex is a 400
`invalid_config`, not a no-op: codex ships no launch flag for the non-executing rung, and silently
dropping it would start codex with *no* launch-time restriction for a caller who explicitly asked for
the most restrictive one. Codex plan mode is reachable only in-band, by sending `/plan` as a message
once the conversation is open. Everywhere else the vocabulary is the canonical rungs `plan`, `manual`,
`ask`, `auto`, `bypass`, plus each harness's own spellings — `acceptEdits` / `dontAsk` /
`bypassPermissions` for claude and claude-code, `read-only` / `workspace-write` /
`danger-full-access` for codex. A native spelling sent to the harness that does not own it is
rejected, not ignored.

**`bypass` over the wire has no `IS_SANDBOX=1`.** The `--sandbox-defaults` [CLI flag](cli.md#flags)
contributes args **and** env — notably `IS_SANDBOX=1`, which suppresses claude-code's *Bypass
Permissions mode* acceptance screen and allows running as root. chatd has no `--sandbox-defaults`
equivalent and no `--auto-accept`; its only levers are the request's `env` and `input_policy`. So a
chatd caller asking for `bypass` must either pass `IS_SANDBOX=1` in `env`, or supply an `input_policy`
with `by_kind: {"bypass_acceptance": …}` — otherwise the harness stops on the acceptance screen,
surfaced as a `bypass_acceptance` input request, and root is disallowed. Note the kind: the
acceptance screen has its **own** kind, distinct from the folder-trust dialog's `trust_prompt`, so a
policy that names only `trust_prompt` does **not** cover it (that is deliberate — it lets a caller
trust a folder without also accepting a skip-all-permissions launch). chatd is precisely the containerized/remote entry point where this bites, so use
one of the two levers explicitly.

### The event stream

`GET /v1/conversations/{id}/events` is a `text/event-stream`. Each frame is `data: <JSON>\n\n` where
the JSON is a typed envelope with a `type` discriminator, mirroring `chat.ConversationEvent`:

```jsonc
{"type": "turn",           "turn":  { "id": "…", "role": "assistant", "state": "complete", "text": "…" }}
{"type": "input_request",  "input": { "id": "…", "kind": "trust_prompt", "prompt": "…", "options": [ … ] }}
{"type": "input_resolved", "input": { "id": "…" }}
```

A comment ping (`: ping`) is sent every 15s to keep the connection alive. Subscribe **before** sending
so you don't miss the completion frame.

### One-shot: `POST /v1/turns`

The HTTP analogue of [`harness-wrapper run`](cli.md#one-shot-run): open, drive a single turn, return
the reply, tear down. Supply an `input_policy` to make an unattended turn survive a trust dialog (e.g.
`{"by_kind": {"trust_prompt": {"kind": "answer", "option_id": "proceed"}}}`). For a `bypass`-class
launch add a `"bypass_acceptance"` entry too — it is a separate kind.

## Reference clients

The repo ships runnable example clients under [`clients/`](https://github.com/olesho/harness-wrapper/tree/main/clients).

Both shipped clients expose the three execution-mode knobs on `open()` as typed optional parameters —
`effort` / `model` / `permissionMode` in TypeScript, `effort` / `model` / `permission_mode` in Python
— so reaching them no longer requires hand-assembling raw `args` and re-implementing the per-harness
translation table. The typed unions (`Effort` / `PermissionMode` in TypeScript, the matching
`Literal` aliases in Python) are **compile-time typo protection only**: the clients are thin
transports that validate nothing at runtime and forward whatever they are given, so every rejection
above is still the server's, surfaced as a 400. Leaving a knob unset omits its key from the request
body entirely (rather than sending `null`), which keeps a knob-free `open()` byte-identical to the
pre-knob clients; an explicit `""` is deliberately sent as `""`. Note that `permissionMode: "plan"` is
not expressible against codex at all — see [`permission_mode` semantics](#permission-mode-semantics).

**curl** smoke test:

```bash
ID=$(curl -s -XPOST localhost:8080/v1/conversations \
  -d '{"harness":"codex","binary_path":"/usr/local/bin/codex"}' | jq -r .id)

curl -N localhost:8080/v1/conversations/$ID/events &        # SSE

TOK=$(curl -s -XPOST localhost:8080/v1/conversations/$ID/control | jq -r .token)
curl -s -XPOST localhost:8080/v1/conversations/$ID/messages \
  -d "{\"token\":\"$TOK\",\"text\":\"hello\"}"
```

**Python** (stdlib only):

```bash
cd clients/python && PYTHONPATH=. python3 examples/basic.py /usr/local/bin/codex codex
```

**TypeScript / Node** (Node >=18.19, or >=20.6 on the 20.x line; built-in `fetch`):

```bash
cd clients/typescript && npm install
npx tsx examples/basic.ts /usr/local/bin/codex codex
```

## Typical flow

1. `POST /v1/conversations` → `{id}`.
2. `GET /v1/conversations/{id}/events` → open the SSE stream and keep reading.
3. `POST /v1/conversations/{id}/control` → `{token}`.
4. `POST /v1/conversations/{id}/messages` with `{token, text}` → `{turn_id}`.
5. Watch the stream for `{"type":"turn", "turn":{"state":"complete"}}` matching `turn_id`. Answer any
   `input_request` via `POST …/input`.
6. `GET …/history` for the full transcript; `DELETE …/control/{token}` to release; `DELETE …/{id}` to
   close.

For the semantics behind each step — control queue, turn states, interactive input — see the
[Chat API](chat.md).
