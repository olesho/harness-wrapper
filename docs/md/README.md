harness-wrapper supervises CLI agent harnesses — **Claude Code, Codex, Gemini, OpenCode, pi** — under
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

- **[Getting Started](guide/getting-started.md)** — install, build, and drive your first conversation
  from Go, the CLI, and over HTTP.
- **[Chat API](guide/chat.md)** — the `pkg/chat` Conversation reference.
- **[HTTP Gateway](guide/gateway.md)** — `harness-chatd` endpoints + Python / TypeScript / curl clients.
- **[Adapter Matrix](guide/adapters.md)** — exactly what each harness supports today.
- **[Architecture](internal/architecture.md)** — how PTY bytes become chat events, layer by layer.
