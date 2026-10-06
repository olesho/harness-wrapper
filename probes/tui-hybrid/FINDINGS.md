# Hybrid claude driver: TUI for input, hooks + transcript for state

**Probed:** claude 2.1.283, macOS arm64, 2026-09-27, against `mockapi.py` (copied from
agentd `probes/p11`; placeholder token). Interactive `claude --session-id <uuid>
--dangerously-skip-permissions` on a 120x40 PTY, no `-p`. Every hook event below is
registered in `$CLAUDE_CONFIG_DIR/settings.json` and logged by `hook.py`; the transcript
JSONL is tailed every 20 ms. Keys: text then `\r`; interrupt a lone `\x1b`.

**Gaps re-probed:** claude 2.1.284, 2026-10-06, with `gaps.py`, which adds claude's debug log
(`--debug-file`) and OpenTelemetry logs as side channels (*Closing the gaps*).

Rerun: `python3 mockapi.py 18712 <w>/mock &`, then `python probe.py <w> main exhaust stall early retry queue`
and `python gaps.py <w> retry exhaust stall early midtext` (both need `pyte`). Timelines land in
`<w>/<session>/timeline.jsonl`.

Question: can a TUI-driven claude report turn state without reading the screen?
**Yes, on claude 2.1.284.** Normal turns, API failures, tools, streaming text and interrupts after
the first token all have a hook or transcript signal. The two gaps the first probe found, an
interrupt before the first token leaving no trace and API retries being invisible, close with
claude's own debug log (`--debug-file`). It records every turn's start and end, each cancel,
and each failed API attempt as they happen (*Closing the gaps*).

## Signals per fact

| Fact | Hook | Transcript | Timing (mock) | Verdict |
|---|---|---|---|---|
| Process up | `SessionStart` `{source:"startup", model}` | — | 0.25–0.55 s after launch | Pass. The composer still has to render; onboarding, trust and bypass dialogs were pre-seeded away (`.claude.json`, `skipDangerousModePermissionPrompt`). |
| Prompt accepted (idle) | `UserPromptSubmit` `{prompt, prompt_id}` | `user` record with `promptId`, `promptSource` | hook 40–50 ms after Enter; record 100–250 ms | **Pass.** The exact prompt text plus a fresh `prompt_id` works as a submission receipt. |
| Prompt sent while busy | `UserPromptSubmit` fires **at enqueue** and carries the **running turn's** `prompt_id` | `queue-operation` `{content}` at enqueue; when dequeued, a `user` record with a **new** `promptId` | — | Caveat. The receipt means "queued", not "started". It matters only if hw sends while busy, and agentd keeps one input in flight. |
| Streaming text | `MessageDisplay` `{turn_id, message_id, index, final, delta}` | — | flushes whole lines; a one-line reply flushes once, at the end | Pass, coarse. |
| Tool activity | `PreToolUse`, `PostToolUse`, `PostToolBatch` with `tool_use_id`, input and response | `assistant` `tool_use` / `user` `tool_result` | Pre ≈ 40 ms after submit | Pass. Richer than the TUI driver, which reports no tools today. |
| Turn completed | `Stop` `{last_assistant_message, background_tasks, session_crons}` | `assistant` `stop_reason:"end_turn"`, then `system/stop_hook_summary`, then **`system/turn_duration`** | the hook arrives 60–100 ms before the records are on disk (the ADR-011 lag) | **Pass.** `turn_duration` is a clean end-of-turn record in the transcript. |
| API error, retries exhausted | **`StopFailure`** `{error:"server_error", last_assistant_message}`. The error enum includes `rate_limit`, `overloaded`, `authentication_failed`, `billing_error`, `max_output_tokens`, … | `assistant` `isApiErrorMessage:true`, `error:"server_error"`, then `turn_duration` | — | **Pass.** A typed failure, and the usage wall is `rate_limit`. |
| API retry in progress | nothing | nothing | the 529 retry recovered in 2 s | **Pass, through the debug log:** `API error (attempt k/N)` for each failed request, live, while the turn stays open. No retry delay (*Closing the gaps*). Hooks and the transcript still show nothing. |
| Interrupt mid-text | **no** `Stop`, **no** `StopFailure` | partial `assistant` (`stop_reason:null`, key `isAbortedMidStream`), then `user` `[Request interrupted by user]` with the same `promptId`; no `turn_duration` | about 200 ms after Esc | **Pass.** The transcript record is the acknowledgement. |
| Interrupt mid-tool | none | `tool_result` "The user…", then `[Request interrupted by user for tool use]` | about 200 ms | Pass. |
| Esc 50 ms after Enter | none | the prompt, `slow0 ` partial, then the interrupt record | — | Pass. The early-Esc hazard from ADR-007 did not reproduce once the prompt was submitted. |
| **Interrupt before the first token** (Esc 1 s into a stalled request) | none | **nothing new.** The prompt's `user` record stays in the file, and the next prompt's `parentUuid` skips it (claude rewound). | debug log 80–95 ms after Esc | **Pass, through the debug log:** `[onCancel]`, then `[engine] turn N end (… stop=null)` (*Closing the gaps*). Claude also puts the prompt back in the composer, and the next typed text would be appended to it (`STALL 30PING 4` was sent as one prompt), so the adapter sends Ctrl-U after every interrupt. |
| Session id | on every hook | file name | — | Pass (`--session-id`). |
| Quit | `SessionEnd` `{reason:"prompt_input_exit"}` | `cost-state` | Ctrl-C twice | Pass. |

