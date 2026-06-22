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
| `POST` | `/v1/turns` | `{harness, binary_path, prompt, cols?, rows?, input_policy?, timeout_seconds?}` | one-shot turn result |
| `POST` | `/v1/conversations` | `{harness, binary_path, args?, working_dir?, env?, cols?, rows?, input_policy?}` | `{id}` |
| `GET` | `/v1/conversations` | — | `[{id, harness, session_id?}]` |
| `DELETE` | `/v1/conversations/{id}` | — | `204` |
| `POST` | `/v1/conversations/{id}/control` | — | `{token}` |
| `DELETE` | `/v1/conversations/{id}/control/{token}` | — | `204` |
| `POST` | `/v1/conversations/{id}/messages` | `{token, text}` | `{turn_id}` |
| `POST` | `/v1/conversations/{id}/input` | `{token, request_id?, option_id?, text?}` | `204` |
| `GET` | `/v1/conversations/{id}/events` | — | **SSE** stream |
| `GET` | `/v1/conversations/{id}/history` | — | `{turns: [...]}` |
| `GET` | `/v1/conversations/{id}/screen` | — | `{text, cols, rows, cursor_col, cursor_row, generation}` |

Errors come back as `{error, code}` with the HTTP status mapped from the `pkg/chat`
[sentinel errors](chat.md#sentinel-errors) (e.g. `ErrNoControl` → 409, `ErrInputPending` → 409,
`ErrUnknownHarness` → 400). The routes and wire DTOs are frozen as golden snapshots (Layer 0 of the
[testing tiers](../internal/testing/README.md)).

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
`{"by_kind": {"trust_prompt": {"kind": "answer", "option_id": "proceed"}}}`).

## Reference clients

The repo ships runnable example clients under [`clients/`](https://github.com/olesho/harness-wrapper/tree/main/clients).

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
python clients/python/examples/basic.py /usr/local/bin/codex codex
```

**TypeScript / Node** (Node 18+, built-in `fetch`):

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
