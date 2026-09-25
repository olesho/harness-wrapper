# ADR-009: claude-code can run on its stream-json protocol instead of its screen

**Status:** Accepted (2026-09-24); amended 2026-09-25

**Intent:** principle 1, *the screen is a contract we don't own*, and principle 4, *prefer the
harness's own record* ([INTENT](../../../../INTENT.md#design-principles)) — claude has a
machine protocol whose frames say what the screen only shows. It also applies 2 (a turn's end, a
retry, an interrupt and a message's receipt are stated by the harness, not inferred), 3 (the same
`Turn`, `InterruptResult` and `InputRequest` vocabulary on both transports) and 6 (the transport is
an option beside the TUI, whose behaviour does not change).

## Context

Every guarantee the TUI conversation gives rests on reading claude's rendered screen: keep-alive
against a classifier that misreads a quiet screen (ADR-006), held turns and a busy gate read from the
status region, an interrupt keyed on Esc with a composer clear and an acceptance gate (ADR-007), turn
text extracted from the screen. Each rests on rendering facts a claude release can change, and a
change surfaces as a silent misreading: claude draws its message bullet as `●` on Linux and `⏺` on
macOS, and until v0.13.1 every completed turn on Linux carried the whole screen as its text and an
API error read as a completed turn.

claude also runs as `claude -p --input-format stream-json --output-format stream-json`: one process
for a whole conversation, NDJSON frames on stdin and stdout, the transport its Agent SDK uses. agentd's
P11 probe recorded it on claude 2.1.281 against a local Messages API on macOS, Ubuntu 26.04 and
Debian 13 (test/corpus/claude-code-stream): one `result` frame per turn with a `terminal_reason`;
`system/api_retry` frames while a turn retries; a `command_lifecycle` receipt for each message's
host-supplied uuid; an `interrupt` control request answered with a receipt; `can_use_tool` permission
requests; capabilities advertised on `system/init`. The transcript, hooks, session ids, resume, MCP,
persona and permission modes behave as in the TUI.

## Decision

**`chat.Options.Transport = TransportStreamJSON` runs claude-code on its stream-json protocol,
behind the same `Conversation`.** The TUI stays the default and the only transport of every other
harness.

1. **One process per Conversation**, on pipes, in its own process group: the session flags chat
   manages (`--session-id`, `--resume`), the stream-json flags, the caller's `Args`, then Effort,
   Model and PermissionMode injected by `wrapper.HarnessArgs` — the same injection `wrapper.Start`
   applies. Below bypass, `--permission-prompt-tool stdio` routes permission prompts to the
   Conversation. `Open` returns once claude answers an `initialize` control request.
2. **Send** records the turn as the TUI does (`beginTurn`), writes the message with a fresh uuid, and
   returns once claude's `command_lifecycle` says it queued or started it (two minutes at most). One
   turn in flight, as on the TUI.
3. **A turn ends on its `result` frame**: complete with the result text; errored with
   `api_error_status` as `HTTPCode` and — from the error tag on the turn's synthetic assistant
   message, the tag the TUI reads from the transcript — the same reasons and codes (`apiErrorClasses`);
   interrupted when the terminal reason is `aborted_*`. A message claude refuses or discards ends
   errored.
4. **Interrupt** sends one `interrupt` control request per turn; concurrent callers join it. The
   turn's result settles it: `stopped` when the turn had streamed text or a tool call, `cancelled`
   when it had not, `too_late` when it finished first. No control token, no composer.
5. **The capability gate**: the first `system/init` must advertise `interrupt_receipt_v1` and
   `msg_lifecycle_v1`. A claude without them fails its first turn with the reason and is stopped.
6. **Quit** closes stdin: claude finishes the turn it is in and exits. **Close** closes stdin, then
   sends SIGTERM and SIGKILL to the process group after a grace each, then drains (ADR-008).
7. **Exit** goes through the TUI's path (`exitWith`): the turn in flight ends errored, EventExited
   is last, with the exit code, the signal and the tail of stderr.
8. **What has no stream counterpart**: `ScreenSnapshot` is empty, `Resize` does nothing, `Wrapper`
   is nil, and Containment is refused. `PermissionMode` reads the mode claude reported;
   `SetPermissionMode` sends a `set_permission_mode` control request. A `can_use_tool` request is an
   `InputRequest` of kind `permission_prompt` with `allow` and `deny`, resolved by `InputPolicy`,
   `OnInputRequest` or `Answer`.
