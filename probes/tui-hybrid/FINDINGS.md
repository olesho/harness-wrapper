# Hybrid claude driver: TUI for input, hooks + transcript for state

**Probed:** claude 2.1.283, macOS arm64, 2026-09-27, against `mockapi.py` (copied from
agentd `probes/p11`; placeholder token). Interactive `claude --session-id <uuid>
--dangerously-skip-permissions` on a 120x40 PTY, no `-p`. Every hook event below is
registered in `$CLAUDE_CONFIG_DIR/settings.json` and logged by `hook.py`; the transcript
JSONL is tailed every 20 ms. Keys: text then `\r`; interrupt a lone `\x1b`.

**Gaps re-probed:** claude 2.1.283 (hw's pin) and 2.1.284, on macOS arm64 and Ubuntu 26.04 arm64
(Lima), 2026-10-06, with `gaps.py`,
which adds claude's debug log (`--debug-file`) and OpenTelemetry logs as side channels
(*Closing the gaps*, *Test matrix*), and on a real account (*Real account*).

Rerun: `python3 mockapi.py 18712 <w>/mock &`, then `python probe.py <w> main exhaust stall early retry queue`
and `python gaps.py <w>` (all eight scenarios; both need `pyte`). `CLAUDE_BIN` picks the claude
binary, `gaps.py` appends one verdict per scenario to `<w>/results.jsonl`, and `DIGEST=1` prints each
timeline. Timelines land in
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

**Re-probed:** claude 2.1.283 (hw's pin) and 2.1.284, on macOS arm64 and Ubuntu 26.04 arm64,
2026-10-06, with `gaps.py`. Each scenario ran in a fresh session, three times per version and
platform; *Test matrix* has every run. Both versions log the same lines on both platforms. Two side channels were added:
- `--debug-file <path>`, tailed as it is written;
- OpenTelemetry logs (`CLAUDE_CODE_ENABLE_TELEMETRY=1`, exported as OTLP/HTTP JSON to a local
  receiver).

The debug log carries the turn lifecycle of claude's `[engine]`, which the TUI's turns run
through (it logs `yield system/init`, as the SDK protocol does):

| Debug line | Meaning |
|---|---|
| `[engine] turn N start` | A turn began |
| `[ERROR] API error (attempt k/N): <status> <body>` | One model request failed, and the turn stays open. `N` is the retry limit plus one |
| `[onCancel] source=local streamMode=…` | claude processed an interrupt. `streamMode` is `responding`, or `tool-use` while a tool runs |
| `[ERROR] Error in API request: Request was aborted.` | The interrupt aborted a request in flight |
| `[engine] turn N end (… stop=<reason> resultLen=<n>)` | The turn ended. `stop` is `end_turn`, `stop_sequence` (an API error), `null` (a reply interrupted) or `tool_use` (a running tool interrupted) |
| `[ERROR] [engine] turn ended in error: <message>` | An errored or interrupted end, with claude's message |

**Interrupts** (12 runs each: two versions, two platforms):
- Before the first token: `[onCancel]` 53–108 ms after Esc, `[engine] turn 1 end … stop=null` within 109 ms, and no interrupt record.
- 50 ms after Enter: the turn's end within 249 ms. Whether claude had received the first chunk, and so writes an interrupt record, depends on timing (9 of 12 did).
- Mid-text: the turn's end within 163 ms, with an interrupt record.
- Mid-tool (`sleep 30`): `[onCancel]` 58–107 ms after Esc with `streamMode=tool-use`, the turn's end 75–796 ms after Esc (339–796 ms on macOS, 75–152 ms on Linux) with `stop=tool_use`, an interrupt record, and no `sleep` left running.

So every interrupt now settles on positive evidence:
- turn end with an interrupt record: `interrupted`, and the partial reply stays in the conversation;
- turn end without one: also `interrupted`, since the request may have reached the model. claude has withdrawn the prompt from the conversation and put it back in the composer, and the adapter clears it with Ctrl-U.

**Retries** (12 runs each): every failed request is logged as it happens.
- A turn that recovers: `1/11, 1/11`, then `stop=end_turn` and `Stop`.
- A turn that exhausts its retries (`CLAUDE_CODE_MAX_RETRIES=2`): `1/3, 1/3, 2/3, 3/3`, then `stop=stop_sequence` and `StopFailure`.

claude retries its first failure at once without advancing its counter, hence the repeated `1/N`. The log carries no retry delay, so a `retrying` observation has `attempt`, `max` and `http_status`, but no `delay_ms`.

**Correlation.** Debug lines carry no prompt or session id. The adapter attributes them by order, which holds because the log belongs to one process and every turn logs its own start and end:
- **A prompt sent while a turn runs** (12 runs): its `UserPromptSubmit` fires at once, with the running turn's `prompt_id`, and the transcript records a `queue-operation`. The debug log then shows the running turn's end and a second `[engine] turn N start` for the queued prompt, each ending in its own `Stop`.
  - In 1 of 12 runs (Linux, claude 2.1.283) the two turns finished out of order on every channel: the queued prompt's `Stop` arrived before the running turn's, and the debug log wrote `turn 2 end` before `turn 1 end`, and `turn 2 start` after both, by its own clock.
  - Hooks carry `prompt_id`, so they stay attributable; the debug log's turn lines do not. The adapter therefore never sends while a turn runs, as the interface already requires, and attributes debug lines only while a single turn is in flight.
- **A turn claude starts after background work** (`BG sleep 3; echo bg-done`, 12 runs): the input's turn ends (`Stop`, `TOOL DONE`); when the command finishes, claude starts a turn of its own. It logs `[engine] turn 2 start`, fires `UserPromptSubmit` with the notification as its prompt (`<task-notification>…`), and records the notification as a user entry. Its `Stop` carries the answer (`BG DONE`). The prompt's `<task-notification>` prefix is how the adapter tells such a turn from an input's.

**Operating the log.**
- **Rotation:** claude reopens the path for each write. After a rename, new lines go to a fresh file at the same path, so the adapter can rotate: rename, read the old file to its end, continue on the new one.
- **Volume:** about 40 KB for a short session, mostly startup. `--debug <categories>` did not narrow what the file receives.
- **Content:** no credential and no prompt text appeared; API error bodies do.
- **Banner:** the TUI shows "Debug mode enabled · logging to …".

**OpenTelemetry** corroborates but can't replace the log. `user_prompt` and `api_request` carry `prompt.id`, but `api_error` arrived for some failed attempts only (attempts 1 and 3 of 3, never 2), followed by `api_retries_exhausted`. No event marks a cancel.

## Test matrix

2026-10-06, against `mockapi.py`; three runs per scenario, version and platform, each in a fresh session. A run passes when its turn settles on the evidence above. Linux is Ubuntu 26.04 arm64 in Lima (`agentd-ubuntu`).

| Scenario | macOS, 2.1.283 | macOS, 2.1.284 | Linux, 2.1.283 | Linux, 2.1.284 |
|---|---|---|---|---|
| Interrupt before the first token (`STALL`) | 3/3 | 3/3 | 3/3 | 3/3 |
| Interrupt 50 ms after Enter | 3/3 | 3/3 | 3/3 | 3/3 |
| Interrupt mid-text | 3/3 | 3/3 | 3/3 | 3/3 |
| Interrupt mid-tool | 3/3 | 3/3 | 3/3 | 3/3 |
| Retry that recovers | 3/3 | 3/3 | 3/3 | 3/3 |
| Retries exhausted | 3/3 | 3/3 | 3/3 | 3/3 |
| Prompt sent while a turn runs | 3/3 | 3/3 | 3/3, one out of order | 3/3 |
| Turn claude starts after background work | 3/3 | 3/3 | 3/3 | 3/3 |
| `probe.py`'s isolated sessions (exhaust, stall, early, retry, queue) | the September findings hold | the same | the same | the same |

The real account's runs are under *Real account*.

## Real account

2026-10-06, claude 2.1.284 on macOS arm64, Haiku, a `claude setup-token` token. `REAL_TOKEN_FILE` makes claude reach the real API, with the token read from the file into claude's environment only. The real API can't be made to stall or fail on demand, so the `real_*` scenarios use ordinary prompts, and retries stay covered by the mock.

| Scenario | Evidence |
|---|---|
| A plain turn | `[engine] turn 1 end … stop=end_turn`, `Stop` with `PONG` |
| Esc 0.3 s after Enter | before the first token: `[onCancel]` and the turn's end 59–112 ms after Esc, `stop=null`, no interrupt record and no assistant entry (2 runs) |
| Esc mid-text, 1 s after the first displayed line | the turn's end 87–136 ms after Esc, `stop=null`, the interrupt record with the partial reply (2 runs) |
| Esc mid-tool (`sleep 30`) | `[onCancel]` 60–112 ms after Esc, the turn's end 348–822 ms after Esc with `stop=tool_use`, the interrupt record, and the tool's process gone 2 s after Esc (3 runs) |
| A prompt sent while a turn runs | 2 engine turns, in order; the queued prompt's `UserPromptSubmit` carries the running turn's `prompt_id`; a `queue-operation` record (2 runs) |
| The turn claude starts after background work | the input's turn ends; when the command finishes, claude's own turn logs `[engine] turn 2 start`, fires `UserPromptSubmit` with `<task-notification>…`, and records the notification (2 runs) |

Every run settled on the same evidence as against the mock. The token appeared in none of the runs' artifacts: debug logs, transcripts, hook logs, OpenTelemetry records, raw terminal output and configuration.

Two probe faults surfaced here, both fixed:
- `probe.py` named the transcript's directory by replacing only `/` and `.` in the working directory. claude replaces every non-alphanumeric character with `-`, and the real scenarios' directories contain `_`, so the first real run read no transcript.
- The mid-tool check counted every `sleep 30` on the machine, including another session's 10-minute-old `sleep 30; gh run watch …`. It now counts only processes started during the scenario.

## Phase 3: subagents, background work and PermissionRequest

**Probed:** claude 2.1.283 (hw's pin), macOS arm64 and Ubuntu 26.04 arm64 (Lima, `agentd-ubuntu`),
2026-10-09, against `internal/mockapi`, with `phase3_test.go` and `scenarios_test.go`: claude's TUI
on a pseudo-terminal as the profile runs it (no `-p`, `--debug-file`), every hook registered, and
the test binary as the hook command, which can block `PermissionRequest` until the probe answers.
Four runs per scenario and platform. Rerun: `HW_REAL_CLAUDE=… HW_TUI_PROBE_RUNS=4
HW_TUI_PROBE_OUT=<dir> go test -count=1 -v ./probes/tui-hybrid/` (`HW_TUI_PROBE_LONG=1` runs the
11-minute case alone).

### Subagents (`AGENT PING 7`): 8 runs, 8 pass

| Signal | Evidence |
|---|---|
| `PreToolUse` | `tool_name: Agent`, the call's input, its `tool_use_id` |
| `SubagentStart` | `agent_id`, `agent_type: general-purpose`, the parent's `prompt_id` |
| `PostToolUse` | `tool_response.isAsync: true`, `status: async_launched`, `agentId`; before `SubagentStart` in 4 runs, after it in 4 |
| `SubagentStop` | the same `agent_id`, `last_assistant_message: PONG 7`, `agent_transcript_path`, and `background_tasks` listing the subagent still `running` |
| Transcript | `<session>/subagents/agent-<agent_id>.jsonl` |
| The input's turn | ends at `Stop` with `TOOL DONE: Async agent launched…` |
| Then | `UserPromptSubmit` with `<task-notification><task-id><agent_id></task-id>…<result>PONG 7</result>`, its own `[engine]` turn, and a `Stop` with `BG DONE` |

**claude's TUI runs every `Agent` call in the background** (claude 2.1.283), though the mock's call
asks for the foreground (`run_in_background: false`, which `PreToolUse`'s input drops). The
subagent's result is then taken up in a turn of claude's own, exactly as a background command's is.
The hooks the record reads (`SubagentStart` and `SubagentStop`, by `agent_id`) are the same as under
stream-json.

### Background work (`BG sleep 2; echo bg-out`): 8 runs, 8 pass

- `PostToolUse` of the `Bash` call carries `tool_response.backgroundTaskId`.
- The input's `Stop` carries `background_tasks: [{id, type: shell, command, description, status:
  running}]`; the notification turn's `Stop` carries `[]`.
- The notification's prompt names the task (`<task-id>`), and the transcript's entry for it has
  `origin.kind: task-notification`, which the record already reads.

So the list of background work is live at each turn's end (`Stop`, and `SubagentStop`), and a
notification says which task ended. No hook carries the list when a task is launched, so the
profile reports it at the end of the turn that launched it.

### PermissionRequest, claude's `default` mode (no `--dangerously-skip-permissions`)

The input is `TOOL touch <file>`, a write, which `default` mode asks about. The hook answers
`{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"|"deny",…}}}`.

| Case | macOS | Linux | Evidence |
|---|---|---|---|
| Fires | 24/24 | 24/24 | 0.09–1.6 s after `PreToolUse`, with `tool_name`, `tool_input`, `permission_suggestions` and the turn's `prompt_id`; no `tool_use_id` |
| `allow` after 1 s | 3/4 | 4/4 | the tool runs (`PostToolUse`) and the turn completes. **In one macOS run the answer was written and never taken:** claude logged `executePermissionRequestHooks called` and nothing after, and the turn waited on the dialog past 90 s. It did not recur; the machine was running two other claude suites at the time |
| `deny` with a message | 4/4 | 4/4 | no `PostToolUse`; the model gets the message as the tool's result (`TOOL DONE: probe says no`) |
| `allow` after 45 s | 4/4 | 4/4 | honoured; `permissionDecisionMs` ≈ 45 000 |
| `allow` after 11 min, hook timeout 86400 s | 1/1 | — | honoured at 660 s: the hook may block past the 600 s default when its own timeout allows |
| The hook times out (5 s) | 4/4 | 4/4 | `timed out after 5000ms`; claude falls back to its dialog, and the turn waits on keystrokes (no `Stop` within 90 s) |
| The hook answers nothing | 4/4 | 4/4 | the same: the dialog waits on keystrokes |
| Esc while the hook blocks | 4/4 | 4/4 | `[engine] turn N end … stop=tool_use`, no `Stop`; the tool never runs and the hook's later answer is ignored: an interrupt as the profile already reads one |

**claude's TUI draws its own dialog while the hook blocks**, in every run: "Do you want to proceed?
1. Yes / 2. Yes, and always allow access to <dir> from this project / 3. No · Esc to cancel". It
fires `Notification` (`notification_type: permission_prompt`) 6–12 s into the wait. The hook's
answer dismisses the dialog. So the hook is an answer path but not the only one: a keystroke that
reaches the terminal while a prompt waits answers the dialog, and a hook that times out, fails or
answers nothing leaves a dialog that only keystrokes can answer.

### What phase 3 builds on this

- **Tools and subagents:** the hook spool, through `claude-code`'s record. No new hook.
- **Claude's own turns and background work:** `UserPromptSubmit` bound to no input, `Stop`'s
  `background_tasks`, and the notification's task id.
- **Prompts: not built.** The hook decides when it answers, but declaring `prompts` takes a posture
  that prompts, which neither Claude profile has (both render `bypass`), and the TUI's fallback
  dialog needs keystrokes. It is left as an owner decision (contract.md, *The Claude Code TUI
  profile*).

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
  - compaction (`PreCompact`/`PostCompact`);
  - `--resume`;
  - an idle session over 60 s (`Notification` idle_prompt);
  - a Stop hook that blocks.
