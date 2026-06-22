# Roadmap v1 — High-Leverage Investments

Three work items identified as the highest-leverage investments in harness-wrapper. Each unlocks real
consumers; each has a concrete smallest-valuable-version. Design decisions are resolved (below);
per-item implementation plans are produced on request, one at a time.

> A fourth item — a persistent `chat.Store` (SQLite) — was dropped: no known consumer is blocked on
> durable Store metadata (harness-chatd is process-bounded, CLI users don't touch `pkg/chat`, the
> future daemon owns wrapper sessions, not chat sessions). Revisit when a concrete consumer asks.

## Decisions made

| Area | Decision |
|---|---|
| Daemon transport (item 3) | **Byte-proxy only.** Daemon owns the PTY master fd; clients connect via Unix socket and read/write bytes through the proxy. No `SCM_RIGHTS`. |
| Daemon lifecycle (item 3) | **Auto-spawn with manual override.** Client connects; on `ECONNREFUSED`, fork-exec a fresh daemon and retry. Idle-exit after a timeout (default 30 min). `harness-wrapperd --foreground` for systemd/launchd. |
| Daemon vs. tmux (item 3) | **Coexist.** tmux stays the simple shell-user path; daemon mode is for programmatic-spawn + later-human-attach. |
| First new adapter (item 1) | **opencode.** |
| Community adapters (item 1) | **Accepted, with PR-template requirements** (corpus scenarios + adversarial recordings + `versions.json` pin + drift check + maintainer sign-off). |
| Event-kind shape (item 2) | **Separate `Kind` values, additive.** Consumers feature-detect by `Event.Kind`; old consumers ignore new kinds. |
| Vocabulary additions (item 2) | **`ThinkingStarted`/`ThinkingCompleted`** + **`ContextUsage`** in scope; **`TurnTextDelta`** and structured **`ToolCall*`** carry over; **`CostDelta` deferred**. |
| Sequencing | adapters first → vocabulary in parallel with remaining adapters → daemon last. |

## 1. More harness adapters

**Goal.** Expand coverage so harness-wrapper is the default supervisor for any meaningful CLI agent —
not just the three currently supported.

Per-adapter workflow: (1) identify the turn-complete signal; (2) session-ID surfacing; (3)
cost/quota/retry/error patterns; (4) transcript JSONL location + schema; (5) implement
`pkg/turns/harness/<name>/`, `pkg/wrapper/internal/harness/<name>/`, and `pkg/transcript/<name>/` (if
applicable); (6) record [corpus](testing/corpus.md) scenarios (canonical + adversarial); (7) wire into
`chat.resolveAdapter` and `harness-chatd`; (8) add a [`versions.json`](versions-drift.md) entry.

**Ordering:** opencode first (stress-tests the onboarding workflow), then cursor / qwen-code / aider
(parallelizable), then others as community PRs land. Community adapters are accepted with a
`CONTRIBUTING-adapter.md` codifying the 8-step workflow, required scenarios, an initial pin, and a
maintainer sign-off.

**Risk** low per adapter; aggregate maintenance grows linearly (mitigated by the
[drift sentry](versions-drift.md)). **Effort** M for opencode, S each thereafter.

## 2. Richer turn vocabulary

**Goal.** Surface more of what the harness is doing — streaming text, structured tool calls, reasoning
indicators, context usage — so consumers can build better UIs.

- **`TurnTextDelta`** — new prompt-region text, diffed snapshot-to-snapshot. Honestly a *best-effort
  approximation* of streaming from TUI bytes, not token-level; consumers needing fidelity read the
  [transcript](transcript.md).
- **`ToolCallStarted` / `ToolCallCompleted` / `ToolCallFailed`** — replaces the single `ToolCall` with
  lifecycle stages + an optional `ToolCallDetail` (`Name`, `Args`, `Status`).
- **`ThinkingStarted` / `ThinkingCompleted`** — for reasoning models (Claude extended thinking, o1 via
  Codex), distinct from text deltas so UIs can render a collapsible block.
- **`ContextUsage`** — scraped from the TUI footer (`Used` / `Total` / `Fraction`).

**Deferred:** `CostDelta` (TUI formats vary wildly; read the transcript for precise accounting);
permission prompts as a distinct kind (covered by `waiting_for_input`); file-edit events (covered by
`ToolCall*`).

Rollout is **additive** — `Event.Kind` gains constants; old consumers' `switch` defaults ignore them.
Not every adapter implements every kind; per-adapter capability is documented in the
[adapter matrix](../guide/adapters.md), and a `VocabularyVersion` constant lets consumers guard logic.
**Risk** medium (inconsistent fidelity across adapters); **Effort** L.

## 3. Attach/daemon path

**Goal.** Let a run started headlessly by one process be driven later by a separate client — see live
output, send input, request stop, query state — without the spawning process staying alive.

A single new binary `harness-wrapperd` plus a `pkg/wrapper/attach` client library. The daemon owns the
PTY master fds; clients connect via a Unix-domain socket and read/write bytes through a proxy.

**v1 surface:** run registry keyed by run-ID; socket at `$XDG_RUNTIME_DIR/harness-wrapper.sock`;
length-prefixed JSON frames with a `protocol_version` handshake; RPCs `Start` / `Attach` (bidirectional
stream) / `Snapshot` / `Stop` / `List`; auto-spawn on socket-missing; idle-exit; a
`harness-wrapper daemon-attach <run-id>` subcommand. Auth is Unix socket file permissions only in v1.

**Why byte-proxy over fd-passing:** cross-platform, enables a future remote daemon, natural
recording/replay, trivial multi-client broadcast. The localhost-socket latency delta is invisible to a
human typing in a TUI.

Explicit edge cases to design: client disconnect mid-stream (run continues, bounded ring buffer, next
attach resumes from snapshot); daemon crash (active runs lost; observe via trace files); run completes
with no client attached (Result held until consumed or idle-timeout); concurrent attach (output
broadcasts, input serialized last-write-wins with a `multi_writer` advisory). **Risk** high (largest
design surface); **Effort** XL.

## Sequencing

opencode adapter (M, workflow stress-test) → cursor/qwen-code/aider (parallelizable) **and**
vocabulary v1 (in parallel) → attach/daemon path (XL, high-risk, last). Daemon is sequenced last so
real-consumer feedback from the adapter work and time for the resolved design to age can surface any
unstated assumptions before code commits to them. It can be promoted if priorities shift — the open
design surface is now small enough that the risk is implementation effort, not architectural rework.

The current next step is **item 1: the opencode adapter.**
