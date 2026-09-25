# ADR-011: claude's subagents start and stop through its own subagent hooks

**Status:** Accepted (2026-09-25)

**Intent:** principle 4, *prefer the harness's own record*, and principle 6, *evolve public contracts
deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 4, a subagent's
start, type, end and last reply come from claude's own hook payloads, and its transcript is claude's
file, read when claude says the subagent has finished. Under principle 6, the new arguments, event
types and field are additions, `post-task` stays, and hw still accepts a `pre-task` that an earlier
config runs. It also applies principle 3: the hooks run under canonical arguments and report in hw's
event vocabulary.

## Context

A supervisor needs to know when a subagent starts. agentd reports that as `subagent.started`. It
also needs to know when the subagent ends and what it said.

Before this record, hw captured claude's subagents with two tool hooks matched on `Task`:

- `pre-task` (`PreToolUse`) fired before the subagent existed and spooled nothing. A subagent's
  start therefore had no event (the ADR-010 follow-up).
- `post-task` (`PostToolUse`) read the subagent's transcript once its tool call returned. It found
  the subagent through `tool_response.agentId`.

claude has hooks of its own for subagents. Both carry the parent's `session_id` and
`transcript_path`:

- `SubagentStart` sends the subagent's `agent_id` and `agent_type`.
- `SubagentStop` sends those two fields, plus `agent_transcript_path` and `last_assistant_message`.

Three facts were measured live on claude 2.1.281 and 2.1.282:

- **The tool is now named `Agent`.** claude still fires hooks matched on `Task` for it.
- **`post-task` misses background subagents.** A subagent run in the background returns from its
  tool call at once, with `tool_response.status` `async_launched`. `post-task` fires at that
  launch, when the transcript holds at most the subagent's prompt, and no hook reads the transcript
  later. So hw never captured a background subagent's work.
- **The subagent's last reply reaches the disk late.** claude writes a transcript record a moment
  after creating it. When `SubagentStop` fired, the last reply reached the disk up to about a tenth
  of a second later, whether or not the tool call waited for the subagent. A hook handler as fast as
  hw's read the transcript before the reply was there. It missed the reply at `SubagentStop`, and at
  `post-task` right after it.

## Decision

1. **The default spec includes `SubagentStart` and `SubagentStop`.** They run under the canonical
   arguments `HookArgSubagentStart` (`subagent-start`) and `HookArgSubagentStop` (`subagent-stop`).
2. **`subagent-start` spools a start marker.** It is one `transcript.EventSubagentStart`
   (`subagent_start`) event:
   - Its role is `system`, and it is stamped when the hook fired.
   - `AgentType` holds the subagent's type, cut to 256 bytes.
   - `Source` is `transcript.SourceHook`, and the native id is `hook:subagent-start:<agent_id>`.
   - It is tagged with the subagent's session (`agent_id`) under the parent's session.
3. **`subagent-stop` spools the transcript, then a stop marker.** The stop marker is one
   `transcript.EventSubagentStop` (`subagent_stop`) event, shaped like the start marker. Its `Text`
   is the subagent's last reply, bounded like a per-tool hook's output (`MaxToolHookBytes`). The
   transcript is handled like this:
   - hw resolves `agent_transcript_path` against the run's working dir and reads it only if it names
     `agent-<agent_id>.jsonl` in a `subagents` directory of the parent session, under the transcript
     root. Either of claude's two layouts qualifies.
   - The file is opened through an `os.Root` at the transcript root, so a symlink cannot lead the
     read outside it.
   - The transcript is parsed and tagged exactly as `post-task` does it. A record has the same id and
     content whichever hook read it.
   - If the file does not yet end with the reply claude handed over, hw reads it again each time it
     grows, for at most one second (`lastReplyWait`). hw spools the last version it read.
   - The stop marker is spooled even when the transcript is missing.
4. **`post-task` stays in the spec, and `pre-task` is retired.**
   - `post-task` (`PostToolUse` matched on `Task`) reads the same records again for a subagent that
     its tool call waited for. Consumers dedup the copies by id. loom already reads subagents
     through it.
   - `pre-task` is no longer in the spec. An ensure rewrites every managed entry under
     `PreToolUse`, so it drops the old entry wherever the spec writes there: the default spec's yield
     guard does, and so do the per-tool hooks. A config written before still runs it, so it is still
     accepted, and it still spools nothing.
5. **Markers never enter `Run`'s conversation.** They are `SourceHook` events, like the per-tool
   hooks' events, and the authority filter never admits those. A consumer reads markers with
   `ReadSpool`. The subagent's transcript events are file events tagged with the subagent's
   session, and the filter admits them as it always admitted `post-task`'s.
6. **`transcript.Event` gains `AgentType`** (`agent_type`, omitted when empty). The durable wire
   form carries it.

## Alternatives

- **Keeping `pre-task` and giving it an event.** It fires before the subagent exists. It has the
  `tool_use_id` and the requested type, but not the subagent's id, so its event could not be tied to
  the subagent's session or transcript.
- **Replacing `post-task` with `SubagentStop`.** `SubagentStop` reads everything `post-task` reads,
  and a background subagent too. But loom reads subagents through `post-task` today. Keeping it
  costs a second read of a waited-for subagent's transcript, whose copies dedup by id.
