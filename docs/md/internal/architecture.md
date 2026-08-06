# Architecture

harness-wrapper is layered: a PTY supervisor at the bottom, a chat API at the top, with two transports
that import the chat layer. Each layer depends only on the one below it, and **data flows upward** —
raw PTY bytes climb the stack and arrive at the top as typed chat events.

![harness-wrapper layered architecture](../diagrams/architecture.svg)

## The four layers

| Layer | Package | Responsibility |
|---|---|---|
| 1 | [`pkg/wrapper`](wrapper.md) | Start a harness under a PTY, stream output, classify the run into a normalized [`Status`](wrapper.md#status). |
| 2 | [`pkg/screen`](screen.md) | A vt100 emulator (vt10x) turning the raw PTY byte stream into a queryable `Snapshot`. |
| 3 | [`pkg/turns`](turns.md) | Per-harness adapters that translate screen + status into turn events; a `Watcher` pumps them. |
| 4 | [`pkg/chat`](../guide/chat.md) | The `Conversation` API: control, `Send`, `Events`, `History`, pluggable `Store`. |

Running alongside layer 3 is [`pkg/transcript`](transcript.md) — read-only parsers for each harness's
own JSONL session log, which `History` prefers over screen-scraped text.

## Data flow

A turn travels up the stack:

1. The harness paints its TUI to the PTY; `pkg/wrapper`'s supervisor reads those bytes continuously.
2. The supervisor copies them to `pkg/screen`, which feeds a vt100 emulator and bumps a generation
   counter on every write.
3. `pkg/turns`' `Watcher` subscribes to screen changes **and** wrapper status events, handing each to
   the per-harness `Adapter`, which emits `TurnComplete` / `Blocked` / `Errored` / `InputRequested`.
4. `pkg/chat` maps those onto `Conversation` turn-state transitions and publishes them on `Events()`.

Meanwhile the wrapper independently classifies *why* the harness is quiet or stopped (idle, cost,
retry, prompt) and emits [`Status`](wrapper.md#status) — both a live signal for the turns layer and a
terminal verdict for the caller.

## Transports stay out of the core

The four packages know nothing about HTTP, framing, or auth. Transports are separate `cmd/` binaries
that import `pkg/chat`:

- [`cmd/harness-chatd`](../guide/gateway.md) — an HTTP + SSE gateway for non-Go clients.
- [`cmd/harness-wrapper`](../guide/cli.md) — a CLI: PTY passthrough, one-shot, and tmux-detached modes.

This keeps the conversation semantics in one place and lets new transports (a future
[daemon](roadmap-v1.md), a gRPC service) reuse them without touching the core.

## Beyond the four layers

The four layers answer "how do we drive a harness?". A second group answers **"how do we run one
job?"** — and each of them is a *consumer* of the stack above, never part of it:

| Package | Question it answers |
|---|---|
| [`pkg/harness`](harness.md) | What can this harness binary do on this run, and how do we get its transcript out? |
| [`pkg/oneshot`](oneshot.md) | Run one turn headlessly and classify the outcome. |
| [`pkg/turnproto`](turnproto.md) | What does one finished turn look like on the wire, in any language? |
| [`pkg/env`](env.md) · `internal/env` | Where does the harness run, and what may it touch? |

The direction is strict: `oneshot → harness → chat → turns → screen · wrapper`. Nothing in the four
layers knows these packages exist, which is why the chat layer can be embedded without dragging in
workspace provisioning, and a workspace can run a turn without linking the chat layer at all.

## Supporting packages

- [`pkg/versions`](versions-drift.md) — the embedded `versions.json` pinning each harness to the
  upstream version its adapter was last verified against.
- [`pkg/discovery`](discovery.md) — "is harness X installed, at what version?" — a probe that runs
  `<binary> --version` with an mtime-keyed cache; `pkg/discovery/models` adds the offline model
  registry and the `/model` picker parser.

A directory-by-directory index, including the test trees and the import rules, is in the
[Repository map](packages.md).

## Design principles

- **The screen is the contract we don't own.** Adapters screen-scrape tools that change between
  releases; the [testing strategy](testing/README.md) and [drift pipeline](versions-drift.md) exist to
  keep that honest.
- **Prefer the harness's own transcript for text.** Screen-scraped reply text is best-effort;
  `pkg/transcript` reads exactly what the model said when a session log is available.
- **Normalize, don't leak.** Callers see one `Status` vocabulary and one turn model, never
  harness-specific markers.
- **Pluggable persistence.** `pkg/chat` stores only metadata via the `Store` interface; bodies live in
  the harnesses' logs.
