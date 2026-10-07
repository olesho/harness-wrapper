# Harness Adapter Interface

`pkg/contract` is the Go interface a runtime uses to drive a harness without naming it: describe it,
render its configuration, open and reopen its sessions, send, interrupt, answer, observe with
acknowledgement, and read its record after a crash ([ADR-012](decisions/adr-012-harness-adapter-interface.md)).
The specification is
[Harness Adapter Interface v1](https://coplan.olehluchkiv.com/d/engine-contract-v1-specification); this
package is its normative form, contract version `harness-adapter/1.6`.

Minor 1 adds two things, each behind a capability
([ADR-013](decisions/adr-013-session-load-and-own-turns.md)): a saved Session **loaded** into a fresh
environment (`session_load`), and the turns a harness **starts by itself** (`autonomous_turns`). A
1.0 caller meets neither: it sends no `load`, and the observations of a harness's own turn name no
input it sent. Minor 2 adds credentials kept from the harness by an **egress broker**
(`brokered_credentials`, [ADR-014](decisions/adr-014-brokered-credentials.md)), and a subscription
login the runtime **keeps** itself and lends behind that broker (`login_keeper`,
[ADR-015](decisions/adr-015-login-keeper.md)), which a 1.1 caller never asks for.

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
  than `0644` (`ProvisionResult.Validate`, `conformance.Apply`). For a credential it keeps from the
  harness it also calls `Placeholder`.
- The **Host** opens Sessions (`NewSession`, then `Open`) and record handles (`OpenRecord`), feeds a
  Session one input at a time, and acknowledges each batch of observations only after the
  Supervisor has committed it. An `Open` whose context the Host cancels, or whose deadline passes,
  fails `open_failed` with **no reason** (`contract.OpenAbandoned`, message `abandoned: …`): nothing
  about the harness or the config failed, and the Host, which owns the context, knows which it was.
  It then calls `Close`.

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

A harness with `background_turns` starts such a turn when work it began in the background ends — a
command or a subagent — to take that work's result up. It is reported the same way, but `Send`
answers `busy` while it runs, as during an input's turn: the turn carries the work's result, and an
input waits for it. Since 1.4 the work itself is reported, live, whenever it changes:
`background_tasks` lists every task still running (`id`, `kind` — `command`, `subagent` or `other`
— and `description`), and an empty list once none is left; `State.background` holds the same list.
A Session whose harness works on in the background is `idle` between turns, but not at rest: a
Supervisor that stops idle Sessions should wait for the list to empty.

## Credentials behind an egress broker

A runtime may keep a harness's credentials out of the harness's reach: it stages a placeholder where
the credential would be, fences the harness's network so that an egress broker is its only way out,
and the broker puts the credential in the placeholder's place in the requests sent where the
credential belongs. An adapter with `brokered_credentials` says what only it knows of that:

- `Descriptor.egress` names the hosts every Session of the harness reaches and, per credential kind,
  a route: the exact hosts the harness presents it to and the headers that carry it there
  (`contract.CheckEgress` holds the form). A kind with no route is never brokered.
- `Placeholder` renders what stands in for a credential: the file the Supervisor stages in its place,
  which `Open` reads as it reads the credential, and the swaps — placeholder, secret, hosts,
  headers — the Supervisor hands its broker. It is pure, a function of the credential and a nonce of
  the Supervisor's, and its result holds the credential's secrets: the Supervisor gives them to the
  broker alone and never journals or logs them (`Swap` prints without its secret).
  `PlaceholderResult.Validate` keeps each swap within its route and every secret out of the file.
  Since 1.5 the request names the agent's model: a harness whose kind serves several providers
  narrows each swap to the provider the model names, and refuses a model it cannot place
  (`invalid_spec` on field `model`) ([ADR-020](decisions/adr-020-placeholder-model.md)).
- The runtime gives every harness the broker's address in `HTTPS_PROXY` and the certificates to
  trust in `SSL_CERT_FILE`, through `HW_HARNESS_ENV`; a profile hands the certificates to its
  harness however it reads them.

The kit's `placeholder` scenario holds the rules, and opens a Session on the placeholder file.

## A login the runtime keeps

A subscription login's access token expires, and the refresh token that renews it rotates: a copy
that refreshes logs every other holder out. A runtime may keep such a login itself, outside every
agent, and lend it behind its broker (`login_keeper`):

- `Descriptor.keeper` names the kind a kept login is lent as, one its egress routes
  (`contract.CheckKeeper`).
- `Keep` opens a `Keeper` over a home of the runtime's keeper identity, which no agent reaches. It
  signs in with a device code (`SignIn`), which the person the login belongs to approves wherever
  they are; reports where the login stands (`Status`); has the harness's own client refresh it
  (`Refresh`), before its credential expires; lends the credential with nothing that refreshes it
  (`Lend`), for `Placeholder`; and signs out.
- A refused refresh fails and `Status` says why; once its credential expires the login is
  `expired` until someone signs in again.

The kit's `keeper` scenario runs a keeper whose sign-in the fixture approves (`Fixture.Approve`).

## Sessions side by side

Since 1.6 an agent may have several Sessions open at once (`concurrent_sessions`), from one Host or
a Host each, over one Layout and one staged credential. Each keeps its own inputs, turn, record,
observations and checkpoint, and what one does reaches no other: an interrupt, a close or a crash
of one ends nothing of the rest, and a record handle on one may be open while the others run.

- `Descriptor.limits.max_sessions` says how many may be open at once, at least 2
  ([ADR-022](decisions/adr-022-sessions-side-by-side.md)): the most the
  adapter's concurrency conformance has passed with the real harness at the version it pins. Like
  `load.sources`, it names nothing the adapter has not run, and it is run again whenever the pin
  moves. `contract.CheckSessions` holds the form; `Descriptor.Sessions` is the number, 1 without the
  capability.
- The limit is the **runtime's to keep**. An adapter does not count an agent's Sessions and need not
  refuse one past `Descriptor.Sessions`; what it does there is unspecified, and the conformance kit
  never opens more than the limit. The only refusal an adapter owes is per Session:
  `open_failed`/`session_in_use` while another Host holds the same Session.
- An adapter without the capability has one Session of an agent open at a time.

The kit's `concurrent-*` scenarios hold the rules, with as many Sessions as the Descriptor allows
where the number is the point and two elsewhere: Sessions open together on a fresh agent, each
under an id of its own, and one open in a Host is refused to another with `session_in_use`
(`concurrent-open`); their turns run at once, a reply, a tool call and streamed text, and nothing
one does is delivered to another (`concurrent-turns`); an interrupt, a close or a crash of one ends
nothing beside it, and a crashed one reopens while another runs (`concurrent-interrupt`,
`concurrent-close`, `concurrent-crash`); a record handle on one reads its own record alone while
another works, which still observes its own tool calls (`concurrent-record`); and Sessions saved
together load together, each opening under its saved id with its own conversation and not its
sibling's (`concurrent-load`).

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
- **One Host per Session.** While a Session's harness runs, its Host holds a lock on the Session
  under `layout.scratch/sessions`, and an `Open` of it in another Host fails with `session_in_use`
  before a second harness starts: two harness processes on one Session write its record from two
  sides. The lock goes with the Host's process; a harness that outlives its Host is the runtime's
  to end.
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
  The store is made whole, beside its place and renamed in, so Sessions that open together share
  it and none finds it without its sentinel.

A harness process's environment is its `open_config`'s, its credential, and what `adapter.HostEnv`
takes from the Host: `PATH`, `LANG`, `LC_*`, `TZ`, and the variables `HW_HARNESS_ENV` names — a
Supervisor's harness-neutral way to pass a setting `Provision` could not render, such as a test's
model API. Never a credential.

A profile (`adapter.Profile`) supplies what is its harness's own: the Descriptor, `Provision`, a
`Transport` to the running harness (submit, interrupt, answer, stop, and its events) and a
`Reader` of its record (chunks of record-origin observations, commit, and the evidence for
`Recover`). It registers with `adapter.Register` under its harness's name. A profile that keeps its
credentials behind a broker also renders their placeholders (`adapter.Placeholderer`); the shared
part checks each request against the Descriptor and each result against the kind's route, and
`adapter.TokenPlaceholder` draws a token's placeholder in its shape.

## The Claude Code profile

`pkg/adapter/claudecode` registers `claude-code`. Its harness distribution, under `harness_root`, is
the pinned claude (`bin/claude`) and the profile's hook helper, `cmd/claude-code-hook`
(`bin/claude-code-hook`).

- **Provision** renders what agentd rendered before it (`TestProvisionMatchesAgentdProfile` holds the
  two side by side): `settings.json` with the hooks, `.claude.json` with onboarding, bypass and
  workspace trust answered, the persona, skills, memory, `mcp.json` and the workspace's `CLAUDE.md`,
  and `open_config` with claude's arguments and environment. Each hook runs the helper, which writes
  what it reports to its Session's spool, `scratch/spool/<session id>`: the transport sets the claude
  process's own `HW_EVENT_SPOOL`, so `settings.json` stays the agent's, and the helper's guard
  (`HW_HARNESS_SESSION_ID`) keeps out a hook of any other session. Files a host kept at the scratch
  root — before this profile, or before each Session had a spool — are read there, each by the
  Session it names.
- **Transport:** stream-json, one claude process per Session in a process group of its own. A fresh
  Session starts under its id (`--session-id`); a reopen resumes its transcript (`--resume`), or,
  when claude never wrote one — the launch that opened the Session ended before its first entry —
  starts under its id as a fresh one would. A reopen with `loaded` does not: with no transcript where
  claude looks it fails with `session_not_found` before claude is launched. A claude whose `system`
  frames speak for another session — a copy, as claude may start of a session another process
  holds — is stopped: before it answered `initialize`, `Open` fails with `session_in_use`; at a
  turn's `system/init`, that turn ends `errored`. An input
  is a user message whose uuid is the input's native id, a fresh UUID kept in its submission marker;
  claude's `command_lifecycle` receipt returns `Send`, and its transcript keeps the uuid as the prompt
  entry's. A turn ends with claude's `result`: by `is_error` and `terminal_reason`. `cancelled` needs
  claude's word that the message never started — claude says `cancelled` after the result of a turn it
  interrupted or failed, too. A failed turn is classed by the synthetic message's tag, the HTTP status
  and, for a 429, whether the account refused it: a usage wall (`You've hit your … limit · resets …`)
  closes the gate until its reset; the server's 429 (`not your usage limit`) is an `api` error.
- **Record:** the session transcript, followed from the checkpoint, and the Session's spool. Checkpoint
  format 1 is the transcript follower's checkpoint — the one agentd stored — so stored checkpoints
  resume where they stood (`TestNodeDBCheckpoint`). Entries become `user_input`, `assistant_text`,
  `tool_use`, `tool_result` and `api_error`, keyed by the entry's uuid (and block) or the tool use id,
  with `entry` set to the entry's uuid. A turn's end is in the record as its final assistant entry
  (`stop_reason: end_turn`), a synthetic API-error entry or an interrupt entry, each a record-origin
  `turn_ended`. Spool files become `tool_started`, `tool_finished` and the subagents' start and stop;
  a file is deleted once its chunk is acknowledged, or at once when it reports nothing.
- **Recover** finds the prompt entry by the marker's native id, then that evidence: without either,
  `unknown`.
- **Behind a broker** claude reaches `api.anthropic.com` alone — the profile turns its nonessential
  traffic off — and presents its token there in `Authorization`. A token's placeholder keeps the
  token's prefix (`sk-ant-oat01-`), so claude takes it for the kind of token it is.
- **Sessions side by side:** up to `MaxSessions` (8), the most `TestClaudeConforms` has run with the
  pinned claude: a claude process, a spool and a transcript each, over the agent's one config root
  and workspace ([ADR-022](decisions/adr-022-sessions-side-by-side.md)).
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
  memory only (in `auth.json` for a lent login), no plugins, apps or analytics, and an MCP server per
  connector — `AGENTS.md` with the persona and where the memory directory is, the skills and the
  memory's files; and the workspace's `AGENTS.md`. The credential kinds are `openai_api_key`, which
  the transport hands codex over the protocol (`account/login/start`); `codex_access_token`, a
  ChatGPT Business or Enterprise workspace's Codex access token, which codex reads from
  `CODEX_ACCESS_TOKEN`; and `codex_chatgpt_login`, a ChatGPT login lent for tests and short runs.
  That is a codex's `auth.json` with its refresh token left out, which the transport writes into
  `CODEX_HOME` at every launch with a refresh token that refreshes nothing: codex runs on the
  access token until it expires. A lent login that holds a refresh token is refused, because a
  refresh token is spent when it is used, and a copy that refreshed would log the lender out.
- **Behind a broker** codex presents an API key to `api.openai.com` and a ChatGPT login's access token
  to `chatgpt.com`, where it sends its model traffic over a WebSocket whatever `chatgpt_base_url`
  says, both in `Authorization`; a workspace's access token has no route. An API key's placeholder
  is a key of its shape. A login's is built afresh: JWT-shaped id and access tokens carrying the
  login's plan and account claims and no other, expiring in 2100 so codex never tries to refresh
  them, the account id, and the refresh token that refreshes nothing. The transport copies the
  certificates in `SSL_CERT_FILE` to `CODEX_CA_CERTIFICATE`, where codex reads them.
- **Keeper:** a ChatGPT login kept with the pinned codex, `CODEX_HOME` the keeper's home with a file
  credential store. The app-server runs for a sign-in (`account/login/start` with
  `chatgptDeviceCode`, until `account/login/completed`), a refresh (`account/read` with
  `refreshToken`) and a sign-out (`account/logout`); `Lend` reads `auth.json` and lends it without
  its refresh token, as a `codex_chatgpt_login`. `TestCodexKeeperLive` signs in against ChatGPT for
  a person who approves it.
- **Transport:** `codex app-server`, JSON-RPC 2.0 on stdio, one process per Session in a process group
  of its own. Its starts in one environment take turns until each has answered `initialize`, at a
  lock in the scratch root: app-servers started together on a fresh `CODEX_HOME` fail to initialize
  its state database ([openai/codex#50290](https://github.com/openai/codex/issues/50290)), and a pin
  move migrates the databases as codex starts. codex chooses a thread's id, so a fresh Session opens without one (no
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
- **Sessions side by side:** up to `MaxSessions` (8), the most `TestCodexConforms` has run with the
  pinned codex: an app-server and a thread each, over the agent's one `CODEX_HOME`, whose databases
  they share ([ADR-022](decisions/adr-022-sessions-side-by-side.md)).
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

## The Pi profile

`pkg/adapter/pi` registers `pi` ([ADR-021](decisions/adr-021-pi-profile.md)). Its harness
distribution, under `harness_root`, is the pinned pi's release executable and the `package.json`
beside it (`bin/pi`, `bin/package.json`; alone, the executable reports version `0.0.0`), and the tag
extension (`hw-tag.ts`, which the profile embeds as `Extension`). The rest of a release serves pi's
TUI, its docs and images ([the probe](../../../probes/pirpc/FINDINGS.md)).

- **Provision** renders the agent dir, `PI_CODING_AGENT_DIR`, in the config root: `settings.json`
  — retries on, three of them, no prompt-cache warming (each warming is a billed call), no install
  telemetry — `APPEND_SYSTEM.md` with the persona and where the memory directory is, `mcp.json` with a
  server per connector, its tools declared to the model (`"exposure": "direct"`; a header from a file
  is `!cat <file>`, which pi runs each time it connects), the skills and the memory's files; and the
  workspace's `AGENTS.md`. pi runs offline (`PI_OFFLINE=1`: its model catalog is the one it ships)
  and without telemetry, with `--no-mcp` when there is no connector. The model names its provider —
  `anthropic/claude-…`, `openai/gpt-…` — one of about thirty in the profile's table (`providers.go`),
  each with fixed hosts and its key in a header; a model of another is refused (`invalid_spec`,
  field `model`). The one credential kind, `api_key`, is that provider's API key, which the transport
  writes into `auth.json` under the provider at every launch; a subscription's token
  (`sk-ant-oat…`) is refused.
- **Behind a broker** the kind's route is every provider's hosts and the headers their keys travel in
  (`x-api-key`, `Authorization`, `x-goog-api-key`). A key's placeholder is a key of its shape, and its
  swap is narrowed to the hosts and header of the provider the agent's model names
  ([ADR-020](decisions/adr-020-placeholder-model.md)), so the key goes into no other provider's
  requests. pi reaches nothing else: it takes the broker as `HTTPS_PROXY`, and its certificate
  authority from `NODE_EXTRA_CA_CERTS` or `SSL_CERT_FILE`.
- **Transport:** `pi --mode rpc`, JSON lines on stdio, one process per Session in a process group of
  its own, under the Session's id (`--session-id`, `--session-dir`); pi resumes the session's file
  when it is there. Start waits for `get_state` to name the session and for `get_commands` to list
  the tag extension's command. An input is a `prompt` whose message leads with its tag,
  `<!--hw:NATIVE-->`: the extension's `input` hook takes the tag off and writes it into the session
  as a `custom` entry (`hw.input`) before the user message. pi answers `started` — the receipt — or
  refuses (a run is busy: `busy`, not submitted); a run ends at `agent_settled`, its outcome the
  run's last assistant message: `stop` completes it, `aborted` interrupts it, `error` fails it,
  classed by its text (`classify.go`: Anthropic's API reads `<status> {json}`, OpenAI's
  `OpenAI API error (<status>): {json}`; a usage wall is OpenAI's `usage_limit_reached`, which says
  when it resets), and `length` is `max_output`. `auto_retry_start` is a retry. An interrupt is
  `abort`; pi records an abort mid-tool as an error, `The operation was aborted.`, so the transport
  first notes, durably, that it interrupted the input (`scratch/pi-interrupts/<native>`). An abort
  while pi waits to retry ends the wait (`auto_retry_end`, `Retry cancelled`) and interrupts the run. An
  extension's dialog is answered `cancelled`: the profile raises no prompts. `Close` closes an idle
  pi's stdin, then sends the group SIGTERM, on which pi also ends the tool commands it runs in
  sessions of their own, and SIGKILL once the grace ends.
- **Record:** the session's file, `<session dir>/<timestamp>_<id>.jsonl`, followed from the
  checkpoint once pi writes it with the first user message (`pkg/transcript/pi`). An input's run
  begins at its user message, whose nearest ancestor since the conversation's last message is the
  tag's entry: pi writes the tag, then may compact the context or write a system message, then the
  user message. The pairing is by `parentId`, because pi writes the tag of an input it refuses too,
  with no user message after it. Entries become `user_input`, `assistant_text`, `tool_use` and `tool_started`,
  `tool_result` and `tool_finished`, keyed by the entry's id (and block) or the tool call's id. The
  run's end is a record-origin `turn_ended`: an answer that stops or is aborted, an abort mid-tool the
  profile noted, or a failure pi gives up on — a class it never retries, or its last attempt — or one
  that anything but pi's retry (a `context_edit` dropping the failed answer) follows. A run left at
  a dropped failure ends at the next input's user message: interrupted when the profile noted it, and
  with no end otherwise.
- **Recover** finds the tag's user message, then the run's end: without either — pi never took the
  input in, or crashed before the run ended — `unknown`.
- **One Session of an agent at a time:** no `concurrent_sessions`, until a probe of pi's own shows
  its Sessions side by side keep apart ([ADR-022](decisions/adr-022-sessions-side-by-side.md)).

`TestPiConforms` runs the conformance kit against a real pi driving `internal/mockapi`'s Responses
API, and `TestPiConformsOnAnthropic` its Messages API (all but `usage-limit`: under an API key,
Anthropic's 429 is a rate limit), when `HW_REAL_PI` names the pinned release's executable
(`probes/pirpc/fetch.sh`). The record's tests read sessions the pinned pi wrote
(`pkg/transcript/pi/testdata`). The `harness-adapter` workflow runs the kit on Linux with the pinned
release it downloads and verifies.

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