Every hook also carries a `prompt_id`, the key that ties a prompt to its turn's events.
It is absent before the first prompt.

## What that means for a hybrid transport

What still needs the screen or the keyboard:

- **Typing prompts and Enter.** Submission is confirmed by `UserPromptSubmit` matching
  the prompt text, not by the typed text echoing on screen.
- **Esc to interrupt.** Afterwards, the debug log's `[onCancel]` and `[engine] turn N end`
  settle the turn, and the interrupt record, when there is one, carries the partial reply.
  Then always send Ctrl-U, so an interrupt before the first token can't leave a restored
  prompt to be merged into the next one.
- **Startup dialogs.** Pre-seed config so they never appear. Keep hw's dialog detector as
  a guard that fails loudly rather than answering them.

What no longer needs the screen:

- **Turn start:** `UserPromptSubmit`, or the `user` record's `promptId`.
- **Turn end:**
  - `Stop` means completed; its text is `last_assistant_message`, confirmed by `turn_duration`.
  - `StopFailure` means errored, and `error` maps straight to hw's API-error classes.
  - An interrupt record means interrupted, with the partial reply kept.
  - The debug log's `[onCancel]` then `[engine] turn N end (stop=null)` without an interrupt
    record means interrupted before the first token. The outcome is `interrupted`, since the
    request may have reached the model, and claude has withdrawn the prompt from the conversation.
- **Retrying:** each `API error (attempt k/N)` line in the debug log, until the turn ends.
- **Busy:** from the first `UserPromptSubmit` until one of those end signals.
- **Tools and streaming text:** Pre/PostToolUse and MessageDisplay.

Compared with stream-json:

- Weaker:
  - interrupts and retries rest on a debug log, whose format claude doesn't document;
  - a retry carries no delay;
  - a queued prompt's receipt carries the wrong turn id;
  - no `can_use_tool` (untested: the `PermissionRequest` hook can return allow/deny and may cover it).
- Same: transcript, session id, resume, hooks, MCP.
- Stronger than today's TUI driver: every turn boundary is a structured record, so there
  is no idle timer or `✻` marker to wait on.

## Closing the gaps: claude's debug log

**Re-probed:** claude 2.1.284, 2026-10-06, with `gaps.py`. Each scenario ran in a fresh session,
three times. Two side channels were added:
- `--debug-file <path>`, tailed as it is written;
- OpenTelemetry logs (`CLAUDE_CODE_ENABLE_TELEMETRY=1`, exported as OTLP/HTTP JSON to a local
  receiver).

The debug log carries the turn lifecycle of claude's `[engine]`, which the TUI's turns run
through (it logs `yield system/init`, as the SDK protocol does):