9. **The account's usage limit**: claude's `rate_limit_event`, which it sends for a claude.ai
   subscription account whenever the limit changes, is `EventRateLimit` and `State().RateLimit`: a
   `RateLimit` with claude's figures — status (`allowed`, `warning`, `rejected`; `unknown` for one
   this build does not know), reset time, the limiting window, utilization, overage, and the usage of
   every window. It ends no turn and blocks nothing; a turn a wall refused still ends with its own
   `Code` and `ResumeAt`. A report without a status is dropped. The TUI transport has none.

## Alternatives

- **A second package with its own Conversation type.** Every consumer — agentd, chatd — would switch
  types to switch transports, and the delivery, store and exit machinery would be copied.
- **Speaking stream-json from agentd.** hw is the substrate; every harness gap is an hw change, and a
  protocol client in a consumer would be a second one to maintain.
- **Making stream-json the default for claude-code.** A behaviour change for every caller at once;
  the TUI stays until consumers have moved.
- **Replaying several queued messages.** claude accepts a message while a turn runs and can fold it
  into that turn; one message in flight keeps each turn's receipt and result unambiguous.

## Boundary

Guaranteed on this transport: a turn ends only on claude's `result`, a lifecycle refusal, or the
process's exit; an interrupt is reported only as claude settled it; a message is reported sent only
after claude's receipt; the capability gate refuses a claude that cannot give those receipts.

Not guaranteed: the protocol is not published as a versioned contract. `command_lifecycle` is marked
internal in the CLI's own schema, and frames can change between versions; the capability gate and
the recorded corpus are how a change is caught. A process a tool detaches into its own session
outlives Close unless a cgroup holds it. After a cancelled or failed turn, claude sends that prompt
again with the next one: the model sees it. A `RateLimit` is claude's report as it stood when sent,
not a live reading; its per-window usage comes from a part of the report claude marks internal, and
an API-key account never reports one.

## Evidence

- `pkg/chat/stream_test.go`, over a fake claude that speaks the recorded frames: turns in order;
  launch arguments at bypass and below; interrupt mid-reply, before the first token, mid-tool, with
  no turn, and three callers joining one; API errors, the usage wall with its reset time, a refused
  message; permission prompts answered and denied by policy; exit mid-turn; a claude without the
  capabilities; Quit; Reopen with `--resume`; SetPermissionMode; refused options; the frame bound.
- `pkg/chat/stream_corpus_test.go` replays thirteen recorded sessions of claude 2.1.281 through the
  driver's frame handling and checks each turn's ending.
- `pkg/chat/stream_live_test.go` (`HW_LIVE_STREAM=1`) runs the real claude against a local Messages
  API: turns with transcript History, the three interrupts, an exhausted and a non-retried API error,
  Quit then Reopen, and a SIGKILL from outside. Passed on macOS, Ubuntu 26.04 and Debian 13 with
  claude 2.1.281.
- `pkg/chat/stream_account_live_test.go` (`HW_LIVE_ACCOUNT=1`) runs the real claude against the
  Anthropic API on a real account: a reply, a Bash tool with PreToolUse and PostToolUse hooks
  firing, a stdio MCP tool call, an interrupt mid-reply (`stopped`), Quit, then Reopen remembering
  the first answer. Passed on macOS, Ubuntu 26.04 and Debian 13 with claude 2.1.281 on haiku. Since
  the amendment it also checks the first reply brings the account's `RateLimit` (status, reset time,
  window, two windows' usage): passed on macOS with claude 2.1.282 and on Ubuntu 26.04 with 2.1.281.
- `TestStream_RateLimit` and `TestParseRateLimit`: a real account's `rate_limit_event`, replayed by
  the fake, arrives before its turn ends and in `State`; a warning, an unknown status, the rejected
  report ahead of a usage wall, and a report without a status dropped.

## Consequences

- agentd drives claude through this transport. The TUI's keep-alive, held turns, busy gate and
  keystroke interrupt remain for the TUI and every other harness.
- `wrapper.HarnessArgs` is public: the launch-knob injection, without a launch.
- `beginTurn` and `exitWith` are shared by both transports.

## Follow-ups

- chatd exposes no transport choice; one comes when a remote consumer needs it. Its SSE frames do not
  carry `EventRateLimit` yet.
- meta-harness mirrors the transport and the corpus.
- The recorded corpus has no `rate_limit_event`: its sessions ran against a local API, which claude
  gets no usage limit from.

## History

- 2026-09-25: claude's `rate_limit_event` surfaces as `EventRateLimit` and `State().RateLimit`
  (Decision 9).
