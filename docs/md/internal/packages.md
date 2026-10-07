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
| `cmd/claude-code-hook` | The Claude Code profile's hook helper: claude's hooks run it from the harness distribution, and it writes the spool | [Claude Code profile](contract.md#the-claude-code-profile) |

## Library packages

| Path | Responsibility | Docs |
|---|---|---|
| `pkg/wrapper` | Run a harness under a PTY; classify the run into a normalized `Status`; translate the execution-mode knobs into argv. `internal/wrapcore` plus every built-in harness's classifier patterns | [Wrapper & Status](wrapper.md) |
| `pkg/wrapper/trace` | Diagnostic event vocabulary (observability only, not a stability surface) | [Trace vs. events](wrapper.md#trace-vs-events) |
| `pkg/screen` | vt100 emulator wrapper turning PTY bytes into a queryable snapshot | [Screen](screen.md) |
| `pkg/turns` | The per-harness `Adapter` contract, capability interfaces, and the `Watcher` | [Turns & Adapters](turns.md) |
| `pkg/turns/generic` | The status-only fallback adapter every other adapter embeds | [Adapter Matrix](../guide/adapters.md#generic) |
| `pkg/turns/harness/*` | TUI adapters: `codex`, `claudecode`, `opencode` | [Adapter Matrix](../guide/adapters.md) |
| `pkg/chat` | The `Conversation` API: control, send, events, history, interactive input, permission switching. `internal/chatcore` plus every built-in screen adapter | [Chat API](../guide/chat.md) |
| `pkg/chat/memstore` | The in-memory `Store` implementation, written against the chat core so it links no harness | [Store interface](../guide/chat.md#store-interface) |
| `pkg/transcript` | Read-only parsers for harness-owned JSONL logs — whole-file readers and a checkpointed follower — and the canonical `Event` | [Transcripts](transcript.md) |
| `pkg/transcript/*` | Per-harness readers: `claudecode`, `codex`, `pi` | [Transcripts](transcript.md#per-harness-logs) |
| `pkg/harness` | Per-harness **capability profiles**, hook installation, transcript acquisition, and `RunTurn` | [Harness profiles & runs](harness.md) |
| `pkg/harness/*` | Profiles: `claude`, `codex`, `opencode`; `all` registers them | [Capability matrix](harness.md#capability-matrix) |
| `pkg/oneshot` | One typed turn, headless, with an auto-accept policy | [One-shot turns](oneshot.md) |
| `pkg/turnproto` | The frozen structured-turn wire format and its exit codes | [Structured turn protocol](turnproto.md) |
| `pkg/discovery` | Is a harness installed, at what version? | [Discovery](discovery.md) |
| `pkg/discovery/models` | Offline model registry + the `/model` picker parser | [Discovery](discovery.md#which-models-offline) |
| `pkg/versions` | The embedded pins binding each adapter to an upstream release | [Versions & Drift](versions-drift.md) |
| `pkg/contract` | The Harness Adapter Interface: the types, `Adapter` / `Session` / `Record`, the registry and the generated JSON Schema; standard library only | [Harness Adapter Interface](contract.md) |
| `pkg/contract/conformance` | The interface's conformance kit: a fake Agent Adapter and scenarios in P11's prompt language | [Running the kit](contract.md#running-the-kit) |
| `pkg/contract/fakeadapter` | An in-process harness's adapter: the kit's reference, and a stand-in for a runtime's tests | [Harness Adapter Interface](contract.md) |
| `pkg/adapter` | hw's Harness Adapter: sessions, the observe/ack cursor, submission markers and record access over a per-harness profile | [Harness Adapter](contract.md#harness-wrappers-harness-adapter) |
| `pkg/adapter/claudecode` | The Claude Code profile: stream-json transport, `Provision`, the transcript and hook spool as the record | [Claude Code profile](contract.md#the-claude-code-profile) |
| `pkg/adapter/codex` | The Codex profile: app-server transport, `Provision`, the thread's rollout as the record | [Codex profile](contract.md#the-codex-profile) |
| `pkg/adapter/pi` | The Pi profile: RPC transport, `Provision`, the session's file as the record, one `api_key` across providers | [Pi profile](contract.md#the-pi-profile) |

## Internal packages

| Path | Responsibility | Docs |
|---|---|---|
| `internal/wrapcore` | `pkg/wrapper` without any classifier patterns; `harness/*` holds each harness's patterns, `detector` their matcher | [Architecture](architecture.md#cores-that-link-one-harness-at-a-time) · [ADR-012](decisions/adr-012-harness-adapter-interface.md) |
| `internal/chatcore` | `pkg/chat` without any screen adapter: a harness is whatever adapter is registered under its name | [Architecture](architecture.md#cores-that-link-one-harness-at-a-time) · [ADR-012](decisions/adr-012-harness-adapter-interface.md) |
| `internal/harnesscore` | `pkg/harness` without `Run` and `RunTurn`: the profile registry, hook specs, the hook handler and its spool, the settings.json merge. The per-harness hook profiles build on it, so they link no chat | [Architecture](architecture.md#cores-that-link-one-harness-at-a-time) · [Harness profiles & runs](harness.md) |
| `internal/mockapi` | A mock of a harness's model API — the Messages API, a Go port of agentd's P11 mock, and the Responses API — for driving a real claude, codex or pi through scripted scenarios | [Claude Code profile](contract.md#the-claude-code-profile) · [Codex profile](contract.md#the-codex-profile) · [Pi profile](contract.md#the-pi-profile) |
| `internal/procgroup` | Starts a harness in a process group of its own, signals the group, and tells when it is empty | [Harness Adapter](contract.md#harness-wrappers-harness-adapter) |
| `internal/facadegen` | Writes `pkg/chat`'s, `pkg/wrapper`'s and `pkg/harness`'s forwarding declarations from their cores (`go generate`) | — |
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
pkg/oneshot  →  pkg/harness  →  pkg/chat  →  internal/chatcore  →  pkg/turns  →  pkg/screen · internal/wrapcore
pkg/chat     →  pkg/turns/harness/* · pkg/turns/generic · pkg/wrapper   (the built-ins it registers)
pkg/wrapper  →  internal/wrapcore · internal/wrapcore/harness/*          (the built-ins it registers)
pkg/harness  →  internal/harnesscore · pkg/chat · pkg/wrapper
pkg/harness/*  →  internal/harnesscore · pkg/transcript/*                (a hook profile links no chat)
pkg/adapter/claudecode  →  pkg/adapter · internal/harnesscore · pkg/harness/claude · pkg/transcript/claudecode
pkg/adapter/codex       →  pkg/adapter · pkg/transcript/codex
pkg/adapter/pi          →  pkg/adapter · pkg/transcript/pi
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
- **`pkg/contract` imports only the standard library** (`TestStandardLibraryOnly`), so a runtime links
  the interface without any of hw's harness code.
- **The cores name no harness.** `internal/chatcore` and `internal/wrapcore` import no screen adapter,
  transcript reader or classifier pattern set; a harness is what is registered under its name. A
  binary that imports a core and one harness's adapter links no other harness
  (`TestLinks_OneHarnessAtATime`). `internal/harnesscore` imports no chat, so one harness's hook
  profile links neither chat nor another harness (`TestLinks_NoChatOneHarness`).

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
