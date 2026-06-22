# Troubleshooting

Most problems surface as a normalized [`Status`](../internal/wrapper.md#status) or a `pkg/chat`
[sentinel error](chat.md#sentinel-errors). This page maps the common symptoms to causes and fixes.

## `binary_not_found`

The configured binary isn't on `PATH` (or at `Options.BinaryPath`). `Run` returns
`StatusBinaryNotFound` with `ExitCode == -1` alongside `wrapper.ErrBinaryNotFound`.

- Pass an absolute `BinaryPath`, or ensure the harness name resolves on `PATH`.
- Check installation with the version probe used by the drift sentry: `claude --version`,
  `codex --version`, etc.

## A run / turn hangs and never completes

Usually the harness is blocked on something the normal flow can't satisfy:

- **Auth wall.** On a fresh machine the harness redirects to an interactive login. The wrapper can't
  type credentials. **Run the harness once by hand** to complete auth, then retry.
- **Trust / permission dialog.** Claude Code blocks on "Do you trust the files in this folder?" (and
  the `--dangerously-skip-permissions` acceptance screen). From the library/gateway, either answer the
  `EventInputRequest` or pre-configure an [`InputPolicy`](chat.md#interactive-input-blocking-prompts):
  ```go
  InputPolicy{ByKind: map[string]Disposition{
      "trust_prompt": {Kind: DispositionAnswer, OptionID: "proceed"},
  }}
  ```
  `harness-wrapper run` and `POST /v1/turns` auto-accept these, so the hang there is almost always the
  auth wall above.
- **`Send` returns `ErrInputPending`.** A dialog is awaiting an answer — call `Answer` (or set a
  policy) before sending.

## `waiting_for_input`

The harness paused at an interactive prompt. This is a **non-terminal** advisory: the harness is still
alive. Answer the prompt (interactive input channel) and it resumes. For harnesses without a turn
marker yet (gemini / opencode / pi), `waiting_for_input` is *also* how the [adapter](adapters.md)
infers turn completion — so a quiet prompt is the expected "turn done" signal there.

## `blocked_by_cost` vs `retry_later` vs `api_error`

These come from the classifier reading known patterns in the harness output:

| Status | Meaning | What to do |
|---|---|---|
| `blocked_by_cost` | Budget / quota / rate-limit hit (terminal). | Record the blocked state; resume when allowed. A `SessionEvent.ResumeAt` carries the reset time when the harness prints one (e.g. Claude's "resets 6:40pm"). |
| `retry_later` | Transient/recoverable error, e.g. a connection reset (terminal). | Back off and retry per your policy; `RetryAfter` carries a hint when parsed. |
| `api_error` | Upstream API returned an error but the harness is **still running** (non-terminal). | Inspect `SessionEvent.HTTPCode` / `Class`; often clears on its own. |

The full error taxonomy (`ErrRateLimited`, `ErrAuth`, `ErrBilling`, …) and HTTP-code mapping is in
[Wrapper & Status](../internal/wrapper.md#errorclass).

## `stale`

No PTY output for `Config.StaleThreshold` (default 5 minutes). `stale` is a **non-terminal advisory** —
it never appears in `Result.Status`, only on `Session.Events()`. The harness may be on a genuinely
long tool call, or wedged. Tune or disable it via `Config.StaleThreshold` (set negative to disable).

## A turn completes too early or too late

Turn boundaries are screen-scraped, so they track the harness's TUI:

- **Too early / truncated reply** — typically a `Busy()` or marker-timing issue. claude-code defers
  completion while the spinner/`esc to interrupt` footer is up; only claude-code has `Busy()`.
- **A new harness version broke detection** — the markers drifted. Run `make check-versions`; if it
  reports drift, follow the [upgrade playbook](../internal/versions-drift.md).

## Chat-level errors

| Error | Cause | Fix |
|---|---|---|
| `ErrNoControl` | `Send`/`Answer` without the token. | `AcquireControl` first (FIFO; it may queue). |
| `ErrTurnInFlight` | A prior assistant turn is still pending/streaming. | Wait for its `complete`/`errored` event. |
| `ErrUnknownHarness` | `Options.Harness` isn't registered. | Use `codex` / `claude-code` / `gemini` / `opencode` / `pi` / `generic`. |
| `ErrClosed` | Method called after `Close`. | Open a new `Conversation`. |

Over HTTP these map to status codes (e.g. `ErrNoControl`/`ErrInputPending` → 409, `ErrUnknownHarness`
→ 400) in the `{error, code}` body.

## Diagnosing deeper

Turn on [trace](../internal/wrapper.md#trace-vs-events) to see the wrapper's internal observations
(`wrapper_started`, `output_quiet`, `harness_classified`, `harness_exited`, …):

```bash
harness-wrapper --trace-stderr claude -- --print hello
```

Trace is diagnostic only — don't make control-flow decisions on it; use `Status`/events for that.
