# ADR-027: a Session may work in a directory of its own beneath the workspace

**Status:** Accepted (2026-10-15). It adds to [ADR-022](adr-022-sessions-side-by-side.md): Sessions
of one agent side by side may each work in a directory of their own — a git worktree of each
repository, say — rather than all in the workspace.

**Intent:** principle 3, *normalize, don't leak*, principle 6, *evolve public contracts
deliberately*, and principle 2, *a wrong verdict is worse than no verdict*
([INTENT](../../../../INTENT.md#design-principles)). Under principle 3, where each harness keeps what
it keeps by its working directory — claude's transcript directory, pi's session header — stays the
profile's: a Host names a directory, and knows no harness. Under principle 6, it is a minor version
behind a capability: a 1.8 caller sends no `work_dir` and meets nothing new. Under principle 2, a
Session that names a directory that is not one fails before its harness starts, and a loaded one
that its load did not move fails `session_not_found`, never opening as an empty conversation.

## Context

agentd's plan [agents working together](https://coplan.olehluchkiv.com/d/agentd-messaging-primitives-for-agents-p)
gives a lead task Sessions that each work in a worktree of their own, `<workspace>/wt/<name>/`, so
that Sessions editing one repository at once do not share an index or a checkout. Every profile
started its harness in the workspace: `Provision` renders the working directory into the
`open_config`, which every Session of the agent shares, and the Host passes it on unchanged. Claude
Code names a Session's transcript for its working directory, pi finds a session by the directory its
header names, and codex starts a thread in a `cwd` it is given; a Host cannot change any of that
without knowing the harness.

The probe ([probes/workdir](../../../../probes/workdir/README.md), [Evidence](#evidence)) ran the pinned claude 2.1.296,
codex 0.160.0 and pi 1.0.4 with real accounts: two Sessions in worktrees of one repository, side by
side, editing and committing at once, one reopened, one ended and its worktree removed, and a load
into another environment at another path. Each passed once the profile ran its harness in the
Session's directory. Codex alone did not read the workspace's `AGENTS.md` from there: it reads a
project's instructions from its root, a directory holding `.git`, down to where it works, and the
workspace holds none.

## Decision

1. **Interface 1.9 adds capability `session_workdir`.** `OpenRequest.work_dir` names the directory a
   Session works in, beneath `Layout.workspace`: a clean relative slash path (`CheckWorkDir`), `""`
   for the workspace. `RecordRequest.work_dir` names it for a record read, and
   `LoadSource.work_dirs` the directories a load's Sessions worked in. An adapter without the
   capability refuses a `work_dir` (`unsupported`); a 1.8 request carrying `work_dirs` is refused
   (`protocol`, `contract.CheckMinor`).
2. **The kit checks the directory when the Session opens**: it must be a directory beneath the
   workspace, reached through no symbolic link, or the open fails `open_failed{config_invalid}`
   before any harness starts. A record read does not check it: a record outlives its directory.
   Profiles take the absolute path in `adapter.Start.WorkDir` and `adapter.RecordSource.WorkDir`.
3. **Each profile runs its harness there.** Claude Code: the process's working directory, the hooks'
   `HW_HOOK_CWD`, and the transcript found by that directory's name, live and in the record
   (`openConfig.In`). Codex: the app-server's directory and the thread's `cwd`, started or resumed.
   Pi: its working directory, which its session's header names.
4. **A load moves what a harness keeps by each directory**: claude's project directory of each
   `work_dirs` entry to the one the same directory beneath the new workspace names
   (`history_relocations`), and pi's header naming it (`history_rewrites`, which since 1.9 may share
   a path when they rewrite one field from values of their own). Codex keeps its rollouts by thread,
   and resumes a thread in the `cwd` it is given.
5. **What the agent's Sessions share stays the agent's.** `Provision` writes nothing into a
   Session's directory. Claude Code and pi read the workspace's instructions file from a directory
   beneath it on their own; codex is told the workspace is a project's root
   (`project_root_markers = [".git", "AGENTS.md"]` in its configuration, when the spec has workspace
   instructions), so it reads the workspace's `AGENTS.md` from there too.
6. **The conformance kit asks it of every adapter**: `workdir` (Sessions in their own directories,
   side by side where the adapter runs several, each showing where it works by reading a mark
   written there; each reopened and its record read there; a bad path and a missing directory
   refused) and `workdir-load` (a Session's directory carried by a load). The fake adapter keeps its
   record by its working directory, as claude does, and breaks both on demand
   (`workdir-ignored`, `workdir-load-stays`).

## Evidence

[probes/workdir](../../../../probes/workdir/README.md), 2026-10-15, macOS arm64, git 2.50.1, real
accounts: claude 2.1.296 (haiku), codex 0.160.0 (its default model, a lent ChatGPT login), pi 1.0.4
(`openrouter/anthropic/claude-haiku-4.5`); each run under 40 seconds. The reports are in
[evidence/](../../../../probes/workdir/evidence/).

| | Claude Code | Codex | Pi |
|---|---|---|---|
| Sessions in `wt/a` and `wt/b`, side by side | yes | yes | one at a time, as pi runs |
| each works there: `pwd`, a file and a commit by relative paths | yes | yes | yes |
| each commit on its own branch, the repository's checkout untouched | yes | yes | yes |
| reads the workspace's instructions from `wt/<name>/` | yes: `CLAUDE.md` in the directories above | yes with `project_root_markers`; `UNKNOWN` without | yes: `AGENTS.md` above |
| a Session ends, its worktree is removed, the other goes on | yes | yes | yes |
| a reopen there, remembering | yes | yes | yes |
| a load to another path: record found, strict open, remembers, works in the new `wt/a`, git works | yes | yes | yes |
| steps failed | 0 of 19 | 0 of 19 | 0 of 18 |

- **Instructions.** Codex reads a project's instructions from its root, the nearest directory above
  holding a marker (`.git` by default), down to where it works. Neither `wt/a/` nor the workspace
  holds `.git` (each worktree is a directory below), so codex read only `wt/a/` and answered
  `UNKNOWN` to the secret word the workspace's `AGENTS.md` named, until decision 5's marker.
- **Worktrees moved.** A worktree's links are absolute by default: copied to another path with the
  source gone, git refused it (`fatal: not a git repository: …/env1/workspace/app/.git/worktrees/app`);
  `git worktree repair <new path>`, run in the copied repository, mended both links. A worktree made
  with `git worktree add --relative-paths` (git 2.48 and later) worked copied with no repair
  (`TestRelativeWorktreesTravel`).
- **Against the mock model API**, the kit's `workdir` and `workdir-load` pass on the fake adapter and
  on the pinned claude, codex (under `gpt-5`, whose tools are shell calls: codex's default model runs
  its tools through a code-mode host the test distribution lacks) and pi; the fake's two breaks fail
  them; every other scenario still passes on all three.

## Alternatives

- **The Host rewrites the `open_config`'s working directory.** It would have to know each profile's
  configuration, and the record lookups and load moves that follow from the directory would still be
  the profile's.
- **A `Provision` per Session.** It renders the agent's whole configuration again for each Session,
  and a load would have to name every one of them.
- **The harness works in the workspace and is told where its worktree is.** It is what agentd's crew
  templates did by convention: every relative path, build and `git` call depends on the model
  remembering to change directory first, and codex's commands each name their directory.

## Boundary

A Session's directory says where its harness works; it is no isolation. Sibling Sessions still share
one workload user, home, configuration and credential, and a harness may read and write anywhere the
workload may (ADR-022). The kit checks the directory once, when the Session opens.

## Consequences

- A runtime that moves a workspace — an export and a load — carries each Session's directory with it
  and names those directories in the load; a git worktree's links name absolute paths unless it was
  made with `--relative-paths` (git 2.48 and later), and a copy elsewhere needs `git worktree repair`
  before git works in it ([Evidence](#evidence)).
- codex treats the workspace as a project's root for every Session of an agent with workspace
  instructions, its `AGENTS.md` the marker: a workspace that is no repository, as before, reads the
  same file.
