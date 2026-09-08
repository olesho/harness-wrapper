# CLI

`cmd/harness-wrapper` is a thin front-end over [`pkg/wrapper`](../internal/wrapper.md) and
[`pkg/chat`](chat.md). It has four modes: **transparent passthrough**, **one-shot**, its structured
sibling **`structured-run`**, and a **tmux-backed** detached path with `attach` / `status` / `kill` /
`list` subcommands.

```bash
go install github.com/olesho/harness-wrapper/cmd/harness-wrapper@latest
```

Supported harness names today: `codex`, `claude`, `opencode`, `pi`.

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

## Structured one-shot: `structured-run`

The machine-readable sibling of `run`, for host orchestrators. It drives exactly one turn the same
way, but instead of printing the plain reply it emits **exactly one JSON object** — a
`turnproto.StructuredTurnResult` — as the **last line on stdout**, and exits with the protocol exit
code (`0` completed, `124` deadline, `1` errored / startup error). Hosts parse the last stdout line
(`turnproto.ParseLastJSONLine`), so harness banner or log noise printed before it is fine.

```bash
echo "do the thing" | harness-wrapper structured-run claude --
harness-wrapper structured-run --prompt-file /path/prompt.txt claude --
```

The prompt comes from stdin, or from `--prompt-file <path>` — the host upload transport, so a prompt
with quotes, newlines, or leading dashes can never corrupt argv. The same
`HARNESS_WRAPPER_RUN_TIMEOUT` deadline applies. The Go host-side client for this subcommand is
`pkg/env.RunStructuredTurn`, which mirrors this invocation shape (including `--sandbox-defaults` via
`StructuredTurnConfig.SandboxDefaults`).

### The reported `permission_mode`

The result carries an optional `permission_mode`: the **canonical rung the turn was launched at**
(`plan` | `manual` | `ask` | `auto` | `bypass`), resolved from the final launch arguments plus the
requested `--permission-mode` — never a harness-native spelling like `acceptEdits` or `read-only`.

The promise is **one-directional**:

- **Presence of `bypass` is trustworthy** for a turn that reached the harness. Every unrestricted
  launch path reports it, including the ones that carry no canonical `--permission-mode` at all:
  `--sandbox-defaults` (which injects `--dangerously-skip-permissions`), a raw
  `--dangerously-skip-permissions` after `--`, and codex's `-s danger-full-access` in every spelling.
- **Absence never means "safe."** An absent key means no canonical rung could be *named*: the harness
  default, an unsupported harness, a codex argv setting only the `-a` approval axis, or a
  present-but-unreadable flag. Claude's `dontAsk` is no longer one of these causes: it reports the
  `manual` rung.
- **A restrictive rung is a launch argument, not a gate.** On this path claude's per-tool permission
  dialog is not detected at all (so `plan` / `manual` / `ask` stall an unattended turn to the
  deadline) and codex approvals are auto-answered, so only codex's `-s` sandbox axis is actually
  enforced. Do not read `"permission_mode": "manual"` off a structured run as "this turn was
  supervised" — see [Runtime enforcement per path](../internal/wrapper.md).
- **`startup_error` never carries it.** No harness was launched, so there is no rung to report;
  `deadline` and `errored` *do* carry it, because those turns reached the harness.

