# ADR-012: harness-wrapper implements the Harness Adapter Interface

**Status:** Accepted (2026-09-28); amended 2026-10-06, 2026-10-09

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
2. **One Harness Adapter, with a profile per harness** (`pkg/adapter`). The shared part holds what
   the interface needs and no harness has to repeat:
   - sessions over a profile's transport;
   - one observe/ack cursor with stable, kind-prefixed identities and a versioned checkpoint;
   - submission markers, and record access with `recover`;
   - interrupts that name their input;
   - the blocked admission gate.

   A profile holds what differs between harnesses: the Descriptor, `Provision`, the transport, the
   record reader, credential injection, hooks and quirks. Each profile registers under its harness's
   name, so a runtime's harness list picks harnesses one at a time.

   A profile's transport speaks its harness's protocol itself. The Claude Code profile drives
   stream-json directly rather than through `pkg/chat`'s Conversation: the interface wants a native
   id per input, `cancelled` only on positive evidence, and retries reported one by one, which the
   Conversation keeps to itself, and exposing them would widen `pkg/chat`'s API. The profile shares
   hw's transcript follower, its hook handler and spool, and its reset-time parser.
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
   alias of, or forwards to, the core's — and every built-in harness, which they register. So does
   `pkg/harness`, over `internal/harnesscore`: the core is everything but `Run` and `RunTurn`, which
   drive a harness through `pkg/chat`, so a harness's hook profile links no chat.
6. **stream-json stays the Claude transport.** The TUI + hooks + transcript hybrid
   (`probes/tui-hybrid`) is not shipped, although it can report what the interface needs: on claude
   2.1.283 and 2.1.284, on macOS and Linux, against the mock API and a real account. Hooks and the
   transcript give turns, tools, failures and receipts. claude's debug log
   (`--debug-file`) gives the rest: each interrupt (`[onCancel]`, then `[engine] turn N end`),
   including one before the first token, and each failed API attempt (`API error (attempt k/N)`).
   Those two facts would rest on a log whose format claude doesn't document, while stream-json
   carries them as frames, with a retry's delay that the log lacks. A hybrid transport, if built,
   pins claude (and holds it to the pin under a strict version policy, ADR-023), keeps the debug
   lines it parses in the conformance fixtures, and refuses to start
   when its first turn logs no `[engine] turn` line. It reads the log's turn lines only while one
   turn is in flight: when two overlap, claude can log them, and fire their hooks, out of order.

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
  reaches its facade when the facade is regenerated (`go generate` in `pkg/chat`, `pkg/wrapper` and
  `pkg/harness`).
- A harness-specific reading in the chat core is an optional capability of its screen adapter.
  Harness names remain only where they select wire-level behaviour, such as submit keys and
  permission-mode flags.

## Follow-ups

In the order of the migration plan: the chat split (`internal/chatcore`, `internal/wrapcore`); the
contract package and conformance kit; the Harness Adapter with its Claude Code profile; the Codex
profile over `codex app-server`. Then, **planned**, the TUI hybrid as a second transport of the
Claude Code profile (Decision 6): agentd's fallback for when stream-json is unavailable or breaks,
behind the same interface, so agentd adds no TUI code of its own (agentd ADR 0002 and 0004, 2026-10-07).

## History

- 2026-10-09: the TUI hybrid's phase 3: `claude-code-tui` declares `tools_observed`, `subagents` and
  `background_turns`, as `claude-code` does. Tools and subagents come from the hook spool through
  the shared record. A prompt the `UserPromptSubmit` hook binds to no input starts a turn of
  claude's own — a task notification's named `task-<task id>`, as the record names it — and `Stop`'s
  `background_tasks` and the notification's task report the background work live. claude's TUI runs
  every subagent in the background, so the input's turn ends at its launch and claude takes its
  result up in a turn of its own (`probes/tui-hybrid`, *Phase 3*: macOS and Linux). Prompts stay
  undeclared, as an owner decision: the `PermissionRequest` hook's `allow` and `deny` decide and it
  may block for minutes, but claude's TUI draws its own dialog meanwhile, falls back to it, needing
  keystrokes, when the hook answers nothing, and neither Claude profile has a posture that prompts.
  Rate-limit reports, streaming text and side-by-side Sessions come later.

- 2026-10-09: the TUI hybrid's phase 2: `claude-code-tui` interrupts a turn and reports retries.
  `Interrupt` presses Esc and is confirmed by the debug log's `[onCancel]`; the turn's end with an
  interrupt's stop reason (`null`, `tool_use`) ends it `interrupted`, before the first token too,
  and Ctrl-U clears the prompt claude puts back in its composer. Each `API error (attempt k/N)`
  claude retries is a `retrying` observation, and the Descriptor declares `retry_visible`. The kit's
  `interrupt` scenarios run. Prompts, claude's own turns, tools and subagents observed and
  side-by-side Sessions come later.

- 2026-10-09: interface 1.7 adds a version policy ([ADR-023](adr-023-harness-version-policy.md)):
  `claude-code-tui` no longer refuses a claude other than the pin unless the agent's
  `version_policy` is `strict`; under `flexible`, the default, its `[engine]` gate alone decides.
  Every profile reports the version it runs.

- 2026-10-07: the TUI hybrid's phase 1 lands as a harness of its own, `claude-code-tui`
  (`pkg/adapter/claudecodetui`), beside `claude-code`, which is unchanged: open and reopen, turns
  that complete or error, the shared record, Close. It pins claude, keeps the debug lines it parses
  in a fixture, gates the first turn on `[engine] turn N start`, and reads turn lines only while one
  turn is in flight. Interrupts, retries, prompts and claude's own turns come in later phases.

- 2026-10-07: the TUI hybrid moves from optional to planned, as agentd's Claude fallback. agentd's
  earlier fallback, hw's TUI driver behind `pkg/chat`, has been unreachable there since agentd moved
  onto this interface (its ADR 0004); its ADRs 0002 and 0004 now name the hybrid instead.

- 2026-10-06: the TUI hybrid's two gaps, an interrupt before the first token and retries in
  progress, close on claude's debug log (`probes/tui-hybrid`: claude 2.1.283 and 2.1.284, macOS
  and Linux, the mock API and a real account). stream-json stays the Claude transport, for the
  reasons Decision 6 now gives.
