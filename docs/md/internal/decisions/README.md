# Architecture decisions

An architecture decision record (ADR) states one design choice: what forced it, what was chosen, what
was turned down, and what it costs. [INTENT](../../../../INTENT.md) gives the criteria a change is
judged against; a record is one such judgement, written down. Each record names the
[design principles](../../../../INTENT.md#design-principles) it applies.

## Index

| Record | Decides | Status | Principles |
|---|---|---|---|
| [ADR-001](adr-001-vt100.md) | `pkg/screen` wraps `vt10x`, chosen by replaying recorded sessions | Accepted 2026-05-14 | 1, 4 |
| [ADR-001 (TS addendum)](adr-001-vt100-ts.md) | The TypeScript screen bench wraps `@xterm/headless` and keeps curated ground truth | Accepted 2026-07-16 | 1 |
| [ADR-002](adr-002-interactive-input.md) | Blocking dialogs are answered through a semantic `InputRequest` / `Answer` channel | Accepted 2026-06-15; amended 2026-08-29, 2026-09-06 | 2, 3, 7 · also 1, 5, 6 |
| [ADR-003](adr-003-env-visibility.md) | The Go environment core stays in `internal/env` until a consumer needs it | Accepted 2026-07-21 | 6 |
| [ADR-004](adr-004-thread-scoped-landlock.md) | A contained launch restricts one locked thread, never the wrapper | Accepted 2026-09-15 | 7 |
| [ADR-005](adr-005-apparmor-socket-layer.md) | On Landlock ABI 6–8 a stacked AppArmor profile denies pathname sockets outside its roots | Accepted 2026-09-19 | 7 · also 6 |
| [ADR-006](adr-006-classification-and-lifetime.md) | A classification ends the harness only when the caller leaves its lifetime to the wrapper; the harness ends its turns, and Send never types into a working harness | Accepted 2026-09-23; amended 2026-09-23 | 2 |
| [ADR-007](adr-007-interrupt.md) | An interrupt is a conversation operation: no control token, never inside a submit, and the turn ends `interrupted` only on the harness's acknowledgement | Accepted 2026-09-23 | 2, 1 · also 3, 4, 6 |
| [ADR-008](adr-008-event-delivery.md) | Events are delivered in order, once, to one callback through a bounded queue, and end with `EventExited`; `Events()` stays best-effort | Accepted 2026-09-23 | 2, 7 · also 6 |
| [ADR-009](adr-009-stream-json-transport.md) | claude-code can run on its stream-json protocol behind the same `Conversation`: turns end on `result` frames, sends and interrupts on claude's receipts, and the account's usage limit is reported as claude states it | Accepted 2026-09-24; amended 2026-09-25 | 1, 4 · also 2, 3, 6 |
| [ADR-010](adr-010-per-tool-hooks.md) | Per-tool hooks are opt-in beside the default spec, spool one bounded event per hook, and never enter `Run`'s conversation | Accepted 2026-09-25; amended 2026-09-25 | 6, 3 · also 2 |
| [ADR-011](adr-011-claude-subagent-hooks.md) | claude's subagents start and stop through its own `SubagentStart` / `SubagentStop` hooks: a start marker, the transcript read at the stop once it holds the last reply, then a stop marker; `post-task` stays and `pre-task` is retired | Accepted 2026-09-25 | 4, 6 · also 3 |
| [ADR-012](adr-012-harness-adapter-interface.md) | hw implements the Harness Adapter Interface: the contract in `pkg/contract`, one Harness Adapter with a profile per harness that renders its configuration and owns its hook helper, and chat and supervisor cores that link one harness at a time under unchanged `pkg/chat` and `pkg/wrapper` | Accepted 2026-09-28; amended 2026-10-06 | 3, 5, 6 |
| [ADR-013](adr-013-session-load-and-own-turns.md) | Interface 1.1: a saved Session loads into a fresh environment through relocations and a strict open, from source versions proved by fixtures; a turn the harness starts itself is a turn with no input, which an input preempts; codex's name and goal are saved with a thread and checked when it is loaded | Accepted 2026-09-30 | 2, 3, 4, 6 |
| [ADR-014](adr-014-brokered-credentials.md) | Interface 1.2: behind an egress broker a harness works with placeholders; the Descriptor routes each credential kind to its hosts and headers, `Placeholder` renders what stands in for a credential and the swaps a broker makes, and the runtime passes the broker and its certificates in the environment | Accepted 2026-10-04 | 3, 6, 7 |
| [ADR-015](adr-015-login-keeper.md) | Interface 1.2 lets a runtime keep a subscription login itself: signed in with a device code, refreshed by the harness's own client on asking, lent behind its broker with nothing that refreshes it; codex's keeper drives its app-server | Accepted 2026-10-04 | 3, 6, 7 |
| [ADR-016](adr-016-headers-from-files.md) | Interface 1.3 lets an MCP connector's secret header come from a file: claude reads it through a headersHelper script each time it connects; codex, which reads headers from its configuration or environment alone, gets it in its own environment at start | Accepted 2026-10-05 | 3, 6 |
| [ADR-017](adr-017-background-turns.md) | Interface 1.3 adds background_turns: the turn a harness starts by itself when background work ends is reported as a turn of no input, live and in the record, and keeps the Session busy without being cut short; claude's is named after the task, from its task notification | Accepted 2026-10-05 | 3, 4, 6 |
| [ADR-018](adr-018-background-tasks.md) | Interface 1.4 adds the observation background_tasks under background_turns: every task the harness runs in the background (command, subagent or other), live, whenever the set changes, and State.background; claude's from background_tasks_changed | Accepted 2026-10-05 | 3, 6 |
| [ADR-019](adr-019-pin-codex-0-160.md) | The codex pin moves to 0.160.0 (claude stays at 2.1.283: 2.1.289 takes a pasted prompt as content); the old pin stays a load source; a loaded thread gets back the name its session_index.jsonl gives, since codex 0.160 answers names from a database no archive carries; codex reads connector headers through http_headers_helper | Accepted 2026-10-05 | 4, 6 |
| [ADR-020](adr-020-placeholder-model.md) | Interface 1.5 gives Placeholder the agent's model, so one credential kind can serve several providers: a harness narrows each swap to the provider the model names, and refuses a model it cannot place | Accepted 2026-10-06 | 3, 6 |
| [ADR-021](adr-021-pi-profile.md) | The Pi profile drives pi over its RPC mode: a tag extension names each input in pi's session, the record proves each run's end or leaves it unknown, and one `api_key` kind serves every provider in the profile's table | Accepted 2026-10-06 | 4, 2 · also 3, 7 |

## Intent coverage

| Principle | Records | How the decision applies it |
|---|---|---|
| 1 · The screen is a contract we don't own | 001, 001-TS, 002, 007, 009 | Emulators are chosen by replaying recorded real sessions, and the dialog detector was re-verified live when claude 2.1.251 changed the dialog's shape; every interrupt shape was captured on the pinned claude before code keyed on it; claude-code can leave the screen for its stream-json protocol, whose frames were recorded on three systems before the driver keyed on them |
| 2 · A wrong verdict is worse than no verdict | 002, 006, 007, 008, 009, 010, 013 | A dialog the adapter cannot parse is reported as unparseable and blocks; it is never guessed at, and never answered with empty keys. A caller that owns the harness's lifetime is told what a classification found and decides; silence is not evidence, and a verdict never outlives its evidence. A turn ends interrupted only when the harness says it stopped, read per turn. No turn's end, and not the harness's exit, is lost to a full channel. A tool's failure is the harness's own failure hook, never read from its output. A loaded Session whose record, name or goal did not come with it does not open, rather than start over or go on without them |
| 3 · Normalize, don't leak | 002, 007, 009, 010, 011, 012, 013, 014, 015 | One `InputRequest` vocabulary for every harness; clients answer an option id or alias, and keystrokes stay in the per-harness adapter. One `interrupted` turn state, with the interrupt keys and the screen reading in the adapter; the stream-json transport reports in the same `Turn`, `InterruptResult` and `InputRequest` vocabulary; per-tool hooks run under canonical arguments and report `tool_use` / `tool_result`, and claude's subagent hooks under canonical arguments as `subagent_start` / `subagent_stop`; a runtime drives every harness through the Harness Adapter Interface, whose profiles hold each harness's files, flags, hooks and record; where a saved Session's files go in a new environment, and the turns a harness starts by itself, are the profile's to know and one vocabulary to the runtime; so are the hosts a harness reaches, where it presents a credential and what may stand in for it, and how it signs in and refreshes a login |
| 4 · Prefer the harness's own record | 001, 007, 009, 011, 013 | Where emulator fidelity falls short, turn text comes from the harness's transcript and the screen serves liveness only; an interrupted turn's partial reply is the transcript's once it records the interrupt; on stream-json a turn's end, text and error tag are the harness's own frames; a subagent's start, type, end and last reply are claude's own subagent hooks, and its transcript is read when claude says it has finished; a turn codex started itself is read from the rollout as one, and what the rollout cannot say — a thread's goal — is asked of codex |
| 5 · Keep the stack one-way and the core transport-free | 002, 012 | Detection sits in `pkg/turns`, the channel in `pkg/chat`, HTTP + SSE only in `cmd/harness-chatd`; the chat and supervisor cores name no harness, and each harness's readings are capabilities of its own adapter |
| 6 · Evolve public contracts deliberately | 002, 003, 005, 007, 008, 009, 010, 011, 012, 013, 014, 015 | SSE frames gained a `type` field additively; no Go surface is published before a consumer can shape it; a policy without the AppArmor layer serializes, and fingerprints, exactly as before; the interrupt route and wire state came with their conformance fixtures; `OnEvent` was added beside `Events()`, whose contract stayed; the stream-json transport is an option beside the TUI, the default; per-tool hooks are an interface beside the default hook spec, which stays as it was; the subagent hooks join the default spec beside `post-task`, and a retired `pre-task` an older config still runs is accepted; `pkg/chat` and `pkg/wrapper` keep their API and every built-in harness over the cores, and the Harness Adapter Interface is a new package beside them; loading and a harness's own turns are a minor version of that interface, each behind a capability, and so are credentials behind a broker and a login the runtime keeps |
| 7 · Say what is enforced, not what is intended | 002, 004, 005, 008, 014, 015 | Accepting a skip-all-permissions launch is its own policy kind; both containment records list what is guaranteed and what is not; a launch refuses rather than degrades and reports the rule it enforced; event delivery states its promise — in order and once while the process lives — and what it does not cover; an adapter renders placeholders and routes, and leaves the secrecy to the runtime's fence and broker, which it does not claim; a keeper lends nothing that refreshes, and says when a login can no longer be refreshed |

No record yet covers the `Status` vocabulary, the `ErrorClass` taxonomy and which matcher may classify
what (principle 2 — ADR-006 decides only who acts on a classification); the transcript parsers (4);
the layering and import rules beyond the cores ADR-012 introduces (5); the frozen `turnproto` contract and the conformance corpus shared
with meta-harness (6); or version pins and the drift pipeline (1). Those are decided in code and in
[Architecture](../architecture.md), [turnproto](../turnproto.md) and
[Versions & drift](../versions-drift.md).

## Shape of a record

Files are named `adr-00N-<slug>.md`. A record opens with two lines — **Status** (accepted date, and
the date of each amendment) and **Intent** (the principles it applies, and in a clause, how) — and
then uses these sections, in this order, leaving out the ones that do not apply:

| Section | Holds |
|---|---|
| Context | What forced the decision: the problem, the constraints, who is affected |
| Decision | What was chosen, stated as it holds now |
| Alternatives | What was considered and turned down, each with its reason |
| Boundary | For a decision that is a guarantee: what it guarantees, and what it does not |
| Evidence | For a decision that rests on measurement: the method and the results it was made on |
| Consequences | What follows — costs, new failure modes, what other code must respect |
| Follow-ups | Work the decision leaves open |
| History | One dated line per amendment, oldest first — only once a record has been amended |

Three rules keep a record readable:

- **State the decision as it holds.** Amend the body in place and add a dated line to History; do not
  leave a superseded design standing beside its replacement. Git keeps the earlier text.
- **Reverse a decision with a new record.** Mark the old one `Superseded by ADR-00N`; do not rewrite
  it into its opposite.
- **Name the intent.** If no principle fits, either the decision is out of scope or the intent is
  missing something — raise that before accepting the record.
