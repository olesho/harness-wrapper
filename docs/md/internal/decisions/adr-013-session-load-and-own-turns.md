# ADR-013: a saved Session loads into a fresh environment, and a harness's own turns are turns

**Status:** Accepted (2026-09-30)

**Intent:** principle 2, *a wrong verdict is worse than no verdict*, principle 3, *normalize, don't
leak*, principle 4, *prefer the harness's own record*, and principle 6, *evolve public contracts
deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 2, a loaded Session
that cannot be continued as it was saved does not open: it never becomes a fresh conversation under
the old name, and never loses a goal without saying so. Under principle 3, where a harness keeps a
Session's files and what it starts by itself stay behind the interface. Under principle 4, a turn the
harness started is read from its record as one. Under principle 6, all of it is a minor version:
optional request fields, two capabilities and one open failure, which a 1.0 caller never meets.

## Context

agentd saves an agent's conversation as an archive of the files its Harness Adapter names, and loads
it into a new agent, on the same machine or another
([Save and load an agent's conversation](https://coplan.olehluchkiv.com/d/save-and-load-an-agents-conversation-cla)).
Interface 1.0 could name the files (`history_roots`, `secret_paths`) and nothing else:

- it could not say where they go when every root has another path. claude keeps a working
  directory's transcripts in a directory named for that path;
- a reopen with no record started a fresh conversation, which suits a launch that ended before its
  first entry, and nothing else. For a load it is the one outcome that must not happen;
- codex keeps a thread's name and goal outside the rollout, and a copy without them resumes with the
  whole conversation and neither, silently
  ([the feasibility probe](../../../../probes/saveload/FINDINGS.md)).

A goal raised a second problem. A thread with an active goal makes codex start turns nobody sent an
input for ([the probe](../../../../probes/codexturns/FINDINGS.md)). The 1.0 transport bound an input to
the turn id `turn/start` answered with; with a goal turn running that id never starts, because codex
folds the input into the running turn, so the input's turn never ended. And a Session saved with an
active goal is loaded with one.

## Decision

1. **Interface 1.1 loads a saved Session** (capability `session_load`).
   - `ProvisionRequest.load` carries what the archive says of its source: the format, the saving
     harness's name, version and adapter, the source's roots, and its workspace as the harness
     resolved it. `Provision` stays pure, so the layout's paths arrive resolved.
   - `ProvisionResult.history_relocations` are prefix rules from saved paths to their places in the
     new environment. Both ends are history and no secret path; rules do not overlap. A path no rule
     names keeps its place. Nothing inside a file is rewritten, but for the one value a 1.7
     `history_rewrites` rule names ([ADR-023](adr-023-history-rewrites.md)).
   - `OpenRequest.loaded` makes a reopen strict: `session_not_found` when the record is not where
     the harness looks, the saved id or none, and, on the first open in the new environment,
     `state_mismatch` when what the harness keeps of the Session outside its record did not come
     with it.
   - No checkpoint travels. A checkpoint names the file it was made on; the Supervisor reads the
     restored record to its end, with no harness running, and starts from there.
2. **An adapter says which saved Sessions it loads.** `Descriptor.load` lists archive formats and
   source harness versions. A version is listed while a Session it saved, kept as an immutable
   fixture, passes the conformance kit's load checks at the adapter's own version
   (`conformance.LoadSaved`); never because its number is lower. The profiles start with their own
   pinned version.
3. **A turn the harness starts itself is a turn** (capability `autonomous_turns`). It is reported as
   `turn_started` and `turn_ended` keyed by a turn id of its own, with no input; the Session is busy
   while it runs; `Interrupt` may name it by `turn_id`.
4. **An input preempts the harness's own turn.** `Send` during such a turn is legal: the adapter
   stops the turn, which ends `interrupted`, and then submits the input, whose turn answers the
   input and nothing else. An interrupt stops the harness's own work until an input's turn
   completes or the Session is reopened.
5. **The Claude Code profile** relocates one directory, `config/projects/<source workspace>`, to the
   one the new workspace names, and refuses a loaded open with no transcript before it launches
   claude.
6. **The Codex profile** saves a thread's name and goal with it (`session_index.jsonl`,
   `goals_1.sqlite`, `goals_1.sqlite-wal`), relocates nothing, and keeps its own account of both,
   `scratch/native/<thread>.json`, as history. It writes that account as codex reports a change and
   once more as the thread parks. Opening a loaded thread for the first time in a `CODEX_HOME`, it
   asks codex for the name and the goal before `thread/resume` and refuses, with `state_mismatch`,
   a thread that lost either — or one saved with no account at all.
7. **The Codex transport follows the turn codex is on**, from `turn/started` to `turn/completed`,
   and binds an input by the id `turn/start` answered with or by the input's `userMessage` item.
   `Submit` interrupts a goal turn that runs and waits for one that is due before it sends
   `turn/start`. If codex folds the input into a goal turn all the same, that turn ends where it
   took the input in; an input dropped with an interrupted goal turn is sent again. A codex on no
   input's turn is stopped by closing its stdin, so a goal turn ends in the rollout.

## Alternatives

- **Let an input join the running goal turn.** It is what codex does with `turn/start` during a
  turn. The reply then answers the goal's prompt and the input together, the turn's outcome is the
  goal turn's, and an interrupt before codex takes the input in drops the input. A caller that sent
  one input would be told about a turn it did not ask for.
- **Refuse inputs while the harness works for itself.** A goal chains turns until it is complete:
  the agent would be unreachable for as long as its goal lasted.
- **Turn goals off** (`goals = false`), or refuse to save a thread whose goal is active. Both keep
  the 1.0 transport and lose a feature codex users have; the second makes a Save fail for a reason
  the agent's owner cannot see from outside.
- **Read a thread's goal from its rollout** instead of keeping an account of it. The rollout logs a
  goal a client sets, and not one the model made, changed or completed.
- **Accept a loaded thread with no account of its name and goal.** It would load, and a goal lost on
  the way would be lost silently, which is the failure the check exists for. Such a thread was last
  opened by an adapter older than this decision; opened once by this one and saved again, it loads.
- **Carry the checkpoint in the archive.** It names an inode; on the restored file the reader
  reports a reset and reads the record again from its start.
- **Compatibility by version order.** "Any newer version loads an older Session" holds until a
  harness moves a file. A listed version is one that was tried.

## Boundary

Guaranteed: a loaded Session opens under its saved id with its record where the harness reads it, or
does not open; a codex thread opens with the name and the goal it was saved with, or does not open;
an input's turn holds that input alone, except in the instant codex starts a goal turn before it
reads the input, where the turn is the input's from its message on.

Not guaranteed: that a harness's own turn the input stopped is resumed where it was — codex starts a
new one; that a Session saved by one harness version loads at another, until that pair has a
fixture; that an archive holds no secret — a transcript may quote one.

## Evidence

- `probes/saveload`: the save and load recipes, for claude 2.1.283 and codex 0.144.5, on macOS and
  Linux, against the mock and each live API.
- `probes/codexturns`: codex's own turns, an input during one, stopping, and what a thread keeps of
  its goal; each finding is a test against the pinned codex.
- The conformance kit's `load`, `load-missing`, `load-refused`, `autonomous` and `load-autonomous`
  scenarios pass against the fake adapter, the shared adapter over a fake profile, and both pinned
  binaries driving the mock; each rule has a fake adapter that breaks it and fails it.

## Consequences

- `pkg/contract` is `harness-adapter/1.1`. A 1.0 caller sends no `load`, no `loaded` and no
  `turn_id`, and is served as before — but a codex thread with a goal now reports turns with no
  input to it too, so a caller that keys every turn on `input_id` has to tolerate a `turn_started`
  with none. agentd, the one caller, moves with this release.
- The Codex profile's history grew by three files and a directory. An archive made by an older
  adapter lacks them, and a thread saved without its native-state account is refused.
- Every Session a Supervisor loads is opened with `loaded` from then on, so a later reopen that
  finds no record fails rather than starts over.
- A version joins `Descriptor.load` with a fixture recorded by that version
  (`testdata/load/<version>`), never regenerated by a later one.
- An input sent to a codex that works on its goal costs the model request of the goal turn it
  stops.

## Follow-ups

Add a claude or codex version to a profile's sources when its pin moves: record the old version's
fixture before the move, and keep it.

## Amendment (2026-10-08): codex 0.160.0's answer, and an input codex may hold

Point 7 binds an input by the id `turn/start` answered with because codex 0.144.5 answered an input
it folded into a goal turn with an id that never started. codex 0.160.0 answers with the running
turn's id, as it answers an input's own turn. So in the instant codex starts a goal turn before it
reads the input, the turn's `turn/started` coming first, the transport takes the turn as the input's
from its start rather than from its message on, live; the rollout still tells them apart. The
boundary above narrows accordingly. It costs attribution, not an input.

codex deduplicates no `clientUserMessageId` (`probes/codexturns`, `TestClientIDIsNoKey`): an input
sent again runs again, so sending one again is safe only where codex surely dropped it. A turn
interrupted before it took in the input it held drops it, and the transport sends that input again,
as before. A turn that ends otherwise takes in the input it holds before it ends
(`TestInputAtATurnsEnd`, `TestInputAtAGoalTurnsEnd`, `TestInputIntoAFailingTurn`), so one that
ended without it means the transport missed the input's message, or a codex that holds it still.
The transport sent such an input again after a few seconds; it now ends it `errored` (`internal`)
instead, unless a turn takes it in meanwhile.

## History

- 2026-10-09: #1 narrowed by [ADR-023](adr-023-history-rewrites.md): a 1.7 rewrite may change one
  value in a saved file's first line.
