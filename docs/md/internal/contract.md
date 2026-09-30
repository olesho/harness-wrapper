# Harness Adapter Interface

`pkg/contract` is the Go interface a runtime uses to drive a harness without naming it: describe it,
render its configuration, open and reopen its sessions, send, interrupt, answer, observe with
acknowledgement, and read its record after a crash ([ADR-012](decisions/adr-012-harness-adapter-interface.md)).
The specification is
[Harness Adapter Interface v1](https://coplan.olehluchkiv.com/d/engine-contract-v1-specification); this
package is its normative form, contract version `harness-adapter/1.1`.

Minor 1 adds two things, each behind a capability
([ADR-013](decisions/adr-013-session-load-and-own-turns.md)): a saved Session **loaded** into a fresh
environment (`session_load`), and the turns a harness **starts by itself** (`autonomous_turns`). A
1.0 caller meets neither: it sends no `load`, and the observations of a harness's own turn name no
input it sent.

## The packages

| Package | What it is |
|---|---|
| `pkg/contract` | The types, the `Adapter` / `Session` / `Record` interfaces, errors, bounds, the registry (`Register`, `Lookup`) and the generated JSON Schema (`schema.json`, embedded as `contract.Schema`). Standard library only, so importing it links nothing else of hw (`TestStandardLibraryOnly`). |
| `pkg/contract/conformance` | The conformance kit: a fake Agent Adapter — a Supervisor that applies what `Provision` renders, and a Host that observes, commits and acknowledges — plus scenarios. |
| `pkg/contract/fakeadapter` | An adapter for a harness that lives in the process: the kit's reference, and a stand-in for a runtime's own tests. |

## Two callers

- The **Supervisor** calls `Describe` and `Provision`. `Provision` is pure: it turns a harness-neutral
  `AgentSpec` into files, an opaque `open_config` and the paths archives include and skip. The
  Supervisor writes the files — beneath their roots, never through a symlink, with modes no wider
  than `0644` (`ProvisionResult.Validate`, `conformance.Apply`).
- The **Host** opens Sessions (`NewSession`, then `Open`) and record handles (`OpenRecord`), feeds a
  Session one input at a time, and acknowledges each batch of observations only after the
  Supervisor has committed it.

A profile registers its adapter under its harness's name in `init`; a runtime links the harnesses it
offers through one file of blank imports and finds them with `contract.Lookup`.

## Loading a saved Session

A Session is saved as files once its harness has stopped: every regular file beneath the
Provision result's `history_roots`, less its `secret_paths`. To continue it in another environment —
other roots, another machine, the source gone — the Supervisor:

1. calls `Provision` with `load`: the archive's format, the saving harness's name, version and
   adapter, the source's roots, and the source's workspace as the harness resolved it. `Provision`
   refuses a source the Descriptor's `load` does not name (`unsupported`, naming the field), and
   answers `history_relocations`: prefix rules that say where the saved history goes when that is
   not where it was. The layout's paths are resolved in such a request: `Provision` is pure and
   resolves none;
2. restores each saved file at `contract.Relocate`'s answer, which must lie beneath a history root
   and outside every secret path (`ProvisionResult.Archived`), and rewrites nothing inside it;
3. reads the restored record to its end with `OpenRecord` and no checkpoint, publishing nothing: the
   checkpoint that read ends on is where the new agent's history begins. A record that reads empty
   is the sign, before any open, that the history is not where the harness looks;
4. reopens the Session with `loaded`, its saved id and that checkpoint.

An open with `loaded` never starts a fresh conversation: it fails with `session_not_found` when the
harness's record is not where the harness looks, the id it returns is the saved one, and — the first
time in the new environment — it fails with `state_mismatch` when what the harness keeps of the
Session outside its record did not come with it. No checkpoint is saved: a checkpoint belongs to
the file it was made on.

The Descriptor's `load` lists the archive formats and the harness versions whose saved Sessions the
adapter continues. A version is a source only while a Session it saved, kept as a fixture, passes
the kit's load checks at the adapter's own version; never because it is older.

## Turns a harness starts itself

A harness with `autonomous_turns` may start a turn no input asked for. Such a turn is reported as
`turn_started` and `turn_ended` whose key and `turn_id` are the turn's id, with no `input_id`, and so
is everything the turn says. While it runs the Session is `busy` with `State.turn_id` and no
`State.input_id`.

- `Send` is legal then. The adapter stops the harness's turn — it ends `interrupted` — and then
  submits the input, whose turn is the input's alone.
- `Interrupt` may name the turn by `turn_id`. Once it stops, the harness starts no turn of its own
  until an input's turn completes or the Session is reopened.
- The record proves such a turn's end under the same id, so a Supervisor that lost the live
  `turn_ended` to a crash finds it in the record.

## harness-wrapper's Harness Adapter

`pkg/adapter` is hw's implementation of the interface: one adapter, with a **profile** per harness.
It is the shared part, and names no harness:

- **Sessions.** The states, one outstanding `Send`, the admission gate (a usage, auth or billing
  error closes it; a usage wall with a known reset opens it again then, and nothing is retried),
  an `Interrupt` that names its input and is ordered after an outstanding send, `Answer`, and
  `Close` with `stopped` and `drained`. An uncertain send keeps the Session busy until its turn ends.
  A turn the transport reports as the harness's own (`adapter.SelfStarter`) is a turn with no input:
  the Session is busy while it runs, admits a `Send` — the transport stops that turn before it
  submits — and interrupts it by its turn id (`adapter.AutoTurnID` of the harness's id for it).
- **Loads.** `Provision` checks a request's `load` against the Descriptor (`contract.CheckLoad`) and
  the profile's relocations against the result. An open with `loaded` that comes back under another
  id than the saved one fails with `session_not_found`.
- **Observe and Ack.** One cursor over the live events and the record, in the order learned. A
  record chunk may span batches: its reset, rescan and faults go with its first item, its
  checkpoint with its last, and the reader moves past it only once that batch is acknowledged.
- **Submission markers**, one per input under `layout.scratch/markers`, synced before the harness
  gets the input. `OpenRecord` and `Recover` rest on them: only an intact store's missing marker
  proves an input never ran. A send the transport refused before anything reached the harness
  withdraws its marker: the input id is free to be sent again, and `Recover` says `not_found`.

A harness process's environment is its `open_config`'s, its credential, and what `adapter.HostEnv`
takes from the Host: `PATH`, `LANG`, `LC_*`, `TZ`, and the variables `HW_HARNESS_ENV` names — a
Supervisor's harness-neutral way to pass a setting `Provision` could not render, such as a test's
model API. Never a credential.

A profile (`adapter.Profile`) supplies what is its harness's own: the Descriptor, `Provision`, a
`Transport` to the running harness (submit, interrupt, answer, stop, and its events) and a
`Reader` of its record (chunks of record-origin observations, commit, and the evidence for
`Recover`). It registers with `adapter.Register` under its harness's name.

## The Claude Code profile

`pkg/adapter/claudecode` registers `claude-code`. Its harness distribution, under `harness_root`, is
the pinned claude (`bin/claude`) and the profile's hook helper, `cmd/claude-code-hook`
(`bin/claude-code-hook`).

- **Provision** renders what agentd rendered before it (`TestProvisionMatchesAgentdProfile` holds the
  two side by side): `settings.json` with the hooks, `.claude.json` with onboarding, bypass and
  workspace trust answered, the persona, skills, memory, `mcp.json` and the workspace's `CLAUDE.md`,
  and `open_config` with claude's arguments and environment. Each hook runs the helper, which writes
  what it reports to the spool: the scratch root itself, beside the markers' directory, so a spool a
  host kept before this profile, at the root it now names scratch, is read where it is.
- **Transport:** stream-json, one claude process per Session in a process group of its own. A fresh
  Session starts under its id (`--session-id`); a reopen resumes its transcript (`--resume`), or,
  when claude never wrote one — the launch that opened the Session ended before its first entry —
  starts under its id as a fresh one would. A reopen with `loaded` does not: with no transcript where
  claude looks it fails with `session_not_found` before claude is launched. An input
  is a user message whose uuid is the input's native id, a fresh UUID kept in its submission marker;
  claude's `command_lifecycle` receipt returns `Send`, and its transcript keeps the uuid as the prompt
  entry's. A turn ends with claude's `result`: by `is_error` and `terminal_reason`. `cancelled` needs
  claude's word that the message never started — claude says `cancelled` after the result of a turn it
  interrupted or failed, too. A failed turn is classed by the synthetic message's tag, the HTTP status
  and, for a 429, whether the account refused it: a usage wall (`You've hit your … limit · resets …`)
  closes the gate until its reset; the server's 429 (`not your usage limit`) is an `api` error.
- **Record:** the session transcript, followed from the checkpoint, and the hook spool. Checkpoint
  format 1 is the transcript follower's checkpoint — the one agentd stored — so stored checkpoints
  resume where they stood (`TestNodeDBCheckpoint`). Entries become `user_input`, `assistant_text`,
  `tool_use`, `tool_result` and `api_error`, keyed by the entry's uuid (and block) or the tool use id,
  with `entry` set to the entry's uuid. A turn's end is in the record as its final assistant entry
  (`stop_reason: end_turn`), a synthetic API-error entry or an interrupt entry, each a record-origin
  `turn_ended`. Spool files become `tool_started`, `tool_finished` and the subagents' start and stop;
  a file is deleted once its chunk is acknowledged, or at once when it reports nothing.
- **Recover** finds the prompt entry by the marker's native id, then that evidence: without either,
  `unknown`.
- **Load:** the history is `config/projects` and `config/memory`. claude names a working directory's
  transcripts for its resolved path, so one relocation moves `config/projects/<source workspace>` to
  `config/projects/<new workspace>` — a subagent's transcript, beneath it, with it — and nothing
  inside is rewritten. The profile loads the Sessions the pinned claude saved.

`TestClaudeConforms` runs the conformance kit, and `TestClaudeObservations` the profile's own
checks, against a real claude driving `internal/mockapi` — a Go port of agentd's P11 mock Messages
API — when `HW_REAL_CLAUDE` names the pinned binary. `TestClaudeLoadsSavedSessions` loads the Session
each source version saved (`testdata/load`). The `harness-adapter` workflow runs them on Linux with
the pinned claude it downloads and verifies.

## The Codex profile

`pkg/adapter/codex` registers `codex`. Its harness distribution, under `harness_root`, is the pinned
codex (`bin/codex`): the native binary from its npm package's vendor directory, never the node shim,
whose death would leave the native process holding the thread.

- **Provision** renders `CODEX_HOME` in the config root: `config.toml` — the model and effort, no
  approvals and `danger-full-access` (the runtime's isolation is the sandbox), a credential store in
  memory only, no plugins, apps or analytics, and an MCP server per connector — `AGENTS.md` with the
  persona and where the memory directory is, the skills and the memory's files; and the workspace's
  `AGENTS.md`. The credential kinds are `openai_api_key`, which the transport hands codex over the
  protocol (`account/login/start`), and `codex_access_token`, which codex reads from
  `CODEX_ACCESS_TOKEN`.
- **Transport:** `codex app-server`, JSON-RPC 2.0 on stdio, one process per Session in a process group
  of its own. codex chooses a thread's id, so a fresh Session opens without one (no
  `assign_session_id`); a reopen resumes the thread (`thread/resume`), or starts a new one when codex
  never wrote the thread's rollout. An input is a `turn/start` whose `clientUserMessageId` is its native
  id; the response is the receipt, and the rollout records the id with the input. codex folds an input
  sent during a turn into that turn, so the transport, like the Session, keeps one turn in flight. A
  turn ends with `turn/completed`: `completed`, `interrupted`, or `failed`, classed by its
  `codexErrorInfo`; `error` notifications with `willRetry` are its retries, and
  `account/rateLimits/updated` its usage, whose full window says when a usage wall lifts. codex refuses
  to interrupt a turn it has not made active, so an interrupt asked before `turn/started` goes once it
  comes; the turn's end, never `turn/interrupt`'s answer, settles it.
- **codex's own turns.** A thread with an active goal makes codex start turns by itself: when the
  thread resumes, and after every turn that completes ([the probe](../../../probes/codexturns/FINDINGS.md)).
  The transport follows the turn codex is on and binds an input to its turn by the id `turn/start`
  answered with, or by the input's `userMessage` item; a turn bound to no input is codex's own.
  `Submit` interrupts the one that runs, waits for one that is due, and only then sends `turn/start`:
  interrupted, codex starts none until the input's turn ends. Should codex fold an input into a turn
  of its own all the same, that turn ends where it took the input in, and the rest is the input's. An
  input dropped with an interrupted turn is sent again, or ends `cancelled` when the interrupt was of
  the input. `Close` on a codex that is on no input's turn closes its stdin: codex aborts a turn of
  its own, says so in the rollout, and exits.
- **Record:** the thread's rollout under `CODEX_HOME/sessions`, followed from the checkpoint once codex
  writes it with the first turn (`pkg/transcript/codex`). A turn begins at `task_started` and belongs to
  the input whose client id its user message carries — `user_message` in codex 0.144,
  `item_completed`'s `UserMessage` in 0.157. Replies become `assistant_text`, tool calls `tool_use` and
  `tool_result`, and the turn's end a record-origin `turn_ended`: interrupted at `turn_aborted`,
  errored at a `task_complete` with an error (0.157), completed at one with a reply. A `task_complete`
  with neither — codex 0.144's failed turn — proves no outcome.
  A turn with no user message is codex's own: its items and its end name the turn and no input.
- **Recover** finds the input's user message by the marker's native id, then its turn's end: without an
  end that proves an outcome, `unknown`.
- **Load:** the history is the rollouts and the memory, and what codex keeps of a thread outside its
  rollout: `session_index.jsonl` (its name), `goals_1.sqlite` and `goals_1.sqlite-wal` (its goal:
  codex can exit with the goal's row in the log alone). A thread resumes by its id in any working directory, so
  nothing is relocated. The rollout cannot say what a thread's goal is, so the profile keeps what
  codex last said of the name and the goal in `scratch/native/<thread>.json`, itself history. Opening a
  loaded thread for the first time in a `CODEX_HOME`, the profile asks codex for both (`thread/read`,
  `thread/goal/get`) before `thread/resume` and fails with `state_mismatch` when they are not what was
  saved, or when no account of them was. The profile loads the threads the pinned codex saved.

`TestCodexConforms` runs the conformance kit against a real codex driving `internal/mockapi`'s
Responses API when `HW_REAL_CODEX` names the pinned binary; the other `TestCodex…` tests hold codex's
own turns, the fallbacks, and the name and goal of a loaded thread, and `TestCodexLoadsSavedThreads`
loads the thread each source version saved (`testdata/load`). The `harness-adapter` workflow runs
them on Linux with the pinned codex it downloads and verifies.

## Running the kit

```go
func TestConformance(t *testing.T) {
	conformance.Run(conformance.Testing(t), conformance.Fixture{
		Adapter:     myAdapter,
		HarnessRoot: root,
		Spec:        contract.AgentSpec{PermissionPosture: contract.PostureBypass},
		Credential:  stageCredential, // nil for a harness that needs none
		Provisioned: pointAtMockAPI,  // e.g. ANTHROPIC_BASE_URL for P11's mockapi.py
		Kill:        killHarness,     // crash the harness process without Close
		HideBinary:  hideBinary,      // optional
		Heard:       lastModelInput,  // optional: what the harness last sent its model
	})
}
```

The scenarios speak the prompt language of agentd's P11 mock Messages API — `PING`, `SLOW`,
`STALL`, `TOOL`, `ERR <code> <k>`, `BIG`, `ASK`, and `LIMIT` for a usage wall (a 429 the account's
limit refused, which a harness does not retry; `ERR 429 <k>` is the server's load, which it does) —
so a real harness runs them against such a mock. A harness with `autonomous_turns` also takes
`MKGOAL <text>`, which gives the Session a goal, and `GOAL <n> <k>`, a goal's objective: `n` turns of
`k` steps, then done.

The load scenarios play the Supervisor's part with `conformance.Save` and `conformance.Restore`, in a
second set of roots, the first removed: the record read with no harness, the strict open, only new
items, the saved conversation in the model's next request (`Fixture.Heard`), a reopen, and a harness's
own work taken up again. `conformance.LoadSaved` runs the same checks on a Session saved earlier and
kept as files (`RecordSavedSession`, `WriteSavedSession`, `ReadSavedSession`): how a profile proves
each source version it names.
Each check names its rule (`[interrupt.other-turn] …`), and `TestBrokenAdaptersFail` breaks each rule
in the fake adapter (`fakeadapter.Options.Break`) and requires the kit to fail exactly that rule.
A scenario a harness cannot run is named in `Fixture.Skip`, with the reason.
