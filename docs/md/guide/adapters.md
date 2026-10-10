# Adapter Matrix

Each harness is integrated through a per-harness [`turns.Adapter`](../internal/turns.md) (turn
detection on the rendered screen) plus an optional [transcript reader](../internal/transcript.md) and
wrapper-level classifier. Capabilities vary by how much of the harness's TUI and on-disk format we've
mapped. This page is the honest, code-grounded snapshot of **what works today**.

| Harness | Status | Turn detection | Session-ID | Transcript | Interactive input | Permission knob | Permission detect |
|---|:--:|---|:--:|---|:--:|:--:|:--:|
| **codex** | ✅ | ✅ `Token usage:` footer | ✅ | ✅ `~/.codex/sessions/` | ✅ startup interstitials | ✅ ¹ | ✅ ² |
| **claude-code** | ✅ | ✅ `✻ <verb> for Ns` | ✅ | ✅ `~/.claude/projects/` | ✅ trust / bypass | ✅ | ✅ |
| **opencode** | ✅ | ⏳ via `waiting_for_input` | ⏳ | ❌ format in flux | — | — | — |
| **generic** | ✅ | — maps wrapper status | — | — | — | — | — |

**Legend** — ✅ implemented · ⏳ partial / pending a real on-screen marker (turn completion falls back
to the wrapper's `waiting_for_input` signal) · ❌ not yet / deferred · — not applicable.

**Permission knob** — whether the harness accepts a launch-time permission rung, from
`wrapper.harnessSupportsPermissionMode`. **Permission detect** — whether the adapter implements
[`turns.PermissionModeDetector`](../internal/turns.md#capability-interfaces), i.e. can read a
permission posture back off the rendered screen. These are two independent facts, decided in two
different packages by two different functions: "accepts a launch rung" (`pkg/wrapper`) and "can read
the posture back" (`pkg/turns`). They happen to cover the same harnesses today; do not collapse them
or describe either as "the same set as" the other column.

¹ codex rejects the `plan` rung — it has no launch-time flag for it (use `/plan` after launch); every
other rung is accepted. ² The two detectors do not report the same *kind* of value — claude-code
returns a canonical rung, codex returns a COLLABORATION-axis value that is not a rung. See the
[capability table](../internal/turns.md#capability-interfaces).

The row keys are **adapter** names. The CLI and the gateway spell the harness differently: see the
CLI's harness registry in [cli.md](cli.md) (which takes `claude`, not `claude-code`) and the
gateway's adapter lookup in [gateway.md](gateway.md) (which requires `claude-code`).

Pinned & verified upstream versions live in [`versions.json`](../internal/versions-drift.md): codex
`0.160.0` (verified 2026-10-05), claude-code `2.1.296` (verified 2026-10-10).
opencode is unpinned pending corpus capture.

pi has no row: it is no harness of this layer. The Harness Adapter's
[Pi profile](../internal/contract.md#the-pi-profile) drives it over its RPC mode, at pi `1.0.4`.

## codex

- **Turn detection** keys on the end-of-turn `Token usage: total=… input=… (+ … cached) output=…`
  footer. The chat layer completes a turn the instant a fresh footer appears (codex has no spinner /
  `Busy()` model — it dedupes by exact footer text).
- **Session-ID**: scraped from the on-screen `codex resume <uuid>` hint → resume with `codex resume <uuid>`.
- **Transcript**: JSONL under `~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ts>-<uuid>.jsonl`.
- **Interactive input**: startup interstitials (update-available notice, model-migration screen) are
  detected and auto-dismissed unless `Options.DisableCodexAutoDismiss` is set. Genuine **approval
  dialogs** — shell-command (`Would you like to run the following command?`) and apply-patch
  (`Would you like to make the following edits?`) — are detected as kind `approval_prompt` and are
  **never** auto-dismissed: they surface as a structured input request and hold the session
  not-ready until answered. Recognition requires the anchor *plus* a proceed row, a deny row, and
  the live `›` selector on a menu row parsed from the anchor tail, so quoted prose cannot
  false-positive (a false positive would deadlock the turn). Pinned by
  `test/corpus/codex/approval-command` and `approval-patch` (codex-cli 0.144.4).

## claude-code

The most fully-featured adapter.

- **Turn detection** keys on the `✻ <verb> for Ns` thinking-summary line (verbs like *Baked*,
  *Pondered*, *Cooked*; multi-unit durations like `1m 22s` after a turn ≥ 60s).
- **`Busy()`** reads the `esc to interrupt` footer + spinner so the chat layer never reports
  `complete` mid-turn (Claude streams in multiple parts: thinking → edit → tool run).
- **Message extraction** isolates the `⏺ …` reply blocks (`● …` on Linux) from TUI chrome —
  important for clean one-shot output.
- **Session-ID**: assigned at launch with `--session-id <uuid>` → resume with `claude --resume <uuid>`.
- **Transcript**: JSONL under `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`.
- **Interactive input**: folder-trust prompt and `--dangerously-skip-permissions` bypass-acceptance,
  with a numbered-menu parser (`proceed` / `deny` aliases). **Graceful quit** via the `/quit` command.
- **Interrupt**: Esc (as `CSI 27 u`) stops a turn mid-reply or mid-tool, or cancels it before its
  first token; the adapter reads which, per turn, and chat ends the turn `interrupted`
  ([Interrupting a turn](chat.md#interrupting-a-turn)). No other adapter can interrupt yet.

## opencode

This adapter detects **status** (via the wrapper's cost/quota/prompt/API patterns), but it has no
confirmed on-screen turn-completion marker yet, so turn boundaries fall back to the wrapper's
`waiting_for_input` signal (lower fidelity: no intermediate work detection). Transcript reading is
**deferred**: the on-disk store is migrating from per-message JSON files to SQLite, and shipping a
reader that silently breaks across that migration is worse than none.

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
