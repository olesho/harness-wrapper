# Save and Load: can a Session continue in a fresh environment?

**Probed:** claude 2.1.283 and codex 0.144.5, the versions hw pins, through
hw's Harness Adapter at `v0.24.0-5-g92409c9`, on 2026-09-30. macOS arm64, and
Linux arm64 on two Lima VMs (Ubuntu, kernel 7.0; Debian, kernel 6.12) with the
claude-code distribution agentd installed there. Mock runs use
`internal/mockapi`. The live runs are on macOS: Claude Code on haiku under a
`claude setup-token` token, and Codex on its default model under a ChatGPT
login (see *The Codex live run's credential*).

Rerun: [README.md](README.md). Each run's evidence is
`evidence/<platform>/<harness>-<mode>.json`, with its tables beside it as `.md`.

**Question:** a Session of each harness is saved as files once its harness has
stopped. Does it continue in an environment where every root has another path
and the source is gone?

**Yes, for both.** Against the mock on macOS, on Linux, and saved on one VM
and loaded on the other; and against each harness's live API. **Every required
check of the gate passes.** Nothing in a record had to be rewritten: Claude
Code needs one directory renamed, Codex nothing.

## The gate

| What the plan's gate asks | Claude Code, mock | Claude Code, live | Codex, mock | Codex, live |
|---|---|---|---|---|
| The source is gone before the restore, and nothing reads or writes it after (`source.inaccessible`, `source.untouched`; across two VMs, `source.elsewhere`) | pass | pass | pass | pass |
| The first resumed model request carries the earlier turns and the tool result (`load.history`) | pass | | pass | |
| The live model recalls the nonce, with no tool and no fixture (`load.history`) | | pass | | pass |
| The same native Session id (`load.same-session`) | pass | pass | pass | pass |
| The record reader reaches the restored record's end; its checkpoint is at the last byte (`load.record-read`) | pass | pass | pass | pass |
| Only new record events after the reopen (`load.new-events-only`) | pass | pass | pass | pass |
| A stop and a second reopen keep what the restored Session added (`load.second-reopen`) | pass | pass | pass | pass |
| The new working directory (`load.new-cwd`) | pass | pass | pass | pass |
| The record is appended to, never rewritten (`load.no-rewrite`) | pass | pass | pass | pass |
| Every request carries the new environment's credential (`load.fresh-credential`) | pass | | pass | |
| The thread's name and goal are kept (`load.aux`, `load.aux-kept`) | | | pass | pass |
| Negative control: nothing restored (`negative.harness-refuses`, `negative.no-context`) | pass | pass | pass | pass |

The mock columns hold for all three mock runs of each harness: macOS, Linux,
and Linux across two machines. Every required check passed in each run: 15 for
Claude Code and 18 for Codex against the mock on one machine, 11 and 13 when
the archive came from another, 14 and 17 live.

In each live run, turn one replied `PONG<nonce>` and turn two ran
`echo <nonce T> | tee saveload-note.txt`. After the restore the model was asked
"What exact word did you reply with in your very first answer in this
conversation?" and answered with the nonce, using no tool. After a stop and a
second reopen it was asked what the shell command had printed, and answered
with nonce T. With nothing restored, the same first question got "This is the
first message in the conversation, so I have not given a prior answer" from
Claude Code, and "None" from Codex.

### The Codex live run's credential

The adapter's Codex profile takes an OpenAI API key or a ChatGPT workspace's
access token. The live run had neither: it ran on the ChatGPT login of the
codex on the machine, which is not one of those kinds. The probe lent it
itself, by setting codex's credential store to a file and giving each
environment an `auth.json` with the login's access token, staged anew per
environment and saved by no archive. The refresh token was never copied.

So the Codex live run differs from an agentd agent's in how codex got its
credential, and in nothing else: the same adapter, app-server, `thread/resume`
and record reader. A run under one of the adapter's two kinds has not been
made.

## What to save

Every file the source held is in each run's evidence, under `inventory`. What
follows is what the variants showed each file to be worth.

### Claude Code

| File | What it is | Save it |
|---|---|---|
| `config/projects/<workspace>/<session>.jsonl` | The Session's transcript | Yes. Alone it continues the conversation. |
| `config/projects/<workspace>/<session>/subagents/agent-<id>.jsonl`, `.meta.json` | A subagent's transcript | Yes. It sits beneath the project directory and moves with it. |
| `config/memory/**` | The agent's memory | Yes. |
| `config/settings.json`, `.claude.json`, `persona.md`, `workspace/CLAUDE.md` | Rendered by Provision | No. The new environment renders its own. |
| `config/backups/**`; live only: `.last-cleanup`, `policy-limits.json*`, `remote-settings.json` | claude's own caches | No. |
| `scratch/markers/**` | hw's submission markers | No. See *Record and checkpoint*. |
| `/tmp/claude-<uid>/<workspace>/<session>/tasks/**` | A subagent task's output, outside the five roots | No, and not needed. claude writes it under `/tmp` whatever `TMPDIR` says. |

`<workspace>` is the workspace's resolved path with every character that is not
a letter or a digit written as `-`.

The adapter's `history_roots` (`config/projects`, `config/memory`), less its
`secret_paths`, are the right inventory as they stand.

### Codex

| Saved | Files | Conversation continues | Name | Goal |
|---|---|---|---|---|
| The candidate: `history_roots`, `session_index.jsonl`, `goals_1.sqlite` with its sidecars, and the workspace's note | 7 | yes | kept | kept |
| `history_roots` alone (`config/sessions`, `config/memory`): what a delete archives today | 2 | yes | **lost** | **lost** |
| The rollout alone | 1 | yes | lost | lost |
| The rollout and `session_index.jsonl` | 2 | yes | kept | lost |
| The rollout, `goals_1.sqlite`, its `-wal` and its `-shm` | 4 | yes | lost | kept |
| The rollout, `goals_1.sqlite` and its `-wal` | 3 | yes | lost | kept |
| The rollout and `goals_1.sqlite` | 2 | yes | lost | **lost** |

So:

- **The rollout alone continues the conversation.** It is
  `config/sessions/YYYY/MM/DD/rollout-<time>-<thread>.jsonl`.
- **`session_index.jsonl` carries the thread's name**, one JSON line per thread.
- **`goals_1.sqlite` and `goals_1.sqlite-wal` together carry the goal.** codex
  exits leaving the goal's row in the write-ahead log, so the database file
  alone restores no goal, and says nothing about it. The `-shm` is not needed.
- The rollout does log a goal's update, as a `thread_goal_updated` event, but
  codex does not rebuild the goal from it.
- `state_5.sqlite*`, `logs_2.sqlite*`, `memories_1.sqlite*`, `installation_id`,
  `.personality_migration`, `skills/**` and, live, `models_cache.json` are not
  needed: a load without them passed every check.
- codex was seen to write nothing outside `CODEX_HOME` and the workspace.

## Relocation

| | Rule | Contents |
|---|---|---|
| Claude Code | One prefix rule: `config/projects/<source workspace>` moves to `config/projects/<new workspace>`. Both names come from resolved paths. | Untouched |
| Codex | None. The rollout keeps its path beneath the config root; `thread/resume {threadId, cwd}` names the new working directory. | Untouched |

`load.no-rewrite` is the proof: after three more turns, the first bytes of the
continued record, as many as were saved, still hash to the saved SHA-256.

The source's workspace path has to travel in the archive **as the harness
resolved it**. claude names the project directory for the resolved path, and
the destination cannot resolve a path of a machine that is gone. On macOS the
two differ (`/var/…` is `/private/var/…`), which is how the probe found it.

Each harness tells the model about the move itself. Claude Code's first
resumed request ends with a system reminder, "Environment update — Primary
working directory …". Codex sends the new workspace's `AGENTS.md` again
("These AGENTS.md instructions replace all previously provided AGENTS.md
instructions") and a new `<environment_context>` with the new `cwd`. The old
paths stay in the history they were said in.

## What the resumed Session sent

The first model request after the restore, from the macOS mock runs (the start
of each item; the harness's own reminders left out):

```
Claude Code                                       Codex
user       PING <A>                               user       PING <A>
assistant  PONG <A>                               assistant  PONG <A>
user       TOOL echo <T> | tee saveload-note.txt  user       TOOL echo <T> | tee saveload-note.txt
assistant  tool_use Bash {"command":"echo <T>…    assistant  tool_use exec_command {"cmd":"echo <T>…
user       tool_result <T>                        tool       tool_result …Output:\n<T>
assistant  TOOL DONE: <T>                         assistant  TOOL DONE: <T>
user       AGENT PING sub
assistant  tool_use Agent {"prompt":"PING sub"…
user       tool_result [Subagent hand-back] …
assistant  TOOL DONE: [Subagent hand-back] …
user       PING again                             user       PING again
```

The request after the second reopen carries all of that, then `PING again`,
`PONG again`, and the new prompt.

## Record and checkpoint

- **Reading the restored record.** A record handle opened on the restored
  environment with no checkpoint delivers the saved turns — the prompt, the
  reply, the tool call, its result — then the end of the record, with a
  checkpoint whose offset is the file's size. No reset, no rescan, no fault.
  For Claude Code this needs the relocation: the reader looks under the new
  workspace's name.
- **Those observations belong to no input.** hw's submission markers live in
  the scratch root, which is not saved, so nothing maps the saved prompts back
  to input ids, and the record proves no `turn_ended` for them. That is
  harmless as long as nothing of this read is published, which is the design.
- **A checkpoint cannot be carried.** Given the source's last checkpoint, the
  reader reports a reset (`replaced`: the checkpoint names the inode it was
  made on) and delivers the whole record again. A Load seeds its own.
- **After the reopen from the seeded checkpoint, only new events come.** The
  first restored Session delivered three record observations — the new prompt,
  its reply, its end — none with an id the restored record held. The Session
  after the second reopen, from the checkpoint the first one left, delivered
  only its own two turns.

## The negative control

With nothing restored:

- **The harness alone refuses.** `claude --resume <id>` exits with
  "No conversation found with session ID", and codex's `thread/resume` answers
  "no rollout found for thread id".
- **The adapter does not.** Its reopen starts a fresh conversation: Claude Code
  under the saved id, Codex under a new thread id. That is deliberate — a
  launch that ended before its first entry has nothing to resume
  (`TestReopenWithoutTranscript`) — and it is exactly the fallback a Load must
  never take.
- **The record handle reads an empty record**: no observation, no checkpoint.

## Beside the gate: a Codex goal that is active

The probe saves a **paused** goal. `TestCodexActiveGoal` shows why, against the
mock, which answers at once and never completes a goal
(`evidence/*/codex-active-goal.json`; macOS figures, each window two seconds):

| | Turns codex started by itself | Model requests |
|---|---|---|
| Goal paused, thread resumed and left alone | 0 | 0 |
| Goal set active | 116 | 115 |
| Thread resumed in a new process, goal active, nothing sent | 68 | 67 |
| Reopened through the adapter, before any input | not seen by the adapter: the Session is `idle` | 51 |

Then one input was sent through the adapter. The record reported it whole —
`user_input`, `assistant_text` and a record-origin `turn_ended` — but no live
`turn_ended` came in ten seconds, and the Session stayed `busy` while codex
made 163 more requests that carried the prompt.

Two things follow, neither of them made by Save or Load:

- codex works on an active goal as soon as its thread is resumed, with no
  input. `goals` is a stable feature, on by default, and hw's rendered
  `config.toml` does not turn it off.
- hw's Codex transport binds a turn to the input it sent. It has no place for
  a turn codex started, and an input that lands among such turns never ends
  for it.

A Load restores a goal's row as it was saved. A Session saved with an active
goal would therefore start working the moment it is reopened, as it does today
after any park.

## What this changes in Step 2

1. **The archive's metadata carries the workspace as the harness resolved
   it**, beside the layout's roots. Provision is pure and cannot resolve a
   path, so the layout it is given for the new environment must be resolved
   too.
2. **The checkpoint is never archived.** The Load's seed is the only source of
   one, as the plan has it.
3. **The seed is also the guard against a fresh conversation.** A Load fails
   when the record handle reads an empty record, before any open. After Open
   it compares the Session id with the saved one, which catches Codex. For
   Claude Code the id is the same either way, so only the seed tells. A strict
   reopen in hw 1.1 — fail with `session_not_found` instead of starting
   fresh — would make the adapter refuse as the harness does.
4. **Codex's history grows by three files**: `session_index.jsonl`,
   `goals_1.sqlite` and `goals_1.sqlite-wal`. Omitting the `-wal` loses the
   goal without an error, so a Load has to read the name and the goal back
   (`thread/read`, `thread/goal/get`) to keep the promise of no silent loss.
   The interface has no operation for that today.
5. **An active goal needs a decision before Codex goals are kept at all**:
   teach hw's transport the turns codex starts, or render `goals = false` and
   keep no goal, or have a Save refuse or pause an active one.
6. **What is not saved, and need not be**: hw's submission markers, claude's
   `/tmp` task output, codex's other stores.

## Not covered

- Codex live under one of the adapter's own credential kinds.
- A save taken mid-turn, or with record events not yet acknowledged. The probe
  saves after a settled turn and a drained close.
- A conversation long enough for the harness to compact it.
- Codex's own subagent threads, and claude's background tasks.
- Workspace fidelity. One file is restored and read back, as a smoke check.
- Any version but the pinned pair, x86_64, and a load on another operating
  system than the save's.
