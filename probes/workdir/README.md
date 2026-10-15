# A Session's own working directory: the harness probe

Can hw start a Session in a directory of its own beneath the agent's workspace — a git worktree of
each repository, at `<workspace>/wt/<name>/` — for the harnesses it pins, and keep it there through
concurrent edits, a reopen, a load into another environment, and a Session's end with its worktree
removed? This probe answers that with real accounts, through the Harness Adapter Interface's
`session_workdir` (1.9, [ADR-027](../../docs/md/internal/decisions/adr-027-session-workdir.md)). It
is Step 1.0 of agentd's plan
[agents working together](https://coplan.olehluchkiv.com/d/agentd-messaging-primitives-for-agents-p).
What it found is in [ADR-027's Evidence](../../docs/md/internal/decisions/adr-027-session-workdir.md#evidence), and the runs
behind that in [evidence/](evidence/).

The conformance kit's `workdir` and `workdir-load` scenarios ask the same of every adapter against
the mock model API, with no account; this probe adds what only a real model shows: that it works
where it is put, reads the workspace's instructions from there, and carries a conversation across a
reopen and a load.

## What it checks

One environment: a workspace holding a repository, `app/`, with a worktree of it per Session at
`wt/a/app` and `wt/b/app`, on branches `a` and `b`; and workspace instructions naming a secret word,
which hw writes at the workspace's root.

| Step | How |
|---|---|
| a Session in the workspace reads its instructions | it is asked the secret word |
| Sessions in `wt/a` and `wt/b` open, side by side where the harness runs several | `OpenRequest.work_dir` |
| each reads the workspace's instructions from two directories below | it is asked the secret word |
| each works there | it runs `pwd`, writes `app/<name>.txt` and commits it, all by relative paths |
| each worktree holds its own commit on its own branch, the repository's checkout neither | `git log` and `git ls-files` |
| `wt/b`'s Session ends, its worktree is removed, its branch kept; `wt/a`'s goes on | `Close(delete)`, `git worktree remove`, a turn in `wt/a` |
| `wt/a`'s Session reopens there and remembers | a reopen from its checkpoint; it is asked what file it made |
| a load into another environment, at another path | the history saved and restored with `LoadSource.work_dirs`, the workspace copied and `git worktree repair` run, the source removed |
| the loaded Session opens in the new `wt/a`, remembers, and git works there | a strict open (`loaded`); `pwd` and `git log` |

`TestRelativeWorktreesTravel` asks git alone: whether a worktree made with `--relative-paths` works
copied elsewhere beside its repository, and one made without does not.

## Run it

The pinned binaries: `probes/saveload/fetch.sh DIR` (claude, codex — or `npm pack` of
`@anthropic-ai/claude-code-<platform>@<pin>` and `@openai/codex@<pin>-<platform>`), and
`probes/pirpc/fetch.sh DIR` (pi). Codex needs its `codex-code-mode-host` beside it: its default model
runs its tools through it.

```sh
HW_REAL_CLAUDE=DIR/claude HW_WORKDIR_CLAUDE_TOKEN_FILE=~/.config/agentd/claude-token \
HW_REAL_CODEX=DIR/codex HW_WORKDIR_CODEX_LOGIN=~/.codex/auth.json \
HW_REAL_PI=DIR/pi/pi HW_WORKDIR_PI_KEY_FILE=~/.config/agentd/openrouter-key \
HW_WORKDIR_EVIDENCE=$PWD/probes/workdir/evidence \
  go test -count=1 -v -timeout 60m ./probes/workdir/
```

Each run spends about ten short turns of its account.

| Variable | What it does |
|---|---|
| `HW_WORKDIR_CLAUDE_TOKEN_FILE` | A `claude setup-token` token. |
| `HW_WORKDIR_CLAUDE_MODEL` | Claude's model; haiku when unset. |
| `HW_WORKDIR_CODEX_LOGIN` | A ChatGPT login's `auth.json`. The probe lends its access token alone, as a runtime does: the refresh token never leaves the file. |
| `HW_WORKDIR_CODEX_MODEL` | Codex's model; its default when unset. |
| `HW_WORKDIR_PI_KEY_FILE` | The API key of pi's model's provider. |
| `HW_WORKDIR_PI_MODEL` | Pi's model, `<provider>/<id>`; `openrouter/anthropic/claude-haiku-4.5` when unset. |
| `HW_WORKDIR_EVIDENCE=DIR` | Writes each run's report to `DIR/<harness>.json` and `.md`. |
| `HW_WORKDIR_WORK=DIR` | Builds the environments under `DIR` and leaves them there. Not for a shared machine: they hold the credential. |
