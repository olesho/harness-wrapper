# harness-chatd clients

Reference clients for the [`harness-chatd`](../cmd/harness-chatd) HTTP + SSE sidecar.

> 📖 The full endpoint reference, SSE envelope, and protocol walkthrough are in the
> **[HTTP Gateway guide](../docs/md/guide/gateway.md)**. This file only covers running the examples.

## Run the sidecar

```sh
go run ./cmd/harness-chatd --bind 127.0.0.1:8080
```

> **Auth:** none in v1. Bind to localhost only.

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
