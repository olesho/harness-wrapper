# harness-chatd clients

Reference clients for the [`harness-chatd`](../cmd/harness-chatd) HTTP + SSE sidecar.

## Run the sidecar

```sh
go run ./cmd/harness-chatd --bind 127.0.0.1:8080
```

> **Auth:** none in v1. Bind to localhost only.

## HTTP surface

| Method | Path | Body | Response |
|---|---|---|---|
| `POST` | `/v1/conversations` | `{harness, binary_path, args?, working_dir?, env?, cols?, rows?}` | `{id}` |
| `GET`  | `/v1/conversations` | — | `[{id, harness, session_id?}]` |
| `DELETE` | `/v1/conversations/{id}` | — | 204 |
| `POST` | `/v1/conversations/{id}/control` | — | `{token}` |
| `DELETE` | `/v1/conversations/{id}/control/{token}` | — | 204 |
| `POST` | `/v1/conversations/{id}/messages` | `{token, text}` | `{turn_id}` |
| `GET`  | `/v1/conversations/{id}/events` | — | SSE stream of `{turn, error?}` |
| `GET`  | `/v1/conversations/{id}/history` | — | `{turns: [...]}` |
| `GET`  | `/healthz` | — | `{ok: true}` |

Errors: `{error, code}` with HTTP status mapped from `pkg/chat` sentinels.

## Python

Stdlib only.

```sh
python clients/python/examples/basic.py /usr/local/bin/codex codex
```

## TypeScript / Node

Node 18+ (built-in `fetch`).

```sh
cd clients/typescript
npm install
npx tsx examples/basic.ts /usr/local/bin/codex codex
```

## curl smoke test

```sh
ID=$(curl -s -XPOST localhost:8080/v1/conversations \
  -d '{"harness":"codex","binary_path":"/usr/local/bin/codex"}' | jq -r .id)

curl -N localhost:8080/v1/conversations/$ID/events &

TOK=$(curl -s -XPOST localhost:8080/v1/conversations/$ID/control | jq -r .token)
curl -s -XPOST localhost:8080/v1/conversations/$ID/messages \
  -d "{\"token\":\"$TOK\",\"text\":\"hello\"}"
```
