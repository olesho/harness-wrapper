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
| [ADR-009](adr-009-stream-json-transport.md) | claude-code can run on its stream-json protocol behind the same `Conversation`: turns end on `result` frames, sends and interrupts on claude's receipts | Accepted 2026-09-24 | 1, 4 · also 2, 3, 6 |

## Intent coverage

| Principle | Records | How the decision applies it |
|---|---|---|
| 1 · The screen is a contract we don't own | 001, 001-TS, 002, 007, 009 | Emulators are chosen by replaying recorded real sessions, and the dialog detector was re-verified live when claude 2.1.251 changed the dialog's shape; every interrupt shape was captured on the pinned claude before code keyed on it; claude-code can leave the screen for its stream-json protocol, whose frames were recorded on three systems before the driver keyed on them |
| 2 · A wrong verdict is worse than no verdict | 002, 006, 007, 008, 009 | A dialog the adapter cannot parse is reported as unparseable and blocks; it is never guessed at, and never answered with empty keys. A caller that owns the harness's lifetime is told what a classification found and decides; silence is not evidence, and a verdict never outlives its evidence. A turn ends interrupted only when the harness says it stopped, read per turn. No turn's end, and not the harness's exit, is lost to a full channel |
| 3 · Normalize, don't leak | 002, 007, 009 | One `InputRequest` vocabulary for every harness; clients answer an option id or alias, and keystrokes stay in the per-harness adapter. One `interrupted` turn state, with the interrupt keys and the screen reading in the adapter; the stream-json transport reports in the same `Turn`, `InterruptResult` and `InputRequest` vocabulary |
| 4 · Prefer the harness's own record | 001, 007, 009 | Where emulator fidelity falls short, turn text comes from the harness's transcript and the screen serves liveness only; an interrupted turn's partial reply is the transcript's once it records the interrupt; on stream-json a turn's end, text and error tag are the harness's own frames |
| 5 · Keep the stack one-way and the core transport-free | 002 | Detection sits in `pkg/turns`, the channel in `pkg/chat`, HTTP + SSE only in `cmd/harness-chatd` |
| 6 · Evolve public contracts deliberately | 002, 003, 005, 007, 008, 009 | SSE frames gained a `type` field additively; no Go surface is published before a consumer can shape it; a policy without the AppArmor layer serializes, and fingerprints, exactly as before; the interrupt route and wire state came with their conformance fixtures; `OnEvent` was added beside `Events()`, whose contract stayed; the stream-json transport is an option beside the TUI, the default |
| 7 · Say what is enforced, not what is intended | 002, 004, 005, 008 | Accepting a skip-all-permissions launch is its own policy kind; both containment records list what is guaranteed and what is not; a launch refuses rather than degrades and reports the rule it enforced; event delivery states its promise — in order and once while the process lives — and what it does not cover |

No record yet covers the `Status` vocabulary, the `ErrorClass` taxonomy and which matcher may classify
what (principle 2 — ADR-006 decides only who acts on a classification); the transcript parsers (4);
the layering and import rules (5); the frozen `turnproto` contract and the conformance corpus shared
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
