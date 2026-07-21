# Glossary

Domain terms used across the docs and code, grouped by layer. Cross-references point to the page where
each is defined in full.

## Supervision

- **Harness** — a CLI agent tool harness-wrapper supervises: Claude Code, Codex, OpenCode, pi.
  Interactive TUIs, not batch commands.
- **PTY** — pseudoterminal. Each harness runs under one (`creack/pty`) so it behaves as if attached to
  a real terminal. The PTY byte stream is the canonical source for output capture and state detection.
- **Wrapper** — [`pkg/wrapper`](wrapper.md): the supervisor that starts a harness under a PTY, streams
  output, and classifies the run.
- **Session** (`wrapper.Session`) — the live handle to a supervised run: `Wait`, `Stop`, `Events`,
  `WriteStdin`, `AttachOutput`, …. Distinct from a `chat.Session` (conversation metadata).
- **Status** — the normalized run-state vocabulary (`idle`, `waiting_for_input`, `blocked_by_cost`,
  `retry_later`, `api_error`, `stale`, `failed`, `interrupted`, `unknown`, `binary_not_found`).
  [Terminal](wrapper.md#status) statuses end the run; non-terminal ones are mid-run advisories.
- **Classifier** — the component that turns recent output + timing into a `Status`, as a
  [four-stage gated pipeline](wrapper.md#how-classification-works). Resolved per-harness from
  `Config.Harness`.
- **ErrorClass** — a stable error taxonomy (`RateLimited`, `Auth`, `Billing`, `Transient`, …) attached
  to status events, independent of harness wording. See [Wrapper](wrapper.md#errorclass).
- **Trace** — diagnostic-only observations (`wrapper_started`, `output_quiet`, …). **Not** an API
  surface; never drive control flow on it — use `Status`/events.
- **Effort** — reasoning-effort hint (`low`…`max`) passed to harnesses that support it.

## Screen & turns

- **Screen / Snapshot** — [`pkg/screen`](screen.md): a vt100 emulator turning PTY bytes into a
  queryable `Snapshot` (rendered text + cursor + a monotonic `Generation`).
- **Adapter** (`turns.Adapter`) — the per-harness contract: `OnScreen` + `OnWrapperStatus` → turn
  [events](turns.md). Optional capabilities: `SessionIDExtractor`, `TranscriptReader`, `Quitter`,
  `MessageExtractor`, `BusyDetector`.
- **Marker** — the on-screen string an adapter keys on for turn completion (claude-code `✻ Verb for Ns`;
  codex `Token usage:`). The fragile, version-dependent part — see [drift](versions-drift.md).
- **`Busy()`** — an adapter capability reporting "still working" vs "idle at the prompt", so the chat
  layer never reports `complete` mid-turn. Only claude-code implements it.
- **Watcher** (`turns.Watcher`) — composes a `Session` + `Screen` + `Adapter` into one
  [event stream](turns.md#the-watcher) via a status pump and a screen pump.
- **Generic adapter** — the fallback that maps wrapper `Status` straight to turn events; embedded by
  every real adapter for the status half of the contract.

## Conversations

- **Conversation** — [`pkg/chat`](../guide/chat.md): one PTY-supervised harness driven as a multi-turn
  chat (`Open`, `Send`, `Events`, `History`, `Close`).
- **Turn / TurnState** — one user or assistant message. Assistant turns flow `pending → streaming →
  complete | errored`. Exactly one assistant turn per `Send`.
- **Control token** — the chat-level [FIFO mutex](../guide/chat.md#control-acquisition) from
  `AcquireControl`; required to `Send`/`Answer`. Distinct from the wrapper-level writer lock the
  Conversation holds for its lifetime.
- **Event** (`ConversationEvent`) — the typed envelope on `Events()`: `turn` / `input_request` /
  `input_resolved`.
- **InputRequest / Answer / Policy** — the [interactive-input channel](../guide/chat.md#interactive-input-blocking-prompts)
  for blocking dialogs (e.g. folder-trust). The client answers semantically (option id/alias); the chat
  layer owns the keystrokes. An `InputPolicy` auto-answers unattended runs.
- **Store** — the pluggable metadata persistence interface; `memstore` is the in-process default. Holds
  metadata only — bodies live in the harness's own log.
- **Transcript** — [`pkg/transcript`](transcript.md): read-only parsers for a harness's own JSONL
  session log, preferred by `History` over screen-scraped text.

## Testing & drift

- **Corpus** — recorded PTY byte streams under [`test/corpus/`](testing/corpus.md), replayed through
  adapters (Layer 2).
- **Adversarial recording** — a negative corpus case where the assistant echoes a marker shape; the
  regex must **not** fire (`TestAdapter_AdversarialNoFire`).
- **Fake harness** — [`cmd/fakeharness`](testing/fakeharness.md): a real, scriptable binary spawned
  over a real PTY to exercise the watcher's wall-clock timing (Layer 3). Not a mock.
- **Sentinel round-trip** — the highest-value invariant: a unique token in the prompt must reappear
  verbatim in the captured reply (catches truncation + extraction drift).
- **Drift** — an upstream release shifting a marker/schema and breaking detection. Caught by the
  [drift pipeline](versions-drift.md).
- **Sentry** — the `cmd/check-versions` drift check (formerly `upstream-version-sentry`): compares `versions.json` pins against the npm registry.
- **Pin / `versions.json`** — the per-harness upstream version each adapter was last verified against.
- **Quiescence** — the timing logic that defers turn completion until output settles, defended by the
  Layer-3 [fake-harness](testing/fakeharness.md) tests.
