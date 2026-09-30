# Codex's own turns

**Probed:** codex 0.144.5, the version hw pins, over `codex app-server` with no adapter between,
against `internal/mockapi`, on 2026-09-30, macOS arm64. Rerun: [README.md](README.md).

**Question:** a thread with an active goal makes codex start turns nobody sent an input for. What
are those turns to a client, what happens to an input sent while one runs, and what does a thread
keep of its goal when codex stops?

## A goal starts turns

With a goal whose status is `active`, codex starts a turn by itself:

- when the goal is set active (`thread/goal/set` from a client, or the model's `create_goal` tool:
  then once the turn that made it completes);
- after every turn that **completes**, the input's turns included;
- when the thread resumes (`thread/resume`).

The turn arrives as `turn/started`, a few milliseconds after the `turn/completed` before it, with
a turn id no `turn/start` answered with. It has no `userMessage` item. In the rollout it is a
`task_started`, a `response_item` of role `user` whose text begins
`<codex_internal_context source="goal">` and carries the objective, the model's messages and tool
calls, and a `task_complete` — and no `user_message` event.

The model has `get_goal`, `create_goal` and `update_goal` tools, so an input can make a goal, and
a goal turn can complete it. `thread/goal/updated` reports every change as it happens, with the
turn that made it (`turnId`, or `null` for a change from outside a turn); `thread/goal/cleared`
reports a clear; `thread/name/updated`, a new name.

## The chain stops

| What happens | The goal after | Does codex start another goal turn? |
|---|---|---|
| The model calls `update_goal {status: "complete"}` | `complete` | No |
| A goal turn fails (its model call errors past the retries) | `blocked` | No |
| A goal turn meets the account's usage wall | `usageLimited` | No |
| `turn/interrupt` of a goal turn: `turn/completed {interrupted}`, `turn_aborted` in the rollout | `active` | **Not until an input's turn completes**, or the thread resumes |
| A client sets `paused` | `paused` | No |

A goal that is `paused`, `blocked`, `usageLimited`, `budgetLimited` or `complete` keeps that status
across a stop and a resume, and starts nothing.

Not probed: whether a goal turn follows an **input's** turn that failed while the goal stayed
active. The adapter assumes one may, and waits a few seconds for it before the next input.

## An input sent while a turn runs

`turn/start` during a running turn does not start a turn. codex answers at once with a turn id that
**never starts**, and folds the input into the running turn at that turn's next model call: a
`userMessage` item with the input's `clientUserMessageId`, whose `turnId` is the running turn, and
a `user_message` event in that turn's rollout. The model then answers the input inside the goal
turn, and `turn/completed` is the goal turn's.

If the running turn is **interrupted** before it takes the input in, the input is dropped: no turn
starts for it, no model request carries it, and the rollout never holds it.

So a client that wants an input answered by a turn of its own must not send it while a goal turn
runs, and must not send it in the instant before one starts.

## Stopping

- Closing codex's stdin in the middle of a goal turn makes codex abort the turn — `turn_aborted`
  in the rollout — and exit 0, within tens of milliseconds.
- SIGTERM in the middle of a turn leaves the turn with no end in the rollout.

## What a thread keeps of its goal, and where

- `thread/read` and `thread/goal/get` answer for a thread **before** `thread/resume`, from what is
  on disk, and start nothing.
- The rollout logs `thread_goal_updated` for a goal a client sets, and for nothing else: not the
  model's `create_goal` or `update_goal`, not a clear. **The rollout cannot say what a thread's goal
  is.** Only codex can, through `thread/goal/get` and its notifications.
- The goal is in `goals_1.sqlite` and that database's write-ahead log. Its row can still be in the
  log alone when codex has exited — after a kill it is, after a clean exit it may be — and the
  database without the log then restores no goal (`probes/saveload`,
  `TestCodexLoadKeepsNameAndGoal`).
- `turn/interrupt` with no turn running is refused: `-32600`, "no active turn to interrupt".

## What the adapter does with this

`pkg/adapter/codex` follows the turn codex is on, from `turn/started` to `turn/completed`, and
binds an input to its turn by the id `turn/start` answered with, or by its `userMessage` item.

- A turn no input is bound to is reported as the harness's own: `turn_started` and `turn_ended`
  naming the turn and no input (capability `autonomous_turns`).
- **An input preempts.** `Send` interrupts the goal turn that runs, waits for its end, and only
  then sends `turn/start`; when a goal turn is due — the thread just resumed, or a turn just
  completed, with the goal active — it waits for that turn to start and interrupts it. After the
  interrupt codex starts nothing of its own until the input's turn ends, so the input's turn is
  the input's alone.
- Should codex fold an input into a goal turn all the same, the goal turn ends where it took the
  input in and the rest is the input's turn. An input dropped with an interrupted goal turn is
  sent again, or ends `cancelled` when the interrupt was of the input.
- Parking closes stdin when codex is on no input's turn, so a goal turn ends cleanly in the
  rollout.
- The rollout cannot vouch for a goal, so the profile keeps what codex last said of the thread's
  name and goal in `scratch/native/<thread>.json`, saves it with the thread, and checks a loaded
  thread against it before `thread/resume` (`open_failed {state_mismatch}`).
