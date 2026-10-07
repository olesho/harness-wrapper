# ADR-022: Sessions side by side

**Status:** Accepted (2026-10-06)

**Intent:** principle 6, *evolve public contracts deliberately*, and principle 2, *a wrong verdict is
worse than no verdict* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 6, several
Sessions of one agent open at once are a capability of interface 1.6, and an adapter declares no
more of them than it has run. Under principle 2, a Session's record holds its own Session alone: no
reader takes a sibling's events, and a Session another Host runs is refused rather than run twice. It
also applies 3 (what keeps two claude or codex processes apart in one environment is the profiles' to
know) and 7 (the lock is the Host's, and what it does not cover is said).

## Context

agentd lets one agent hold several chats at once, each a Session in a Host of its own
([agentd: parallel Sessions in one agent](https://coplan.olehluchkiv.com/d/agentd-parallel-sessions-in-one-agent)).
The Sessions of an agent share its Layout: one config root, one workspace, one scratch root, one
staged credential. hw assumed one Session of an agent open at a time:

- Claude Code's hooks wrote to one spool, the scratch root, and each record reader took the whole of
  it as its one consumer: a Session's reader took its siblings' tool and subagent events, which
  never reached their own Session.
- The submission-marker store was made in two steps, the directory and then its sentinel. A Session
  opening between them found the store not intact, and its first send failed `maybe_submitted`.
- codex 0.160.0's app-servers started together on a fresh `CODEX_HOME` fail to initialize its state
  database ([openai/codex#50290](https://github.com/openai/codex/issues/50290)).
- Nothing kept two harness processes off one Session. In stream-json mode claude 2.1.283 resumes a
  session another claude holds in place: both name the same id, and both write one transcript
  ([anthropics/claude-code#97833](https://github.com/anthropics/claude-code/issues/97833)). claude
  2.1.283 also has a path that starts a copy of a held session under an id of its own; a transport
  that followed the Session's id would follow a transcript that never grows.

## Decision

1. **Interface 1.6 adds `concurrent_sessions`.** Several Sessions of one agent may be open at once,
   from one Host or a Host each, over one Layout and one staged credential. Each keeps its own
   inputs, turn, record, observations and checkpoint; what one does reaches no other; a record
   handle on one may be open while the others run. `Limits.MaxSessions`, at least 2, says how many:
   the most the adapter's concurrency conformance has passed with the real harness at its pin, run
   again at every pin move. `contract.CheckSessions` holds the form, and `Descriptor.Sessions` is the
   number, 1 without the capability.
2. **One Host per Session.** While a Session's harness runs, its Host holds an flock on the Session
   under `scratch/sessions`; an `Open` of the Session in another Host fails `session_in_use` before a
   second harness starts.
3. **The marker store is made whole**, beside its place and renamed in; a Host that loses the race
   drops its own.
4. **Claude Code spools per Session.** The transport points each claude at
   `scratch/spool/<session id>` through the process's own `HW_EVENT_SPOOL`, and arms the hook
   helper's session guard on every launch. A reader takes its own spool, and at the scratch root only
   its own Session's files, which a host kept there before. A claude whose `system` frames speak for
   another session is stopped: before it answered `initialize`, `Open` fails `session_in_use`; at a
   turn's `system/init`, that turn ends `errored`.
5. **Codex starts take turns**, at an flock in the scratch root, from the app-server's start until it
   answers `initialize`: every start, since a pin move migrates the databases as codex starts.
6. **The kit's `concurrent-*` scenarios** hold the rules, with as many Sessions as the Descriptor
   allows where the number is the point, and the fake adapter breaks each rule.
7. **Claude Code and Codex declare the capability with `MaxSessions` 8.** Pi does not: it has one
   Session of an agent open at a time until a probe of its own passes.

## Alternatives

- **A config root per Session** (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`): isolated by construction, but it
  splits the agent's memory, skills, login and history by Session, and a save and a load would place
  each. The step-0 probe found the shared files hold: `.claude.json` was never torn, every
  transcript and rollout held its own Session, and codex's databases passed their integrity check.
- **A spool per Session through `settings.json`**: the hooks' configuration is the agent's one file.
  The process's own `HW_EVENT_SPOOL` leaves it alone.
- **Serializing only the first codex start on a fresh home**: a pin move migrates the databases as
  codex starts, and N starts in one environment cost N initializations, well under a second each.
- **The `system/init` id check alone, to keep one claude per Session**: claude resumes a held
  session in place under the same id, which no id check sees. The lock refuses the second Host
  before its claude starts.
- **One Host for all of an agent's Sessions**: agentd's plan gives each Session a Host and a unit of
  its own, so one Session stops, and is confirmed stopped, alone. hw's code is the same either way.

## Boundary

Guaranteed, within one environment (one scratch root):

- No two Hosts run one Session's harness at once: the second `Open` fails `session_in_use` before
  anything starts.
- A Session's record reader delivers its own Session's hook events, and its subagents', and no
  other's.
- No Session finds the marker store half made.
- Codex app-servers initialize one at a time.

Not guaranteed:

- The lock is the Host process's. A harness that outlives its Host, its process group not ended, is
  not covered; agentd ends a Host's unit with everything in it.
- Hosts of one Session in two environments, or the fake adapter's Sessions in two processes, do not
  see each other.
- A claude that resumes a held session in place is kept out by the lock alone; the id check catches a
  copy only.

## Evidence

- **Step 0** (`probes/sessions`, hw #107): 4 and 8 Claude Code Sessions for 30 minutes against the
  mock, and 4 for 5 minutes live; some 27,000 hook events, each in its own Session's spool. 4 and 8
  Codex Sessions for 30 minutes; started together on a fresh home, 1 of 2, 1 of 4 and 7 of 8
  app-servers failed, and behind one that started first none did.
- **The kit at 8 Sessions** passes with claude 2.1.283 and codex 0.160.0 on macOS and on Ubuntu
  26.04, both arm64. With the
  start lock taken out, 5 to 7 of 8 app-servers failed `concurrent-open` with codex#50290's error;
  with one spool read whole, Sessions were delivered each other's tool events
  (`concurrent.isolated`).
- **claude 2.1.283 in stream-json mode**, resuming a session another claude held: every frame of
  both named the same id, and both wrote one transcript, 27 lines from the turns of two processes.

## Consequences

- A runtime counts an agent's open Sessions against `Descriptor.Sessions()` and its own capacity.
  Adapters do not enforce the limit: past it their behavior is unspecified and untested, since
  `MaxSessions` states what has been verified rather than a ceiling (made explicit 2026-10-07).
- Each Session is a harness process: a busy one peaked near 500 MiB with Claude Code and 380 MiB
  with Codex in the probe, so a runtime sizes its capacity by memory.
- `MaxSessions` is re-run, not carried, when a pin moves.
- Hook files a host left at the scratch root are read by the Session they name; any other's stay.

## Follow-ups

- Pi declares the capability once a probe of its own passes.
- At each claude pin move: whether stream-json mode starts copies of held sessions, which the id
  check would then stop.
