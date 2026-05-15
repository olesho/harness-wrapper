# harness-wrapper

A Go toolkit for running CLI agent harnesses (Claude Code, Codex, …)
under supervision and exposing them as programmable chat sessions.
The repository layers in four steps:

1. **`pkg/wrapper/`** — supervises a harness process under a PTY,
   streams its output, and classifies the run into a small vocabulary
   of normalized states (`idle`, `failed`, `interrupted`,
   `waiting_for_input`, `blocked_by_cost`, `retry_later`, …).
2. **`pkg/screen/`** — a vt100 terminal emulator (per [ADR-001](docs/adr-001-vt100.md)
   we wrap [`vt10x`](https://github.com/hinshun/vt10x)) that turns
   the harness's raw PTY byte stream into queryable screen state.
3. **`pkg/turns/`** — per-harness adapters that translate screen state
   + wrapper status into a small set of chat events (`TurnComplete`,
   `ToolCall`, `Blocked`, `Errored`).
4. **`pkg/chat/`** — the Go-level chat API: `Conversation.Open`,
   `AcquireControl`, `Send`, `Events`, `History`. Storage is pluggable
   via the `Store` interface; `pkg/chat/memstore` ships the in-memory
   default.

Transport layers (HTTP, gRPC, …) are intentionally out of scope and
live in separate binaries that import `pkg/chat`.

```
                ┌──────────────────────────────┐
                │ pkg/chat (Conversation API)  │
                └──────────────┬───────────────┘
                               │
            ┌──────────────────┴──────────────────┐
            │                                     │
   ┌────────▼─────────┐                ┌──────────▼──────────┐
   │ pkg/turns        │                │ pkg/transcript      │
   │  +harness/codex  │                │  +codex             │
   │  +harness/cc     │                │  +claudecode        │
   │  +generic        │                │ (read-only JSONL)   │
   └────────┬─────────┘                └─────────────────────┘
            │
   ┌────────▼─────────┐
   │ pkg/screen       │  vt10x emulator
   └────────┬─────────┘
            │
   ┌────────▼─────────┐
   │ pkg/wrapper      │  PTY supervisor + status classifier
   └──────────────────┘
```

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
        if ev.Turn.ID == turnID && ev.Turn.State == chat.TurnStateComplete {
            break
        }
    }

    history, _ := conv.History(ctx)
    _ = history // [{Role:"user", Text:"summarize this project"}, {Role:"assistant", Text:"..."}]
}
```

See [docs/chat.md](docs/chat.md) for the full library reference.

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

## Supported harnesses

| Harness     | Status detection | Turn detection           | Session ID extraction    | Transcript reader        |
|-------------|------------------|--------------------------|--------------------------|--------------------------|
| codex       | ✅                | ✅ `Token usage:` footer   | ✅ `codex resume <uuid>`   | ✅ `~/.codex/sessions/`    |
| claude-code | ✅                | ✅ `✻ <verb> for Ns` line  | ✅ `claude --resume <uuid>`| ✅ `~/.claude/projects/`   |
| generic     | ✅ (fallback)     | ✅ via `waiting_for_input` | —                        | —                        |

Other harnesses can be supported by implementing `turns.Adapter` (and
optionally the `SessionIDExtractor` / `TranscriptReader` capability
interfaces).

## Layout

- `pkg/wrapper/` — PTY supervisor + Status vocabulary
- `pkg/wrapper/trace/` — diagnostic event vocabulary
- `pkg/screen/` — vt100 emulator wrapper (vt10x per [ADR-001](docs/adr-001-vt100.md))
- `pkg/turns/` — turn-detection interface, `generic` fallback, per-harness adapters under `harness/`
- `pkg/transcript/` — read-only harness JSONL parsers (codex, claudecode)
- `pkg/chat/` — Conversation API, Store interface
- `pkg/chat/memstore/` — in-memory `Store` implementation
- `cmd/harness-wrapper/` — thin CLI front-end for the wrapper
- `internal/screenbench/` — bake-off harness used to choose the vt100 emulator
- `test/corpus/` — recorded byte streams used by the bake-off and the adapter compatibility tests
- `test/fakeharness/mock/` — generic mock harness used by the wrapper test suite
- `docs/` — design notes, ADRs, library reference

## Testing

```sh
go test ./...
```

Per-harness adapter tests double as the **compatibility test suite**:
they replay byte streams from `test/corpus/` and assert that turn
detection still fires correctly. When upstream CLIs change their TUI
markers, these fail loudly — see `test/corpus/README.md` for the
recording workflow.
