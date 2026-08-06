# Client libraries

The repo ships two reference clients for the [HTTP gateway](gateway.md) under `clients/` — a
stdlib-only Python module and a dependency-free TypeScript module. They are **thin transports**: they
map method calls to `harness-chatd` routes, decode the JSON, and re-raise errors. They validate
nothing, so every rejection you see is the server's.

Both mirror the same object model:

| Concept | Python | TypeScript |
|---|---|---|
| Gateway handle | `Client(base_url, timeout=30.0)` | `new Client(baseUrl)` |
| One conversation | `Conversation` | `Conversation` |
| Control lease | `with conv.control():` | `await conv.withControl(fn)` |
| Event stream | `for ev in conv.events():` | `for await (const ev of conv.events())` |
| Error | `HarnessChatError(status, code, message)` | `HarnessChatError` with `.status`, `.code` |

## Python

Stdlib only — no pip install. Put `clients/python` on `PYTHONPATH` and import `harness_chat`.

```python
from harness_chat import Client, HarnessChatError

client = Client("http://127.0.0.1:8080")
conv = client.open(
    harness="codex",
    binary_path="/usr/local/bin/codex",
    working_dir="/path/to/project",
    effort="high",            # optional
    model="gpt-5-codex",      # optional
    permission_mode="ask",    # optional
)

with conv.control():
    turn_id = conv.send("summarize this project")
    for ev in conv.events():
        if ev.type == "turn" and ev.turn.id == turn_id and ev.turn.state == "complete":
            print(ev.turn.text)
            break

for turn in conv.history():
    print(turn.role, turn.text)
conv.close()
```

### API

```python
class Client:
    def __init__(self, base_url: str, timeout: float = 30.0) -> None: ...
    def open(self, *, harness, binary_path, args=None, working_dir="", env=None,
             cols=0, rows=0, effort=None, model=None, permission_mode=None) -> Conversation: ...
    def list(self) -> list[dict]: ...

class Conversation:
    def acquire(self) -> str: ...          # returns the control token
    def release(self) -> None: ...
    def control(self): ...                 # context manager around acquire/release
    def send(self, text: str) -> str: ...  # returns turn_id
    def history(self) -> list[Turn]: ...
    def events(self) -> Iterator[TurnEvent]: ...
    def close(self) -> None: ...           # swallows a 404 (already closed)
```

`Turn` and `TurnEvent` are dataclasses with `from_json` constructors. `TurnEvent.type` is one of
`turn`, `input_request`, `input_resolved`; `turn` is `None` on the two input frames.

`Effort` and `PermissionMode` are `Literal` aliases for editor completion only — nothing is checked at
runtime.

Run the example and the tests:

```bash
cd clients/python && PYTHONPATH=. python3 examples/basic.py /usr/local/bin/codex codex
```

```bash
cd clients/python && python3 -m unittest discover -s tests -t .
```

## TypeScript / Node

Requires Node **≥ 18.19** (built-in `fetch` and `ReadableStream`). The package is
`@harness-wrapper/chat-client`; `main`/`exports` point straight at `src/index.ts`, so it is consumed
as source via `tsx` or your own bundler.

```ts
import { Client, HarnessChatError } from "./src/index.js";

const client = new Client("http://127.0.0.1:8080");
const conv = await client.open({
  harness: "codex",
  binaryPath: "/usr/local/bin/codex",
  workingDir: "/path/to/project",
  effort: "high",            // optional
  model: "gpt-5-codex",      // optional
  permissionMode: "ask",     // optional
});

await conv.withControl(async () => {
  const turnId = await conv.send("summarize this project");
  for await (const ev of conv.events()) {
    if (ev.type === "turn" && ev.turn?.id === turnId && ev.turn.state === "complete") {
      console.log(ev.turn.text);
      break;
    }
  }
});

console.log(await conv.history());
await conv.close();
```

`OpenOptions` is camelCase (`binaryPath`, `workingDir`, `permissionMode`); the client maps it to the
gateway's snake_case body. `events(signal?)` accepts an `AbortSignal`.

```bash
cd clients/typescript && npm install && npx tsx examples/basic.ts /usr/local/bin/codex codex
```

```bash
cd clients/typescript && npm install && npm run typecheck && npm test
```

## Two things both clients do deliberately

**Unset knobs are omitted, not nulled.** `effort`, `model` and `permission_mode` are added to the
request body only when they are *present* (Python checks `is not None`, so `effort=""` is sent as
`""`). A call that sets none of them produces a body byte-identical to the pre-knob clients: exactly
`harness`, `binary_path`, `args`, `working_dir`, `env`, `cols`, `rows`. Both test suites pin that key
set.

**Typed unions are compile-time only.** `Effort` / `PermissionMode` (TS) and their `Literal`
counterparts (Python) protect against typos in an editor. They do not stop an invalid combination —
notably `permissionMode: "plan"` against codex, which is a 400 from the server
([why](gateway.md#permission-mode-semantics)).

## Running both suites

```bash
make test-clients
```

This target is **not hermetic**: `npm install` needs network (the TypeScript client deliberately ships
no lockfile), and the Node runner needs ≥ 18.19. The Python suite must run from `clients/python` so
`harness_chat.py` lands on `sys.path`.

> **Scope gap.** The [cross-language conformance corpus](../internal/testing/conformance.md) freezes
> the gateway DTOs and the CLI protocol, but does **not** yet cover `clients/`. The client suites are
> ordinary unit tests against an in-process HTTP stub.

## Writing your own client

The gateway is plain HTTP + SSE, so any language works. The contract you are coding against:

1. [Endpoint table](gateway.md#endpoints) — routes, bodies, status codes.
2. [Error format](gateway.md#endpoints) — `{error, code}` with the code table.
3. [Event envelope](gateway.md#the-event-stream) — `data: <json>` frames with a `type` discriminator
   and a `: ping` comment every 15s. No SSE `event:` field is ever emitted, so read the discriminator
   from the payload.
4. [Conversation semantics](chat.md) — control lease, one assistant turn per send, interactive input.

The DTO field names and the route list are frozen as golden snapshots, so a client written against
them stays valid until an intentional, reviewed change.
