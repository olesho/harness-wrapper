# ADR-010: Per-tool hooks are opt-in, and their events stay out of Run's conversation

**Status:** Accepted (2026-09-25)

**Intent:** principle 6, *evolve public contracts deliberately*, and principle 3, *normalize, don't
leak* ([INTENT](../../../../INTENT.md#design-principles)) — per-tool hooks are added beside the
default spec, which `Run` and loom keep installing unchanged, under canonical arguments and the
existing `tool_use` / `tool_result` vocabulary. It also applies 2: a failed tool is reported from the
harness's own failure hook, never inferred from its output.

## Context

A supervisor that outlives its harness — agentd — needs each tool call as it happens: that it
started, and when and how it ended, while the turn is still running. The transcript records a call
once the harness writes it; the stream records it only on the stream transport, and neither says when
the tool finished. claude reports all three moments through its hooks: `PreToolUse` before every tool,
`PostToolUse` after one succeeds and `PostToolUseFailure` after one fails, each with the tool's name,
its `tool_use_id` and its input, the outcome hooks adding the response or the error — and, inside a
subagent, the subagent's `agent_id` (measured on claude 2.1.281 and 2.1.282).

hw's claude hook provider handled only the lifecycle hooks and the `Task`-matched subagent hooks;
`HandleHookEvent` refused any other event (`unknown event "pre-tool-use"`), so a consumer that
registered per-tool hooks got nothing in its spool.

## Decision

1. **An optional interface, not the default spec.** `ToolHookProvider.ToolHookEntries()` returns a
   harness's per-tool hook entries; claude's are `PreToolUse`, `PostToolUse` and
   `PostToolUseFailure`, for every tool. `HookSpec` does not include them: each fires a hook
   subprocess on every tool call, and `Run` never needs them. A consumer that wants them appends them
   to the spec it ensures.
2. **Canonical arguments.** `HookArgPreToolUse` (`pre-tool-use`), `HookArgPostToolUse`
   (`post-tool-use`) and `HookArgPostToolUseFailure` (`post-tool-use-failure`): the `<harness> <arg>`
   the hook runs and the prefix of the spool file it writes.
3. **One event per hook, in the existing vocabulary.** A start is a `tool_use` carrying the input; an
   end a `tool_result` carrying the response as text; a failure a `tool_result` carrying the error,
   prefixed `interrupted: ` when the harness says so. The event is stamped when the hook fired, its
   `Source` is the new `transcript.SourceHook`, and its native id is `hook:<arg>:<tool_use_id>`. A
   tool run inside a subagent is tagged with the subagent's session under the parent's.
4. **Bounded.** Input and output are each cut to `MaxToolHookBytes` (16 KiB). Cut output ends with
   `ToolHookTruncated`; cut input becomes a JSON string of its cut JSON text.
5. **Never in Run's conversation.** The authority filter drops every `SourceHook` event, parent or
   subagent: each is a third copy of a call the stream or the file records. A consumer reads them from
   the spool with `ReadSpool`.

## Alternatives

- **Adding the entries to claude's `HookSpec`.** Every `Run` and loom launch would fork a hook
  subprocess on every tool call for events it never reads.
- **New event types (`tool_start`, `tool_end`).** The `Type` vocabulary is the one loom's DTO shares;
  a hook's start and end are a `tool_use` and a `tool_result` seen at another moment, and `Source`
  already says where an event came from.
- **The transcript's native ids (`tool-use:<id>`).** The hook's copy would collapse into the file's
  or the stream's under dedup, losing the moment it records; and a success and a failure of one call
  would need distinct ids anyway.
- **Reading the tool's outcome from `PostToolUse` alone.** claude sends a failed tool to
  `PostToolUseFailure` instead; without it a failed call never ends.

## Boundary

Guaranteed: a consumer that installs the entries gets one spooled event per hook claude fires, with
the tool's name and `tool_use_id`, bounded, in a file its argument names; `Run` and the default spec
behave exactly as before.

Not guaranteed: that claude fires the hooks — a hook that fails or times out on claude's side is not
retried, and a call can lose its start or its end. The events are claude's report through a
subprocess the harness runs, so, under agentd, they come from the workload and are untrusted. A
failure's file name also starts with `post-tool-use-`; a consumer matching by name matches the
longer argument first.

## Evidence

- `pkg/harness/claude/toolhooks_test.go`: each hook's event, identity, bounds at and past the limit,
  subagent tagging, a traversal `agent_id` refused, malformed payloads refused, and the default spec
  unchanged.
- `pkg/harness/toolhooks_test.go`: the entries install beside the `Task`-matched and yield hooks of the
  same native events, idempotently, with a consumer's command and owner, keeping a user's hook; each
  hook through `HandleHookEvent` spools one file that `ReadSpool` returns and `AckSpool` removes.
- `pkg/harness/filter_test.go`: no mode admits a `SourceHook` event.
- `pkg/harness/toolhooks_live_test.go` (`HW_LIVE_ACCOUNT=1`): the real claude on a real account, hooks
  installed as a consumer installs them and handled by the test binary: a Bash call spooled a start
  and an end with one `tool_use_id`, the input and the output; a Read of a missing file a start and a
  failure carrying claude's error. Passed on macOS with claude 2.1.282 and Ubuntu 26.04 with 2.1.281.

## Consequences

- `transcript.SourceHook` joins `SourceLive` and `SourceFile` in the durable spool form.
- A consumer of `ReadSpool` tells the kinds apart by file name or by the event's native id.

## Follow-ups

- Codex and the other harnesses offer no per-tool hooks yet.
- claude's `pre-task` hook spools nothing, so a subagent's start has no event of its own beyond its
  `Task` call's `pre-tool-use`.
