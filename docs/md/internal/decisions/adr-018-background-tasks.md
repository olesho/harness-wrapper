# ADR-018: the work a harness runs in the background is reported

**Status:** Accepted (2026-10-05)

**Intent:** principle 3, *normalize, don't leak*, and principle 6, *evolve public contracts
deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 3, claude's task
types reach callers as three kinds of the contract's own. Under principle 6, a new observation kind
is a minor version: interface 1.4, under the capability whose turns it explains.

## Context

[ADR-017](adr-017-background-turns.md) reports the turn claude starts when background work ends.
Between the input's turn that started the work and that turn, the Session is `idle`, and a
Supervisor that stops idle Sessions — agentd parks an agent after ten idle minutes — stops claude
with the work, whose result is then never taken up. Nothing told it the harness was not at rest.

claude 2.1.283 says so. Whenever its background work changes it emits
`system/background_tasks_changed`, listing every task still running: `task_id`, `task_type`
(`local_bash` for a shell command, `local_agent` for a subagent) and `description`; an empty list
once none is left. `system/task_started` and `system/task_updated` say the same per task.

## Decision

1. **Interface 1.4 adds the observation `background_tasks`**, live, under `background_turns`:
   every task the harness runs in the background, each an `id`, a `kind` — `command`, `subagent`
   or `other` — and a `description`, reported whenever the set changes, an empty list once none is
   left. It names no input or turn: the work outlives the turn that started it.
   `State.background` holds the same list.
2. **The Session** keeps the latest list for `State`, and drops it when the harness exits.
3. **The claude profile** reports `background_tasks_changed` as it comes, mapping `local_bash` to
   `command`, an `*_agent` type to `subagent` and anything else to `other`, with descriptions cut
   to 200 bytes.
4. **The conformance scenario `background`** checks that the work is reported running — one
   command, naming no input or turn — and then ended, and that `State` lists nothing once it is
   taken up.

## Consequences

- A Supervisor can hold a Session whose harness works on in the background, and show what runs.
  agentd holds an agent's park while the list is not empty, up to a cap.
- A background command that never ends — a server — keeps the list non-empty for as long as it
  runs; what to do about that is the Supervisor's choice.