- **Reading the transcript once, without waiting.** A handler as fast as hw's would miss the
  subagent's last reply whether or not the call waited for it. The stop marker would still carry the
  reply, but the transcript, which loom nests under the parent, would not.
- **A fixed pause before reading.** Every stop would pay the pause, and a fixed pause still does not
  bound claude's delay. Waiting for the reply claude names costs nothing when the reply is already
  on disk.
- **Reading every subagent transcript at `SessionEnd`.** That is a backstop, not a start marker: it
  would not report a subagent while the subagent runs.

## Boundary

Guaranteed, with the default spec installed:

- Each subagent that claude starts spools one start marker, and each one that stops spools one
  stop marker. Both are tagged with the subagent's session under its parent's and carry the
  subagent's type.
- The stop marker carries the subagent's last reply, bounded.
- The transcript read at the stop is tagged as `post-task` tags it, with the same ids.
- A `pre-task` entry from an older config spools nothing and is not an error.

Not guaranteed:

- **That claude fires the hooks.** A hook that fails or times out is not retried.
- **That the transcript read at `SubagentStop` is complete.** hw gives up in two cases and spools
  what the file held: the reply is still not on disk after one second, or the reply claude names
  does not match the text of the file's last assistant entry as hw joins it.
- **Records written after the last reply.** `subagent-stop` does not read records that claude
  writes after the last reply. For a background subagent, nothing reads them later.
- **That markers can be trusted.** They come from the workload, through a hook subprocess, so under
  agentd they are untrusted.
- **Every field on older claude.** claude added `SubagentStart` in 2.0.43. `SubagentStop` has
  carried `agent_id` and `agent_transcript_path` since at least 2.0.42; `agent_type` and
  `last_assistant_message` were added to it between 2.0.43 and 2.1.250. Without those two fields,
  the stop marker has no type and no reply, and `subagent-stop` does not wait.

## Evidence

- **Versions.** The hook payloads were read from claude's own bundles.
  - `SubagentStart` is absent from 2.0.42 and present from 2.0.43, with `agent_id` and `agent_type`.
  - `SubagentStop` has `agent_id` and `agent_transcript_path` in both, and neither `agent_type`
    nor `last_assistant_message`.
  - 2.1.250, 2.1.258 and 2.1.282 build both payloads with every field used here.
  - Live runs on 2.1.281 and 2.1.282 received those fields. hw pins 2.1.270.
- **`pkg/harness/claude/subagenthooks_test.go`** covers:
  - both markers, with their identity, bounds and rune-safe type cut;
  - malformed payloads, which are refused;
  - the transcript before the stop marker;
  - the same events from `subagent-stop` and `post-task` for one subagent (`reflect.DeepEqual`);
  - both layouts;
  - refused paths: outside the root, traversal, another subagent's file, another session's
    directory, and a file that is not in a `subagents` directory;
  - a symlinked file or directory out of the root, which is refused (the test fails if the read
    bypasses `os.Root`);
  - a relative config root;
  - a missing transcript;
  - a late reply, which is waited for; a reply that is never written, which costs at most the
    bound; no reply, with no wait;
  - the retired `pre-task`.
- **`pkg/harness/claude/hookinstall_test.go`** checks two things. A fresh install has
  `SubagentStart` and `SubagentStop` and no `pre-task`. An earlier hw's config migrates in one
  ensure, with the user's hooks kept.
- **`pkg/harness/subagenthooks_test.go`** runs `HandleHookEvent`, `ReadSpool` and `AckSpool` for
  both hooks, with the resume guard armed.
- **`pkg/harness/subagenthooks_live_test.go`** (`HW_LIVE_ACCOUNT=1`) runs the real claude on a real
  account, with the default spec installed and the test binary handling the hooks. The prompt asks
  once for a subagent the call waits for, and once for a background subagent. The mode is read from
  the parent's `Agent` call. Each run spooled:
  - one start marker and one stop marker, of type `general-purpose`, the stop carrying the reply;
  - at the stop, the subagent's transcript, including that reply, and every record as the final file
    holds it;
  - for the waited-for subagent, the same records again from `post-task`;
  - for the background subagent, nothing from `post-task`.

  It passed twice in each mode on macOS with claude 2.1.282, and on Ubuntu 26.04 with 2.1.281.
  Before the wait was added, live runs of the same kind read the transcript at `SubagentStop`
  without the reply every time, and `post-task` missed it in four of five waited-for runs.

## Consequences

- **A subagent's stop now spools its transcript as well.** For a waited-for subagent, the records
  arrive twice, from `subagent-stop` and from `post-task`, with the same ids. loom's event store
  collapses them on (run, session, id).
- **A stop can hold claude for up to a second.** That happens only while the file lacks the reply.
  The measured wait was about a tenth of a second.
- **A consumer of `ReadSpool` has two new file prefixes.** It tells a start from a stop by file name
  (`subagent-start-`, `subagent-stop-`) or by the marker's type — for agentd, `subagent.started`
  and `subagent.stopped`. A consumer that took a subagent's end from its `post-task-` file takes it
  from the stop marker instead: for a background subagent, `post-task` fires at the launch.

## Follow-ups

- Records claude writes after a background subagent's last reply are not read. A `SessionEnd`
  backstop that reads the session's `subagents` directory would close that gap.
- Codex offers no subagent hooks.
