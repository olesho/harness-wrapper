# Repository map

Where everything lives, what depends on what, and which page documents it.

![harness-wrapper package map](../diagrams/package-map.svg)

## Entry points

| Path | What it is | Docs |
|---|---|---|
| `cmd/harness-wrapper` | The CLI: transparent passthrough, one-shot `run`, machine-readable `structured-run`, tmux-detached mode | [CLI](../guide/cli.md) |
| `cmd/harness-chatd` | HTTP + SSE gateway exposing `pkg/chat` to non-Go clients | [HTTP Gateway](../guide/gateway.md) |
| `cmd/check-versions` | Offline drift sentry against the npm registry | [Versions & Drift](versions-drift.md) |
| `cmd/fakeharness` | A scriptable stand-in harness — test infrastructure, not a product binary | [Fake Harness](testing/fakeharness.md) |

## Library packages

| Path | Responsibility | Docs |
|---|---|---|
| `pkg/wrapper` | Run a harness under a PTY; classify the run into a normalized `Status`; translate the execution-mode knobs into argv | [Wrapper & Status](wrapper.md) |
| `pkg/wrapper/trace` | Diagnostic event vocabulary (observability only, not a stability surface) | [Trace vs. events](wrapper.md#trace-vs-events) |
| `pkg/screen` | vt100 emulator wrapper turning PTY bytes into a queryable snapshot | [Screen](screen.md) |
| `pkg/turns` | The per-harness `Adapter` contract, capability interfaces, and the `Watcher` | [Turns & Adapters](turns.md) |
| `pkg/turns/generic` | The status-only fallback adapter every other adapter embeds | [Adapter Matrix](../guide/adapters.md#generic) |
| `pkg/turns/harness/*` | TUI adapters: `codex`, `claudecode`, `opencode`, `pi` | [Adapter Matrix](../guide/adapters.md) |
| `pkg/chat` | The `Conversation` API: control, send, events, history, interactive input, permission switching | [Chat API](../guide/chat.md) |
| `pkg/chat/memstore` | The in-memory `Store` implementation | [Store interface](../guide/chat.md#store-interface) |
| `pkg/transcript` | Read-only parsers for harness-owned JSONL logs, and the canonical `Event` | [Transcripts](transcript.md) |
| `pkg/transcript/*` | Per-harness readers: `claudecode`, `codex`, `pi` | [Transcripts](transcript.md#per-harness-logs) |
| `pkg/harness` | Per-harness **capability profiles**, hook installation, transcript acquisition, and `RunTurn` | [Harness profiles & runs](harness.md) |
| `pkg/harness/*` | Profiles: `claude`, `codex`, `opencode`, `pi`; `all` registers them | [Capability matrix](harness.md#capability-matrix) |
| `pkg/oneshot` | One typed turn, headless, with an auto-accept policy | [One-shot turns](oneshot.md) |
| `pkg/turnproto` | The frozen structured-turn wire format and its exit codes | [Structured turn protocol](turnproto.md) |
| `pkg/env` | Host-side client for running a structured turn inside a workspace | [Execution environments](env.md#running-a-turn-in-a-workspace) |
| `pkg/discovery` | Is a harness installed, at what version? | [Discovery](discovery.md) |
| `pkg/discovery/models` | Offline model registry + the `/model` picker parser | [Discovery](discovery.md#which-models-offline) |
| `pkg/versions` | The embedded pins binding each adapter to an upstream release | [Versions & Drift](versions-drift.md) |

## Internal packages

| Path | Responsibility | Docs |
|---|---|---|
| `internal/env` | The environment core: provisioners, containments, `Workspace`, `Compose`, lifecycle, retention | [Execution environments](env.md) · [ADR-003](decisions/adr-003-env-visibility.md) |
| `internal/env/daytona`, `internal/env/openshell` | The shipped provisioner / containment drivers | [Two orthogonal axes](env.md#two-orthogonal-axes) |
| `internal/fakeharness` | Script format and builder for the scriptable real-PTY fake | [Fake Harness](testing/fakeharness.md) |
| `internal/screenbench` | The vt100 emulator bake-off, the live recorder, and a synthetic generator | [ADR-001](decisions/adr-001-vt100.md) · [Corpus](testing/corpus.md) |

## Test and support trees

| Path | Contents |
|---|---|
| `test/corpus/` | Recorded PTY byte streams and screen captures, plus the auth / models / permission-mode / status sub-corpora — see [Corpus](testing/corpus.md) |
| `test/scripts/` | JSON scripts driving the unattended recorder, one per canonical scenario |
| `test/conformance/` | The [cross-language conformance corpus](testing/conformance.md) |
| `clients/` | Reference [Python and TypeScript clients](../guide/clients.md) for the gateway |
| `crossrepo/` | Deliverables staged here but destined for a sibling repo (patch bundles and ticket bodies) |
| `scripts/` | Corpus mirroring and manifest scripts, plus a Node port of the docs generator |
| `docs/md/`, `docs/gen/` | These pages, and the static-site generator that renders them |

## Import direction

The rule is one-way and load-bearing: **a layer may import the one below it, never above.**

```
cmd/*  →  pkg/chat · pkg/harness · pkg/oneshot · pkg/wrapper
pkg/oneshot  →  pkg/harness  →  pkg/chat  →  pkg/turns  →  pkg/screen · pkg/wrapper
pkg/turns  →  pkg/transcript          (for the reader capability's return type)
pkg/env    →  internal/env · pkg/turnproto
```

Consequences worth keeping true:

- **`pkg/transcript` is a leaf.** It imports neither `pkg/turns` nor `pkg/chat`, so anything may parse
  a harness log — including a tool that never starts a harness.
- **`pkg/chat` does not import `pkg/harness`.** The dependency runs the other way: `harness.RunTurn`
  is a *consumer* of the conversation API, not part of it.
- **`pkg/discovery/models` does not import `pkg/discovery`.** The offline registry stays usable
  without the process-probing half.
- **Transports import the core; the core knows nothing about transports.** No package under `pkg/`
  imports `net/http`.

## Nested modules

Two directories are separate Go modules on purpose, so their dependencies never enter the main
module's graph:

| Module | Why |
|---|---|
| `docs/gen` | The docs site generator pulls in a markdown renderer and a syntax highlighter that no library consumer should inherit. |
| `internal/screenbench` | The bake-off compares *several* terminal emulators; only one of them is a real dependency. Its files also carry a build tag, so an ordinary `go build ./...` never touches it. |

## Generated API reference

`docs/MODULES.md` is generated from the AST and lists every module's exported types and functions with
their doc comments. These pages explain *why* and *how*; that file is the authoritative *what*. When
the two disagree about a signature, the generated file is right.
