# Codex's own turns: the probe

What does codex's app-server do with a turn no client started, and with an input sent while one
runs? This probe answers that for the codex hw pins, by speaking JSON-RPC to `codex app-server`
itself, with no adapter between, against the mock model API. What it found is in
[FINDINGS.md](FINDINGS.md); the Harness Adapter's Codex profile (`pkg/adapter/codex`) is built on
those findings, and each is a test here, so a codex that behaves otherwise fails this probe first.

It is part of Step 2 of
[Save and load an agent's conversation: Claude Code and Codex](https://coplan.olehluchkiv.com/d/save-and-load-an-agents-conversation-cla):
a thread with an active goal is saved and loaded as it is, so the adapter has to follow the turns
codex starts for it.

## Run it

The probe is a Go test package that skips without the binary it names. `probes/saveload/fetch.sh`
downloads the pinned one for this platform and checks it against its publisher's checksum:

```sh
probes/saveload/fetch.sh /tmp/pinned
HW_REAL_CODEX=/tmp/pinned/codex go test -count=1 -v ./probes/codexturns/
```

`-v` prints every request, answer and notification with its time, which is the evidence behind
each finding. No account and no network: the model is `internal/mockapi`, whose `MKGOAL <text>`
scenario has the model make a goal and whose `GOAL <n> <k>` is a goal's objective — `n` turns of
`k` chunks each, then an `update_goal` call that completes it. `FAILSLOW <s>` is a model call that
fails after `<s>` seconds, every time codex tries it.

| Test | What it establishes |
|---|---|
| `TestGoalStartsTurns` | An active goal starts a turn when it is set and after each turn that completes; what such a turn looks like to a client and in the rollout. |
| `TestModelMakesGoal` | The model makes and completes a goal with its own tools; the rollout logs neither, nor a clear. |
| `TestInterruptedGoalRests` | An interrupted goal turn leaves the goal active and codex idle until an input's turn completes. |
| `TestInputDuringGoalTurn` | `turn/start` during a running turn starts no turn: codex answers with the running turn, and folds the input into it, or drops it if that turn is interrupted first. |
| `TestInputAtATurnsEnd` | An input sent as an input's turn ends, swept across the end a millisecond at a time, is taken in by that turn or has a turn of its own: never held past the end. |
| `TestInputAtAGoalTurnsEnd` | The same across a goal turn's end: taken in by it, or by the turn after. |
| `TestInputIntoAFailingTurn` | A turn that fails past its retries takes the input it holds in as it fails; the model never has it. |
| `TestClientIDIsNoKey` | codex deduplicates no `clientUserMessageId`: an input sent twice is taken in twice, and one that ran runs again. |
| `TestStopAndResumeOnAGoal` | stdin EOF and SIGTERM abort a goal turn cleanly; the name and goal are readable before `thread/resume`, which starts a goal turn. |
| `TestFailedGoalTurnStopsTheGoal` | A failed goal turn blocks the goal; a usage wall marks it `usageLimited`. |
| `TestRestingGoalSurvivesARestart` | A goal that is not active keeps its status across a restart and starts nothing. |
