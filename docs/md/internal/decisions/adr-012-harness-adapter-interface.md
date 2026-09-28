# ADR-012: harness-wrapper implements the Harness Adapter Interface

**Status:** Accepted (2026-09-28)

**Intent:** principle 3, *normalize, don't leak*, principle 5, *keep the stack one-way*, and principle
6, *evolve public contracts deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under
principle 3, everything a runtime needs to know about a harness — its files, flags, hooks, record
and quirks — moves behind one interface, and the runtime above it names no harness. Under principle
5, the chat and supervisor layers split into a core that links one harness at a time, with the
public packages on top. Under principle 6, `pkg/chat` and `pkg/wrapper` keep their API and every
built-in harness, and the interface is a new package beside them.

## Context

agentd runs a coding-agent harness as a **Durable Agent**: an agent whose inputs, their outcomes and
its conversation survive process exits and restarts of the machine it runs on. The target is defined
in three documents:

- [Durable Agents on remote runtimes: concepts and adapters](https://coplan.olehluchkiv.com/d/ai-agents-on-remote-runtimes-concepts-an);
- [Harness Adapter Interface v1](https://coplan.olehluchkiv.com/d/engine-contract-v1-specification), the Go interface between agentd and a harness;
- [Durable Agent API v1](https://coplan.olehluchkiv.com/d/durable-agent-api-v1-specification), between agentd's control plane and a runtime.

Today agentd composes hw's pieces and holds the harness knowledge itself. It renders claude's
`settings.json`, `.claude.json` and MCP config, assembles claude's argv and environment, runs claude's
hooks through a command of its own, follows claude's transcript by its file layout, and converts
`pkg/chat` events. Every agentd binary links every hw screen adapter, because `pkg/chat` picks
adapters with a switch and `pkg/wrapper` picks classifier patterns with another.

The migration plan is
[Migrating to Durable Agents: harness-wrapper and agentd changes](https://coplan.olehluchkiv.com/d/harness-specific-logic-moves-from-agentd).

## Decision

1. **hw implements the Harness Adapter Interface.** Its Go types, generated JSON Schema and registry
   live at `pkg/contract` in this module and follow its releases. The contract imports only the
   standard library, so importing it links nothing else of hw. A conformance kit, a fake Agent
   Adapter plus scenarios, is a separate package beside it.
2. **One Harness Adapter, with a profile per harness.** The shared part holds what the interface
   needs and no harness has to repeat:
   - sessions over the chat core;
   - one observe/ack cursor with stable, kind-prefixed identities and a versioned checkpoint;
   - submission markers, and record access with `recover`;
   - interrupts that name their input;
   - the blocked admission gate.

   A profile holds what differs between harnesses: the Descriptor, `Provision`, the transport, the
   record reader, credential injection, hooks and quirks. Each profile registers under its harness's
   name, so a runtime's harness list picks harnesses one at a time.
3. **Rendering a harness's configuration is in scope.** `Provision` turns a harness-neutral agent
   definition into the harness's files, argv and environment. It is pure: hw renders, and the caller
   writes.
4. **A harness's hook helper ships with its distribution.** `Provision` renders the helper's
   invocation; the helper writes hw's spool, and the adapter reads it back as neutral observations.
   A runtime needs no hook command of its own.
5. **The chat and supervisor layers split into cores.** `internal/chatcore` is `pkg/chat` without any
   screen adapter, and `internal/wrapcore` is `pkg/wrapper` without any classifier patterns. A
   harness is whatever is registered under its name. The per-harness readings the conversation needs
   are optional capabilities of the screen adapter (`pkg/turns`): readiness, dialog state and anchors,
   dialog re-reading, and interstitial dismissal.

   `pkg/chat` and `pkg/wrapper` stay as they are: the same API — every exported identifier is an
   alias of, or forwards to, the core's — and every built-in harness, which they register.
6. **stream-json stays the Claude transport.** The TUI + hooks + transcript hybrid
   (`probes/tui-hybrid`) is not a shipped transport: an interrupt before the first token leaves it no
   trace, and it cannot see API retries.

## Alternatives

- **The contract in a module or repository of its own.** It would get its own version tags, and a
  consumer that implements no harness would not list hw in its `go.mod`. Neither is needed to link
  one harness at a time: Go links the imported package graph, not every package of a module.
  With one implementer (hw) and one caller (agentd), hw's release train is enough; moving the package
  later changes import paths, which type aliases can ease.
- **A complete adapter per harness.** Each would repeat the cursor, the checkpoints, recovery, the
  blocked gate and interrupt targeting. The profile split keeps the per-harness code to what differs,
  as `pkg/chat` already does with its screen adapters.
- **agentd keeps the harness knowledge, and hw only drives conversations.** agentd would carry each
  harness's file formats, flags and hook names, and a second harness would be agentd work. The
  knowledge belongs where the harness is already understood.
- **A build tag to leave the built-in harnesses out of `pkg/chat`.** It would make a Claude-only
  binary depend on every build remembering the tag, and a forgotten tag links everything without an
  error. The cores make the dependency graph say it instead.

## Consequences

- `pkg/contract` and the Harness Adapter are new public API, versioned as the interface specifies
  (`harness-adapter/1.<minor>`).
- The cores are internal: hw's own packages import them; callers outside the module keep `pkg/chat`
  and `pkg/wrapper`. A test compares the `pkg/chat` API with its golden file.
- Code that must link one harness at a time imports the cores. A new exported identifier in a core
  reaches its facade when the facade is regenerated (`go generate` in `pkg/chat` and `pkg/wrapper`).
- A harness-specific reading in the chat core is an optional capability of its screen adapter.
  Harness names remain only where they select wire-level behaviour, such as submit keys and
  permission-mode flags.

## Follow-ups

In the order of the migration plan: the chat split (`internal/chatcore`, `internal/wrapcore`); the
contract package and conformance kit; the Harness Adapter with its Claude Code profile; the Codex
profile over `codex app-server`.
