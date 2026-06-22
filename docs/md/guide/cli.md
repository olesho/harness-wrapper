# CLI

`cmd/harness-wrapper` is a thin front-end over [`pkg/wrapper`](../internal/wrapper.md) and
[`pkg/chat`](chat.md). It has three modes: **transparent passthrough**, **one-shot**, and a
**tmux-backed** detached path with `attach` / `status` / `kill` / `list` subcommands.

```bash
go install github.com/olesho/harness-wrapper/cmd/harness-wrapper@latest
```

Supported harness names today: `codex`, `claude`, `gemini`.

## Transparent passthrough

Supervise a harness in your own terminal — the wrapper allocates a PTY, forwards your keystrokes and
the harness's output unchanged, and emits trace events on the side. Everything after `--` goes to the
harness verbatim:

```bash
harness-wrapper claude -- --print hello
harness-wrapper codex --
```

Because your terminal is already on the PTY, this mode needs no attach concept. When both stdin and
stdout are real TTYs the wrapper auto-enables raw mode and SIGWINCH forwarding, restoring terminal
state on exit.

## One-shot: `run`

Drive exactly one turn and exit — useful for scripts and CI. The prompt is read from **stdin**, the
final assistant reply is printed to **stdout**, and the process exits non-zero on error:

```bash
echo "summarize this project" | harness-wrapper run codex
echo "explain main.go" | harness-wrapper run claude -- --model sonnet
```

- Blocking dialogs (e.g. Claude Code's folder-trust prompt) are **auto-accepted** so an unattended
  run never hangs. See [Interactive input](chat.md#interactive-input-blocking-prompts).
- The run has a default timeout of **15 minutes**, overridable via the `HARNESS_WRAPPER_RUN_TIMEOUT`
  environment variable (any Go duration, e.g. `5m`, `30s`).

## Detached (tmux)

For shell users who want to start a run, walk away, and reconnect later, the CLI ships a tmux-backed
path. Spawn the run inside a detached tmux session named `hw-<NAME>` and return immediately:

```bash
harness-wrapper --tmux-session demo claude --
```

Then manage it with the subcommands:

| Command | Effect |
|---|---|
| `harness-wrapper attach <name>` | Re-exec into the tmux session (live terminal). |
| `harness-wrapper status <name> [--json]` | Report whether the session is alive. |
| `harness-wrapper kill <name>` | Terminate the session. |
| `harness-wrapper list` | List all `hw-*` sessions. |

> The programmatic, cross-process daemon (`harness-wrapperd`) is **future work** — see the
> [Roadmap](../internal/roadmap-v1.md) (item 3). Today's `attach` targets tmux, not a daemon.

## Flags

Wrapper flags go *before* the harness name:

| Flag | Meaning |
|---|---|
| `--trace-file PATH` | Write diagnostic [trace](../internal/wrapper.md#trace-vs-events) events as NDJSON to `PATH`. |
| `--trace-stderr` | Write trace events as NDJSON to stderr. |
| `--tmux-session NAME` | Spawn the run inside a detached tmux session `hw-<NAME>` and exit. |
| `--effort LEVEL` | Reasoning effort: `low`, `medium`, `high`, `xhigh`, `max` (passed to harnesses that support it). |

`--tmux-child` is an internal in-pane re-exec marker; never set it by hand. `-h` / `--help` / `help`
print usage.

## Which mode do I want?

- **Watch and interact yourself** → passthrough (`harness-wrapper <name> --`).
- **Scripted single reply** → `run` (stdin → stdout).
- **Start now, attach later, from a shell** → `--tmux-session` + `attach`.
- **Programmatic multi-turn from another language** → not the CLI; run the
  [HTTP gateway](gateway.md) instead.

The CLI's flags and routes are frozen as golden snapshots (Layer 0 in the
[testing tiers](../internal/testing/README.md)), so they change only on an intentional edit.