It reports the argv half only: the `IS_SANDBOX=1` environment half that `--sandbox-defaults` also
sets is not representable in a rung string and is not reflected here.

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
| `--model ID` | Model id for harnesses that support it (`claude --model`, `codex -c model`). |
| `--auto-accept` | `run` only: auto-answer blocking prompts (affirmative) even with a terminal attached, instead of asking the human. |
| `--sandbox-defaults` | `run` and `structured-run` only; **dangerous**. For `claude`, injects `--dangerously-skip-permissions` into the harness args and sets `IS_SANDBOX=1` in the harness env (parity with meta-harness; see the [wrapper spec note](../internal/wrapper.md#sandbox-defaults-injection)). No-op for every other harness. The default passthrough mode **rejects** it with an error — an interactive session should make that policy call in the harness itself. |
| `--permission-mode RUNG` | Launch-time permission posture for `claude` / `codex`: `plan`, `manual`, `ask`, `auto`, `bypass` (per-harness native spellings also pass through). Accepted in **every** mode **including passthrough**, unlike `--sandbox-defaults` — see the composition rule below. `plan` is **rejected** for `codex` (no launch-time flag exists; use `/plan` after launch). Unsupported for `opencode` and `pi`. |

`--effort` / `--model` reach the same per-harness translation as the gateway's `effort` / `model`
fields (via `wrapper.Start` / `wrapper.Run`), so behaviors 1, 3 and 4 of
[the gateway's `effort` and `model` semantics](gateway.md#effort-and-model-semantics) hold here
verbatim: `--effort` is validated and hard-fails while `--model` is silently dropped on a harness
that has none, an explicit `--effort`/`--model` (or codex `-c` key) in the harness args wins over
the flag, and codex remaps `max` → `xhigh`. **Behavior 2 differs**: this CLI's harness registry is
`codex`, `claude`, `opencode`, `pi`, and it rejects `claude-code` outright — so its effort-capable
names are `codex` and `claude`, not the gateway's `codex` and `claude-code`. (`run` maps `claude` →
`claude-code` internally before `chat.Open` sees it, so the two never disagree about which harness
runs — only about which name you type.)

### `--permission-mode` rungs

The canonical rungs are harness-independent; the wrapper translates each one into the harness's own
launch-time flags before the harness starts:

| Rung | `claude` / `claude-code` | `codex` |
|---|---|---|
| `plan` | `--permission-mode plan` | rejected (no launch-time flag; use `/plan` after launch) |
| `manual` | `--permission-mode manual` | `-s read-only -a untrusted` |
| `ask` | `--permission-mode acceptEdits` | `-s workspace-write -a on-request` |
| `auto` | `--permission-mode auto` | `-s workspace-write -a never` |
| `bypass` | `--permission-mode bypassPermissions` | `-s danger-full-access -a never` |

**`--permission-mode bypass` is not a drop-in for `--sandbox-defaults`.** `--sandbox-defaults`
contributes two halves — the args half (`--dangerously-skip-permissions`) and the env half
(`IS_SANDBOX=1`, which is what allows running as **root** and suppresses claude-code's "Bypass
Permissions mode" acceptance screen). The `bypass` rung delivers only the args half, so with `bypass`
alone the acceptance screen comes back and root is disallowed.

**Composition rule.** The two flags compose in exactly one combination: `--sandbox-defaults
--permission-mode bypass` (or `... bypassPermissions`) is accepted, with `--sandbox-defaults`
contributing the env half only. `--sandbox-defaults` paired with **any other** mode — including
codex's native `danger-full-access` — exits 2 with:

```
harness-wrapper: --sandbox-defaults is incompatible with --permission-mode <mode> (only --permission-mode bypass composes with it)
```

In an interactive `run` the acceptance screen reaches the human on the tty; **`--auto-accept`** is the
escape hatch that answers it unattended. Reach for that rather than `--sandbox-defaults` unless you
actually want root semantics.

> Restrictive rungs (`plan`, `manual`, `ask`) are fully enforced only when a human is at the TUI
> (passthrough, or `run` from a terminal for codex). Under `structured-run` and unattended `run`,
> claude's permission dialogs are not detected (the turn stalls to the deadline) and codex's approval
> prompts are auto-approved (only the `-s` sandbox axis still binds).

See the [wrapper spec](../internal/wrapper.md#sandbox-defaults-injection) for the per-path
enforcement table and the known limitations behind that caveat.

`--tmux-child` is an internal in-pane re-exec marker; never set it by hand. `-h` / `--help` / `help`
print usage.

## Which mode do I want?

- **Watch and interact yourself** → passthrough (`harness-wrapper <name> --`).
- **Scripted single reply** → `run` (stdin → stdout).
- **Machine-parsed single turn from a host orchestrator** → `structured-run` (JSON result line).
- **Start now, attach later, from a shell** → `--tmux-session` + `attach`.
- **Programmatic multi-turn from another language** → not the CLI; run the
  [HTTP gateway](gateway.md) instead.

The CLI's flags and routes are frozen as golden snapshots (Layer 0 in the
[testing tiers](../internal/testing/README.md)), so they change only on an intentional edit.