| Debug line | Meaning |
|---|---|
| `[engine] turn N start` | A turn began |
| `[ERROR] API error (attempt k/N): <status> <body>` | One model request failed, and the turn stays open. `N` is the retry limit plus one |
| `[onCancel] source=local streamMode=…` | claude processed an interrupt |
| `[ERROR] Error in API request: Request was aborted.` | The interrupt aborted a request in flight |
| `[engine] turn N end (… stop=<reason> resultLen=<n>)` | The turn ended. `stop` is `end_turn`, `stop_sequence` (an API error) or `null` (interrupted) |
| `[ERROR] [engine] turn ended in error: <message>` | An errored or interrupted end, with claude's message |

**Interrupts.**
- Before the first token (3 runs): `[onCancel]` 80–95 ms after Esc, with `[engine] turn 1 end … stop=null` in the same moment, and no interrupt record.
- 50 ms after Enter, and mid-text (6 runs): `[onCancel]` 65–212 ms after Esc, the turn's end within 212 ms, and an interrupt record every time.

So every interrupt now settles on positive evidence:
- turn end with an interrupt record: `interrupted`, and the partial reply stays in the conversation;
- turn end without one: also `interrupted`, since the request may have reached the model. claude has withdrawn the prompt from the conversation and put it back in the composer, and the adapter clears it with Ctrl-U.

**Retries** (3 runs each): every failed request is logged as it happens.
- A turn that recovers: `1/11, 1/11`, then `stop=end_turn` and `Stop`.
- A turn that exhausts its retries (`CLAUDE_CODE_MAX_RETRIES=2`): `1/3, 1/3, 2/3, 3/3`, then `stop=stop_sequence` and `StopFailure`.

claude retries its first failure at once without advancing its counter, hence the repeated `1/N`. The log carries no retry delay, so a `retrying` observation has `attempt`, `max` and `http_status`, but no `delay_ms`.

**Correlation.** Debug lines carry no prompt or session id. The adapter attributes them by order, which holds because it keeps one input in flight and the log belongs to one process. A turn claude starts on its own, after background work, also logs `[engine] turn N start`; the adapter would attribute it by its `UserPromptSubmit`, or report a turn without an input. That case is not probed yet.

**Operating the log.**
- **Rotation:** claude reopens the path for each write. After a rename, new lines go to a fresh file at the same path, so the adapter can rotate: rename, read the old file to its end, continue on the new one.
- **Volume:** about 40 KB for a short session, mostly startup. `--debug <categories>` did not narrow what the file receives.
- **Content:** no credential and no prompt text appeared; API error bodies do.
- **Banner:** the TUI shows "Debug mode enabled · logging to …".

**OpenTelemetry** corroborates but can't replace the log. `user_prompt` and `api_request` carry `prompt.id`, but `api_error` arrived for some failed attempts only (attempts 1 and 3 of 3, never 2), followed by `api_retries_exhausted`. No event marks a cancel.

## Costs and risks

- **The debug log is not a documented interface.** Its lines can change in any release. The adapter pins claude, parses only the six lines above, keeps them in the conformance kit's fixtures, and refuses to run when the log shows no `[engine] turn N start` for its first turn. That is the same kind of gate stream-json applies through `system/init` capabilities.
- **Hook delivery is live in hw now.** hw's Claude Code profile (`pkg/adapter/claudecode`,
  v0.30.0) already reads the transcript and the hook spool as its record, through the
  observe cursor. A hybrid transport would reuse that and add the debug log as a live source.
- **One process per hook.** Every hook starts a process. MessageDisplay fires per line
  flush, so a long reply means many processes. Register it only when streaming is wanted,
  and use a small compiled hook binary (agentd's).
- **Installing hooks.** hw writes a project `.claude/settings.json`. The probe used
  `$CLAUDE_CONFIG_DIR/settings.json`, and `--settings <file>` would avoid touching the
  worktree. Check how settings sources merge for hooks.
- **Version coupling moves** from rendered text to hook schemas. `StopFailure`,
  `MessageDisplay`, `prompt_id` and `last_assistant_message` are recent. Check them per
  pinned version, the way ADR-0002 gates on `system/init` capabilities: at SessionStart,
  confirm the payload fields exist and refuse to run otherwise.
- **Not probed yet:**
  - real account;
  - Linux;
  - a real usage wall (`StopFailure` `rate_limit`?);
  - `PermissionRequest` answering;
  - subagents;
  - compaction (`PreCompact`/`PostCompact`);
  - `--resume`;
  - an idle session over 60 s (`Notification` idle_prompt);
  - a Stop hook that blocks.
