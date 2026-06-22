# Adapter Matrix

Each harness is integrated through a per-harness [`turns.Adapter`](../internal/turns.md) (turn
detection on the rendered screen) plus an optional [transcript reader](../internal/transcript.md) and
wrapper-level classifier. Capabilities vary by how much of the harness's TUI and on-disk format we've
mapped. This page is the honest, code-grounded snapshot of **what works today**.

| Harness | Status | Turn detection | Session-ID | Transcript | Interactive input |
|---|:--:|---|:--:|---|:--:|
| **codex** | ✅ | ✅ `Token usage:` footer | ✅ | ✅ `~/.codex/sessions/` | ✅ startup interstitials |
| **claude-code** | ✅ | ✅ `✻ <verb> for Ns` | ✅ | ✅ `~/.claude/projects/` | ✅ trust / bypass |
| **gemini** | ✅ | ⏳ via `waiting_for_input` | ❌ | ✅ `~/.gemini/tmp/<project>/chats/` | — |
| **opencode** | ✅ | ⏳ via `waiting_for_input` | ⏳ | ❌ format in flux | — |
| **pi** | ✅ | ⏳ via `waiting_for_input` | ❌ | ✅ `~/.pi/agent/sessions/` | — |
| **generic** | ✅ | — maps wrapper status | — | — | — |

**Legend** — ✅ implemented · ⏳ partial / pending a real on-screen marker (turn completion falls back
to the wrapper's `waiting_for_input` signal) · ❌ not yet / deferred · — not applicable.

Pinned & verified upstream versions live in [`versions.json`](../internal/versions-drift.md): codex
`0.141.0`, claude-code `2.1.185` (verified 2026-06-21). gemini / opencode / pi are unpinned pending
corpus capture.

## codex

- **Turn detection** keys on the end-of-turn `Token usage: total=… input=… (+ … cached) output=…`
  footer. The chat layer completes a turn the instant a fresh footer appears (codex has no spinner /
  `Busy()` model — it dedupes by exact footer text).
- **Session-ID**: scraped from the on-screen `codex resume <uuid>` hint → resume with `codex resume <uuid>`.
- **Transcript**: JSONL under `~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ts>-<uuid>.jsonl`.
- **Interactive input**: startup interstitials (update-available notice, model-migration screen) are
  detected and auto-dismissed unless `Options.DisableCodexAutoDismiss` is set.

## claude-code

The most fully-featured adapter.

- **Turn detection** keys on the `✻ <verb> for Ns` thinking-summary line (verbs like *Baked*,
  *Pondered*, *Cooked*; multi-unit durations like `1m 22s` after a turn ≥ 60s).
- **`Busy()`** reads the `esc to interrupt` footer + spinner so the chat layer never reports
  `complete` mid-turn (Claude streams in multiple parts: thinking → edit → tool run).
- **Message extraction** isolates the `⏺ …` reply blocks from TUI chrome — important for clean
  one-shot output.
- **Session-ID**: `claude --resume <uuid>` hint → resume with `claude --resume <uuid>`.
- **Transcript**: JSONL under `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`.
- **Interactive input**: folder-trust prompt and `--dangerously-skip-permissions` bypass-acceptance,
  with a numbered-menu parser (`proceed` / `deny` aliases). **Graceful quit** via double Ctrl-C.

## gemini, opencode, pi

These adapters detect **status** (via the wrapper's cost/quota/prompt/API patterns) and, where the
on-disk format is stable, **read transcripts** — but they do not yet have a confirmed on-screen
turn-completion marker, so turn boundaries currently fall back to the wrapper's `waiting_for_input`
signal (lower fidelity: no intermediate work detection).

- **gemini** — transcript reader implemented (dual-shape JSONL under `~/.gemini/tmp/<project>/chats/`);
  session-ID extraction is a stub (Gemini uses user-chosen `/chat save <tag>` rather than a visible
  UUID).
- **opencode** — transcript reading is **deferred**: the on-disk store is migrating from per-message
  JSON files to SQLite, and shipping a reader that silently breaks across that migration is worse than
  none.
- **pi** — transcript reader implemented (JSONL v3 under `~/.pi/agent/sessions/`); session-ID
  extraction pending an identified on-screen marker.

## generic

The safety net. `generic` ignores the screen and maps wrapper [`Status`](../internal/wrapper.md#status)
transitions straight to turn events — `waiting_for_input → TurnComplete`,
`blocked_by_cost / retry_later / api_error → Blocked`, `failed / interrupted → Errored`. Every other
adapter embeds it for the `OnWrapperStatus` half of the contract.

> A few harnesses also have a **wrapper-level classifier** without a full turn adapter (e.g. `cursor`),
> giving normalized status but no turn model.

## Adding a harness

The per-adapter workflow — identify the turn-complete marker, session-ID surfacing, cost/quota
patterns, and transcript schema; implement the adapter + classifier + reader; record corpus scenarios;
wire it in; pin the version — is documented in the [Turns & Adapters](../internal/turns.md) internals
and sequenced in the [Roadmap](../internal/roadmap-v1.md) (item 1: more adapters, opencode next).
