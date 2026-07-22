# harness-chatd clients

Reference clients for the [`harness-chatd`](../cmd/harness-chatd) HTTP + SSE sidecar.

> 📖 The full endpoint reference, SSE envelope, and protocol walkthrough are in the
> **[HTTP Gateway guide](../docs/md/guide/gateway.md)**. This file only covers running the examples.

Both clients accept `effort` / `model` on `open()`. They are **not** symmetric — `effort` is
validated and hard-fails, `model` is silently dropped on a harness that has no model flag; see
[`effort` and `model` semantics](../docs/md/guide/gateway.md#effort-and-model-semantics).

## Run the sidecar

```sh
go run ./cmd/harness-chatd --bind 127.0.0.1:8080
```

> **Auth:** none in v1. Bind to localhost only.

## Python

Stdlib only. `PYTHONPATH=.` is required: `sys.path[0]` is the *script's* directory
(`clients/python/examples`), never the cwd, so `cd` alone does not put `harness_chat` on the path.

```sh
cd clients/python && PYTHONPATH=. python3 examples/basic.py /usr/local/bin/codex codex
```

## TypeScript / Node

Node **>=18.19** (or **>=20.6** on the 20.x line) — the floor declared in `package.json`'s
`engines`, for built-in `fetch` plus the module-register hook `tsx` uses.

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
