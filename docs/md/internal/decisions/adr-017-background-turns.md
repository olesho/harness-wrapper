# ADR-017: a turn the harness starts to take up background work

**Status:** Accepted (2026-10-05)

**Intent:** principle 3, *normalize, don't leak*, principle 4, *prefer the harness's own record*,
and principle 6, *evolve public contracts deliberately*
([INTENT](../../../../INTENT.md#design-principles)). Under principle 3, a turn claude starts by
itself reaches callers in the one turn model, as a turn of no input. Under principle 4, its end is
read from claude's transcript too. Under principle 6, it is a capability of interface 1.3 of its
own, beside `autonomous_turns`, whose promises it does not make.

## Context

claude can run a command or a subagent in the background. Its input's turn ends while that work
runs; when the work ends, claude tells the model so and starts a turn by itself to take the result
up. That turn is no input's: the transport's turns began at an input's submission and ended at its
`result`, so its `result` — the answer — was dropped, and the Session looked idle while claude
worked. agentd's real-account e2e met it with a background subagent.

[ADR-013](adr-013-session-load-and-own-turns.md)'s `autonomous_turns` reports turns a harness
starts itself, but promises what codex's goals do and claude cannot: that an input stops such a
turn, and that once stopped the harness rests until an input's turn ends. claude starts a turn
whenever background work ends, and that turn carries the work's result: cutting it off loses it.

claude 2.1.283 marks such a turn. Live, `system/task_notification` names the task that ended, a
`system/init` starts the turn, and its `result` carries `origin` kind `task-notification`. In the
transcript, the notification is a user entry with that origin, whose text names the task
(`<task-id>`). The task id is what the two have in common.

## Decision

1. **Interface 1.3 adds `background_turns`.** The harness starts a turn of its own when work it
   began in the background ends. Such a turn is reported as `turn_started` and `turn_ended` naming
   a turn and no input, live, and its end from the record too; while it runs the Session is busy
   under its turn id, a Send answers busy as during an input's turn, and Interrupt may name it.
   Unlike `autonomous_turns`, the harness starts one whenever background work ends, whatever
   stopped one before.
2. **The Session** reports own turns for either capability; it lets a Send through an own turn
   only with `autonomous_turns`, whose Submit stops it.
3. **The claude profile** declares the capability. Its transport starts an own turn at the
   `system/init` that follows a task notification while no input's turn runs, named
   `task-<id>` after the task; the next `result` ends it, and so does a `result` whose origin is a
   task notification whatever turn is current. `InterruptTurn` interrupts it. The record reader
   starts the same turn at the notification's entry, reports none of its text as input, and ends
   it at the reply's `end_turn`.
4. **The mock model** runs `BG <command>` in the background and answers the notification
   `BG DONE`; the conformance scenario `background` checks the turn live and in the record.

## Consequences

- agentd sees claude's answer after a background subagent or command as a turn of its own, with
  the agent busy meanwhile; an input waits for it.
- A turn whose start a frame never marked — a `result` with the task-notification origin and no
  notification before it — is reported started and ended at once.
- Several background tasks that end together and start one turn name it after the first.
