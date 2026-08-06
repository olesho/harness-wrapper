# harness-wrapper

A Go toolkit for running CLI agent harnesses (Claude Code, Codex, OpenCode, pi, …)
under supervision and exposing them as programmable chat sessions.
The repository layers in four steps:

1. **`pkg/wrapper/`** — supervises a harness process under a PTY,
   streams its output, and classifies the run into a small vocabulary
   of normalized states (`idle`, `failed`, `interrupted`,
   `waiting_for_input`, `blocked_by_cost`, `retry_later`, …).
2. **`pkg/screen/`** — a vt100 terminal emulator (per [ADR-001](docs/md/internal/decisions/adr-001-vt100.md)
   we wrap [`vt10x`](https://github.com/hinshun/vt10x)) that turns
   the harness's raw PTY byte stream into queryable screen state.
3. **`pkg/turns/`** — per-harness adapters that translate screen state
   + wrapper status into a small set of chat events (`TurnComplete`,
   `ToolCall`, `Blocked`, `Errored`).
4. **`pkg/chat/`** — the Go-level chat API: `Conversation.Open`,
   `AcquireControl`, `Send`, `Events`, `History`. Storage is pluggable
   via the `Store` interface; `pkg/chat/memstore` ships the in-memory
   default.

Transport layers stay out of the core packages and live in separate
binaries that import `pkg/chat`; this repo ships one such gateway,
`cmd/harness-chatd` (HTTP + SSE) — see [Use over HTTP](#use-over-http).

```
                ┌──────────────────────────────┐
                │ pkg/chat (Conversation API)  │
                └──────────────┬───────────────┘
                               │
            ┌──────────────────┴──────────────────┐
            │                                     │
   ┌────────▼──────────┐               ┌──────────▼──────────┐
   │ pkg/turns         │               │ pkg/transcript      │
   │  +harness/codex   │               │  +codex             │
   │  +harness/cc      │               │  +claudecode        │
   │  +harness/opencode│               │  +pi                │
   │  +harness/pi      │               │ (read-only JSONL)   │
   │  +generic         │               └─────────────────────┘
   └────────┬──────────┘
            │
   ┌────────▼──────────┐
   │ pkg/screen        │  vt10x emulator
   └────────┬──────────┘
            │
   ┌────────▼──────────┐
   │ pkg/wrapper       │  PTY supervisor + status classifier
   └───────────────────┘
```

> 📖 **Full documentation** lives under [`docs/md/`](docs/md/README.md) (canonical markdown, renders on
> GitHub) and builds into a themed, dark/light HTML site with SVG diagrams:
>
> ```sh
> make docs        # build docs/md/ → docs/html/
> make docs-serve  # preview at http://localhost:4321
> ```
>
> Start at the [Getting Started](docs/md/guide/getting-started.md) guide or the
> [Architecture](docs/md/internal/architecture.md) overview.

## Install

```sh
go get github.com/olesho/harness-wrapper/pkg/chat
go get github.com/olesho/harness-wrapper/pkg/wrapper       # supervisor only
```

## Use the chat library

```go
import (
    "context"

    "github.com/olesho/harness-wrapper/pkg/chat"
    "github.com/olesho/harness-wrapper/pkg/chat/memstore"
)

func main() {
    ctx := context.Background()
    conv, err := chat.Open(ctx, chat.Options{
        Harness:    "codex",
        BinaryPath: "/usr/local/bin/codex",
        WorkingDir: "/path/to/project",
        Store:      memstore.New(),
    })
    if err != nil { panic(err) }
    defer conv.Close(ctx)

    release, err := conv.AcquireControl(ctx)
    if err != nil { panic(err) }
    defer release()

    turnID, err := conv.Send(ctx, "summarize this project")
    if err != nil { panic(err) }

    for ev := range conv.Events() {
        if ev.Type == chat.EventTurn && ev.Turn.ID == turnID && ev.Turn.State == chat.TurnStateComplete {
            break
        }
    }

    history, _ := conv.History(ctx)
    _ = history // [{Role:"user", Text:"summarize this project"}, {Role:"assistant", Text:"..."}]
}
```

See [the Chat API reference](docs/md/guide/chat.md) for the full library reference.

## Use the wrapper alone

```go
import (
    "context"
    "os"

    "github.com/olesho/harness-wrapper/pkg/wrapper"
)

func main() {
    res, err := wrapper.Run(context.Background(), wrapper.Config{
        BinaryPath: "/usr/local/bin/claude",
        Args:       []string{"--print", "hello"},
        Stdout:     os.Stdout,
    })
    if err != nil { panic(err) }
    _ = res.Status // wrapper.StatusIdle, StatusFailed, etc.
}
```

## Use as a CLI

```sh
go install github.com/olesho/harness-wrapper/cmd/harness-wrapper@latest
harness-wrapper claude -- --print hello
```

See the [CLI guide](docs/md/guide/cli.md) for one-shot (`run`) and tmux-detached modes.

## Use over HTTP

`cmd/harness-chatd` exposes `pkg/chat` over HTTP + Server-Sent Events so non-Go
clients can drive multi-turn conversations across a process boundary:

```sh
go run ./cmd/harness-chatd --bind 127.0.0.1:8080
```

v1 has no auth — bind to localhost only. See the [HTTP Gateway guide](docs/md/guide/gateway.md)
for the endpoint reference and ready-to-run Python and TypeScript example clients.

## Supported harnesses

| Harness     | Status detection | Turn detection           | Session ID extraction    | Transcript reader        |
|-------------|------------------|--------------------------|--------------------------|--------------------------|
| codex       | ✅                | ✅ `Token usage:` footer   | ✅ `codex resume <uuid>`   | ✅ `~/.codex/sessions/`    |
| claude-code | ✅                | ✅ `✻ <verb> for Ns` line  | ✅ `claude --resume <uuid>`| ✅ `~/.claude/projects/`   |
| opencode    | ✅                | ⏳ via `waiting_for_input` | ⏳ (no on-screen UUID known) | ⏳ (on-disk store in flux: JSON → SQLite) |
| pi          | ✅                | ⏳ idle + `Busy` spinner    | ⏳ headless via `--mode json` | ✅ `~/.pi/agent/sessions/` |
| generic     | ✅ (fallback)     | ✅ via `waiting_for_input` | —                        | —                        |

The per-harness detail and "adding a harness" workflow are in the
[Adapter Matrix](docs/md/guide/adapters.md). Other harnesses can be supported by implementing
`turns.Adapter` (and optionally the `SessionIDExtractor` / `TranscriptReader` capability interfaces).

## Layout

- `pkg/wrapper/` — PTY supervisor + Status vocabulary
- `pkg/wrapper/trace/` — diagnostic event vocabulary
- `pkg/screen/` — vt100 emulator wrapper (vt10x per [ADR-001](docs/md/internal/decisions/adr-001-vt100.md))
- `pkg/turns/` — turn-detection interface, `generic` fallback, per-harness adapters under `harness/`
- `pkg/transcript/` — read-only harness JSONL parsers (codex, claudecode, pi)
- `pkg/chat/` — Conversation API, Store interface
- `pkg/chat/memstore/` — in-memory `Store` implementation
- `pkg/harness/` — per-harness capability profiles, hook installation, `RunTurn`
- `pkg/oneshot/` — one typed turn, headless, with an auto-accept policy
- `pkg/turnproto/` — the frozen structured-turn wire format + exit codes
- `pkg/env/` — run a structured turn inside a workspace (`internal/env` holds the environment core)
- `pkg/versions/` — read API for the embedded `versions.json` (pinned upstream versions per harness)
- `pkg/discovery/` — "is harness X installed on PATH, at what version?"; `models/` adds the offline
  model registry
- `cmd/harness-wrapper/` — thin CLI front-end for the wrapper
- `cmd/harness-chatd/` — HTTP + SSE gateway exposing `pkg/chat` to non-Go clients
- `cmd/check-versions/` — offline drift check against the npm registry
- `clients/` — Python + TypeScript example clients for `harness-chatd`
- `internal/screenbench/` — bake-off harness used to choose the vt100 emulator + scripted recorder
- `test/corpus/` — recorded byte streams used by the bake-off and the adapter compatibility tests
- `docs/md/` — canonical documentation sources; `docs/gen/` — the Go static-site generator

## Testing

Hermetic suite (vet + gofmt + race + corpus replay):

```sh
make test
```

Per-harness adapter tests double as the **compatibility test suite**: they replay byte streams from
`test/corpus/` and assert that turn detection still fires correctly. The full strategy — five tiers
from golden-snapshot API freezes to nightly live conformance — is documented in
[Testing Tiers](docs/md/internal/testing/README.md).

## Drift-detection pipeline

When an upstream CLI ships a new version, the TUI markers / classifier strings / transcript schemas
can shift and break our adapters. The `Makefile` defines a local, developer-on-demand pipeline that
catches drift before users do:

```sh
make check-versions        # offline pinned-vs-latest check via the npm registry (~2s, free)
make rebake-corpus HARNESS=<name> SCENARIO=<name>   # refresh one scenario
make rebake-corpus-all     # refresh all 18 scenarios (paid for codex/claude)
```

`pkg/versions/versions.json` pins each harness to the upstream version its adapter was last verified
against. When drift is detected, [Versions & Drift](docs/md/internal/versions-drift.md) walks through
diagnosing, re-baking the corpus, tightening the regex, and bumping the pin.
