harness-wrapper supervises CLI agent harnesses — **Claude Code, Codex, OpenCode, pi** — under
a pseudoterminal, classifies their execution state into a small normalized vocabulary, and exposes
them as programmable, multi-turn **chat sessions**. Drive them from Go, from the CLI, or from any
language over HTTP.

## Why it exists

CLI agent harnesses are interactive TUIs, not batch commands. They stall, ask questions, hit rate
limits, and wait for human input. A caller that just wants "send a prompt, get the reply, know when
the turn is done" has to screen-scrape a moving target.

harness-wrapper absorbs that. It runs each harness under a real PTY, watches the rendered screen,
and turns the mess into:

- a **normalized status** — `idle`, `waiting_for_input`, `blocked_by_cost`, `retry_later`,
  `failed`, … — so a run can be retried, paused, or resumed without coupling to a specific CLI;
- a **turn model** — one assistant turn per `Send`, with `complete` / `errored` transitions you can
  subscribe to;
- **history** — read back from the harness's own JSONL transcript when available.

## The shape of it

The repository layers in four steps — a PTY supervisor at the bottom, a chat API at the top — with
two transports (an HTTP gateway and a CLI) that import the chat layer:

![harness-wrapper layered architecture](diagrams/architecture.svg)

## Where to go next

**Using it**

- **[Getting Started](guide/getting-started.md)** — install, build, and drive your first conversation
  from Go, the CLI, and over HTTP.
- **[CLI](guide/cli.md)** — passthrough, one-shot, structured, and tmux-detached modes.
- **[Chat API](guide/chat.md)** — the `pkg/chat` Conversation reference.
- **[HTTP Gateway](guide/gateway.md)** — `harness-chatd` endpoints and wire format.
- **[Client libraries](guide/clients.md)** — the shipped Python and TypeScript clients.
- **[Permissions & sandboxing](guide/permissions.md)** — the rung vocabulary, what it enforces, and
  what it does not.
- **[Adapter Matrix](guide/adapters.md)** — exactly what each harness supports today.
- **[Troubleshooting](guide/troubleshooting.md)** — when a harness stalls, hangs, or won't authenticate.

**How it works**

- **[Architecture](internal/architecture.md)** — how PTY bytes become chat events, layer by layer.
- **[Repository map](internal/packages.md)** — every package, what depends on what.
- **[Wrapper & Status](internal/wrapper.md)** · **[Turns & Adapters](internal/turns.md)** ·
  **[Screen](internal/screen.md)** · **[Transcripts](internal/transcript.md)** — the four layers.
- **[Harness profiles & runs](internal/harness.md)** · **[One-shot turns](internal/oneshot.md)** ·
  **[Structured turn protocol](internal/turnproto.md)** · **[Execution environments](internal/env.md)**
  — running a single job, anywhere.
- **[Discovery](internal/discovery.md)** · **[Versions & Drift](internal/versions-drift.md)** — what is
  installed, and what we verified against.
- **[Testing Tiers](internal/testing/README.md)** — how any of this stays true across upstream releases.
