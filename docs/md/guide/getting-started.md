# Getting Started

harness-wrapper is a Go toolkit. You can consume it three ways, in increasing order of decoupling:

1. **As a Go library** — import `pkg/chat` (conversations) or `pkg/wrapper` (raw supervision).
2. **As a CLI** — `harness-wrapper` supervises a harness under a PTY, or drives one turn headlessly.
3. **Over HTTP** — run `harness-chatd` and drive conversations from any language.

> **You need a harness binary on PATH.** harness-wrapper supervises *other* tools — install at least
> one of `claude`, `codex`, `opencode`, or `pi` first, and make sure it is authenticated (run it once by hand).

## Prerequisites

- **Go 1.25+** (the module targets `go 1.25.0`).
- At least one harness CLI installed and logged in. The version each adapter was last verified against
  is pinned in [`pkg/versions/versions.json`](../internal/versions-drift.md) (currently codex `0.144.5`,
  claude-code `2.1.247`).

## Install

As a library:

```bash
go get github.com/olesho/harness-wrapper/pkg/chat
go get github.com/olesho/harness-wrapper/pkg/wrapper   # supervisor only
```

As CLIs:

```bash
go install github.com/olesho/harness-wrapper/cmd/harness-wrapper@latest
go install github.com/olesho/harness-wrapper/cmd/harness-chatd@latest
```

From a checkout:

```bash
git clone https://github.com/olesho/harness-wrapper
cd harness-wrapper
make build      # go build ./...
make test       # go vet + gofmt + go test -race ./...
```

## 1. Drive a conversation from Go

A `Conversation` owns one PTY-supervised harness process. Acquire control, send a message, watch the
turn complete, read history:

```go
package main

import (
	"context"
	"fmt"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
)

func main() {
	ctx := context.Background()

	conv, err := chat.Open(ctx, chat.Options{
		Harness:    "codex", // or "claude-code", "opencode", "pi", "generic"
		BinaryPath: "/usr/local/bin/codex",
		WorkingDir: "/path/to/project",
		Store:      memstore.New(),
	})
	if err != nil {
		panic(err)
	}
	defer conv.Close(ctx)

	release, err := conv.AcquireControl(ctx) // FIFO token; ctx-cancellable
	if err != nil {
		panic(err)
	}
	defer release()

	turnID, err := conv.Send(ctx, "summarize this project")
	if err != nil {
		panic(err)
	}

	for ev := range conv.Events() {
		if ev.Type == chat.EventTurn && ev.Turn.ID == turnID && ev.Turn.State == chat.TurnStateComplete {
			fmt.Println(ev.Turn.Text)
			break
		}
	}

	history, _ := conv.History(ctx)
	_ = history // [{Role: user, …}, {Role: assistant, Text: "…"}]
}
```

See the full reference in **[Chat API](chat.md)**.

### Just the supervisor

If you don't need turns — you only want to run a harness and learn *why* it stopped — use
`pkg/wrapper` directly:

```go
res, err := wrapper.Run(ctx, wrapper.Config{
	BinaryPath: "/usr/local/bin/claude",
	Args:       []string{"--print", "hello"},
	Stdout:     os.Stdout,
})
// res.Status is wrapper.StatusIdle, StatusBlockedByCost, StatusRetryLater, …
```

See **[Wrapper & Status](../internal/wrapper.md)**.

## 2. Use the CLI

Transparent passthrough — supervise a harness in your own terminal, forwarding stdin/stdout:

```bash
harness-wrapper claude -- --print hello
```

One-shot — drive exactly one turn headlessly. The prompt is read from **stdin**, the reply is printed
to **stdout**, and blocking dialogs are auto-accepted so an unattended run never hangs:

```bash
echo "summarize this project" | harness-wrapper run codex
```

Detached via tmux, then attach later:

```bash
harness-wrapper --tmux-session demo claude --
harness-wrapper attach demo
```

Full subcommand and flag reference: **[CLI](cli.md)**.

## 3. Drive it over HTTP

Run the gateway (localhost only — v1 has no auth):

```bash
go run ./cmd/harness-chatd --bind 127.0.0.1:8080
```

Then open a conversation, acquire control, send, and stream events:

```bash
ID=$(curl -s -XPOST localhost:8080/v1/conversations \
  -d '{"harness":"codex","binary_path":"/usr/local/bin/codex"}' | jq -r .id)

curl -N localhost:8080/v1/conversations/$ID/events &        # SSE stream

TOK=$(curl -s -XPOST localhost:8080/v1/conversations/$ID/control | jq -r .token)
curl -s -XPOST localhost:8080/v1/conversations/$ID/messages \
  -d "{\"token\":\"$TOK\",\"text\":\"hello\"}"
```

The full endpoint table plus ready-to-run Python and TypeScript clients are in
**[HTTP Gateway](gateway.md)**.

## Next steps

- **[Adapter Matrix](adapters.md)** — what each harness supports (turn detection, session-id,
  transcripts, input).
- **[Troubleshooting](troubleshooting.md)** — what to do when a run reports `binary_not_found`,
  `blocked_by_cost`, or stalls.
- **[Architecture](../internal/architecture.md)** — the layers behind all three entry points.
