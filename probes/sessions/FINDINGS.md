# Sessions side by side: can several Sessions of one harness share an agent's environment?

**Probed:** claude 2.1.283 and codex 0.160.0, the versions hw pins, through
hw's Harness Adapter at `v0.31.0`, on 2026-10-06. Linux arm64 on two Lima VMs
(agentd-ubuntu: Ubuntu 26.04, kernel 7.0; agentd-debian: Debian 13, kernel 6.12), 4 vCPUs
and 3.9 GiB each, with the binaries agentd's bundle installs. Mock runs use
`internal/mockapi`. The live run is Claude Code on haiku under a `claude
setup-token` token. Codex ran against the mock only.

Rerun: [README.md](README.md). Each run's evidence is
`evidence/<platform>/<harness>-<mode>-n<N>.json`, with its tables beside it as
`.md`.

**Question,** from Step 0 of
[agentd: parallel Sessions in one agent](https://coplan.olehluchkiv.com/d/agentd-parallel-sessions-in-one-agent):
can N Sessions of one harness run at once in one agent's environment — one
config dir or `CODEX_HOME`, one workspace, one staged credential — each in a
Host of its own, without corrupting or losing what they share?

**Yes, for both, with the two changes the plan asks of the profiles.** Every
one of the plan's criteria passed in every run, once the probe's own faults
were fixed (see *The probe's faults*). With a hook spool per Session, Claude Code's hook
events never crossed Sessions. With a start lock, Codex's app-servers always
started; without it they did not.

## The criteria

| Criterion | Claude Code, 4 for 30 min | Claude Code, 8 for 30 min | Claude Code live, 4 for 5 min | Codex, 4 for 30 min | Codex, 8 for 30 min |
|---|---|---|---|---|---|
| No Session's start hangs | pass: 0.3–0.4 s at once | pass: 4.5–6.3 s at once | pass: 0.3 s at once | pass: 1.6–1.7 s at once | pass: 0.3–0.4 s at once |
| `.claude.json` parses after every write and keeps what was rendered | pass | pass | pass | | |
| Every transcript or rollout parses and holds only its own Session | pass, 6 | pass, 10 | pass, 6 | pass, 5 | pass, 9 |
| Every hook event lands in its own Session's spool | pass, 12,707 events | pass, 13,004 events | pass, 1,736 events | | |
| A connector's headersHelper still runs at the end | pass | pass | pass | | |
| Every app-server starts behind the start lock, and fails without it | | | | pass: 0 of 5 behind it; 1 of 4 without | pass: 0 of 9 behind it; 7 of 8 without |
| No turn fails on a busy database | | | | pass | pass |
| `session_index.jsonl`, when codex writes it, parses | | | | not written | not written |
| codex's databases pass SQLite's integrity check | | | | pass, 6 | pass, 6 |
| Input turns completed | 3,024 of 3,024 | 3,091 of 3,091 | 411 of 412 | 4,998 of 4,998 | 13,679 of 13,679 |

Each Claude Code Session's script cycled through a reply, a Bash call, a
subagent, a background command and the turn claude takes when it ends, and
streamed text. The one live turn that did not complete was a subagent's: the
model ended it with an empty reply, once in 82, with nothing of another
Session in it. Each Codex Session's cycled through a reply, a shell call,
streamed text, and a goal codex worked in two turns of its own.

## Claude Code

- **The hook spool per Session works.** The probe gave each Session its own
  spool by rewriting `HW_EVENT_SPOOL` and `spool` in its open configuration,
  and left `settings.json` as rendered: the hook command takes the spool from
  the claude process's environment. A wrapper around the hook helper kept a
  copy of every payload: each named the Session whose spool it reached. Every
  tool use's `tool_started` and `tool_finished`, and every subagent's start
  and stop, reached its own Session's Host and no other.
- **`.claude.json` was never torn, corrupt or missing an entry.** It was read
  every 20 ms. In print mode claude barely writes it: one change after the
  Sessions started, none during 30 minutes of turns, none at the end.
- **claude changes two of the rendered entries on its own.** A control Session
  alone, before the run, removed `bypassPermissionsModeAccepted` and the
  workspace's `hasCompletedProjectOnboarding`: claude 2.1.x keeps those
  elsewhere now. The workspace's `hasTrustDialogAccepted`, which a connector's
  `headersHelper` needs, stayed. The profile renders two keys that do
  nothing.
- **No transcript of no Session.** No resumed copy under another id appeared
  ([anthropics/claude-code#97833](https://github.com/anthropics/claude-code/issues/97833)
  needs a Session resumed while it runs, which a Host per Session never does).
- **Opens slow down under load, and never hang.** Eight claude processes
  starting at once on 4 vCPUs took 4.5–6.3 s, against 0.3–0.4 s for four.
  A Session opened at the end beside four busy ones took 4.7 s.
- **A turn of claude's own after a background command is not certain.** After
  4 of 1,222 background commands in the mock runs claude took none within 60
  s. That is claude's behaviour, not a crossing of Sessions; agentd's node
  does not wait for such a turn.

## Codex

- **The start race is real, and a start lock prevents it.** Codex 0.160.0
  app-servers started at once on a fresh `CODEX_HOME` failed: 1 of 2, 1 of 4,
  and 7 of 8, each with `failed to initialize sqlite state runtime`
  ([openai/codex#50290](https://github.com/openai/codex/issues/50290)). When one
  app-server started alone first, every later one started, at once, in every
  run.
- **`session_index.jsonl` is never written in agentd's use.** codex writes it
  when a thread is named, and agentd names none. The shared-file risk the plan
  listed for it does not arise.
- **The goals database and the other five held.** Every database passed
  `pragma integrity_check` after the run. Their write-ahead logs reached
  4–7 MiB in 30 minutes of four Sessions, and 4–8 MiB of nine (`logs_2`,
  `state_5`, `goals_1`); `thread_history_1` grew to 35 MiB.
- Each thread's rollout held only its own thread.

## Memory and pace

Peak resident memory, read from `/proc` every 2 s: a Session's harness and
every process beneath it — the Bash calls, a subagent's work:

| | Claude Code | Codex |
|---|---|---|
| A Session freshly opened | 217–266 MiB | 187–192 MiB |
| A busy Session, 30 min of turns, 4 at once | 460–490 MiB | 309–352 MiB |
| A busy Session, 30 min of turns, 8 at once | 414–442 MiB | 314–378 MiB |
| Every Session together, 8 at once | 3,292 MiB | 3,051 MiB |
| A busy Session's peak rate of observation batches | 13–14 a second | 24–29 a second |

- **Memory, not just processes, bounds the cap.** Eight busy Claude Code
  Sessions nearly filled the 3.9 GiB VM: 3,292 MiB of harnesses beside the
  probe and the OS. On a 4 GiB runtime four busy Claude Code Sessions fit with
  room; eight do not, once a real agent's stdio MCP servers come with each.
- **So does the CPU, for Claude Code.** Four and eight Claude Code Sessions
  ran about the same number of turns in 30 minutes (3,024 and 3,091): four
  busy Sessions, with the mock on the same machine, saturated four vCPUs, and
  eight each took turns at half the pace. Codex is lighter: nine Sessions ran
  13,679 turns where five ran 4,998.
- The pace a Host asks of its Supervisor is in agentd's probe: node.db keeps
  it at 8 Sessions with room to spare.
- **The Host's side is small.** The probe's own process, holding every
  Session's adapter, peaked at 145 MiB for eight Claude Code Sessions and the
  mock. A heap profile of the 8-Session Codex run, taken with every
  Session still open after 30 minutes and some 17,000 turns, held 74 MB: 53 MB
  the mock's, 8 MB the probe's records, and about 7 MB hw's adapter for all
  nine Sessions — a few hundred bytes a turn for sends, markers and turn ids,
  which a parked Session's Host gives back when it exits.

## The probe's faults

Some runs failed for three faults of the probe, not of a harness; each was
fixed and its run made again:

- It sent an input while a Session was busy, and spun on `busy`. agentd's
  node sends only to an idle Session and lets a turn the harness started itself
  run to its end; the probe now does the same.
- The mock kept every request it answered, with its system prompt and, on the
  Responses API, the whole conversation: a 4-Session Codex run grew the probe
  to 2.4 GB in 24 minutes and the kernel killed it. `mockapi.KeepRequests` now
  bounds what a long run keeps.
- A turn's end reached the probe twice — live, with its text, then from the
  record, without — and the second overwrote the first: the live run counted
  replies as empty. It now keeps the text.

## What this changes in the plan

- **Step 1's profile changes are confirmed as enough.** The spool per Session
  through the claude process's environment, and Codex's start lock, are the
  whole of what the two profiles need for Sessions side by side at these
  pins. Nothing else the Sessions share failed.
- **`MaxSessions`:** at these pins both profiles have passed 8. That is what
  each may declare.
- **The sizing rule's figures:** a busy Claude Code Session peaks near 500
  MiB, a Codex one near 380 MiB, before a real agent's MCP servers; and four
  vCPUs saturate at four busy Claude Code Sessions.
- **One risk the plan lists does not arise:** `session_index.jsonl`.

## Not covered

- Linux arm64 only: no x86_64 machine was free.
- Codex live: the mock only. A live run needs an API key or a workspace's
  access token, and spends the account.
- Stdio MCP servers: the probe's connector is an HTTP one. A real agent's
  stdio servers start with each Session's claude, and add to its memory.
- A crash of one Session while others run is not here: it is in the plan's
  conformance and crash-matrix work, with agentd's node in the loop.
