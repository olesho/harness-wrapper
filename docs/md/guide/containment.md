# Landlock containment

harness-wrapper can start a harness inside a **Landlock domain** on Linux: an extra,
kernel-enforced boundary around the harness child and everything it starts — tools, MCP servers,
subshells. It restricts which files the harness can open, which pathname and abstract UNIX sockets it
can reach, which processes outside the domain it can signal, and (optionally) which TCP ports it can
connect to. It needs no container, VM or root.

Containment is **opt-in and additive**. Nothing turns it on for you: not a permission rung,
`--sandbox-defaults`, an environment variable or a harness profile. With it unset, every surface
behaves exactly as it did before the feature existed. And it is an *outer* layer: the harness's own
[permission rungs and sandbox](permissions.md) keep their meanings inside it. The one exception is
codex's own Linux sandbox, which cannot run inside a Landlock domain, so a contained codex runs only at
the rung that bypasses it (see [Harness profiles](#harness-profiles)).

## Quickstart

On a Linux host with Landlock ABI 9 (Linux 7.1 or later), with `claude` or `codex` on `PATH`:

1. **Check the host.** `contain-check` prints the policy a launch would get and every reason it would
   be refused, and starts nothing:

   ```bash
   harness-wrapper contain-check claude --
   ```

2. **Sign in once**, into a directory you keep for it:

   ```bash
   harness-wrapper contain-login --contain-state-dir ~/.local/state/hw-claude claude
   ```

   It prints a sign-in page. Open it in any browser, on any device, and paste the code the page shows
   back at the prompt. For codex, it prints a page and a one-time code to enter there instead. See
   [Signing in](#signing-in).

3. **Start the harness contained**, as that login, in the current directory:

   ```bash
   harness-wrapper --contain landlock --contain-state-dir ~/.local/state/hw-claude \
     --contain-restrict-tcp --contain-allow-tcp 443 claude --
   ```

   The harness can write the working directory. Your home directory, other checkouts, SSH agent and
   D-Bus are out of its reach, and TCP reaches only port 443. A contained codex also needs
   `--permission-mode bypass` (see [Harness profiles](#harness-profiles)).

> **Both profiles are inactive for now**, so step 3 is refused at the `profile` stage until their
> authenticated conformance runs pass. Steps 1 and 2 work today: a login does not need an activated
> profile.

## Requesting it

| Surface | How |
|---|---|
| CLI | `--contain landlock`, plus `--contain-rw PATH`, `--contain-ro PATH`, `--contain-restrict-tcp`, `--contain-allow-tcp PORT`, `--contain-min-abi N`, `--contain-state-dir DIR`, `--contain-pass-env NAME` — on passthrough, `run`, `structured-run` and tmux runs. `contain-check` previews the policy; `contain-login` signs a harness in. See [CLI](cli.md#containment-flags). |
| Go | `wrapper.Config.Containment`, `chat.Options.Containment` (and `ReopenOptions`), `harness.TurnConfig.Containment`, `oneshot.Config.Containment`, `pkg/env.StructuredTurnConfig.Containment`; `wrapper.StartLogin` and `wrapper.LoginStatus` for [signing in](#signing-in). |
| HTTP | a `containment` object on `POST /v1/conversations` and `POST /v1/turns`, after checking `GET /v1/capabilities`. See [Gateway](gateway.md#containment). |
| Clients | `containment` on the TypeScript and Python `open()`; both check the capability route first. |

The request (`containment.Request`, aliased as `wrapper.Containment`):

| Field | Meaning |
|---|---|
| `kind` | `"landlock"`, the only kind. |
| `read_only` / `read_write` | Extra **existing absolute** paths the harness may read, or read and write. |
| `restrict_tcp` | Off by default (TCP unrestricted, and reported so). On: every TCP bind is denied, and every connect except to `connect_tcp`. |
| `connect_tcp` | Permitted remote ports. Empty with `restrict_tcp` denies all TCP. Ports without `restrict_tcp` are invalid. |
| `min_abi` | Minimum Landlock ABI; default and floor **9** (Linux 7.1). |
| `state_dir` | Caller-managed, persistent, shareable state instead of private state (see [State](#private-state)). |
| `pass_env` | Extra environment variable **names** to inherit. HOME, temporary and harness-state variables cannot be named. |

## What the domain enforces

Every contained launch handles every ABI 9 filesystem right, and the harness gets only:

- the harness profile's baseline — `/usr` (and `/bin`, `/lib`, `/lib64`, `/sbin` where they are real
  directories) read/execute; `/proc` read-only; an enumerated list of `/etc` files; `/dev/null`,
  `/dev/urandom`, `/dev/tty` and the session's own PTY;
- the harness executable (and, for codex, its package root and `node`);
- the **working directory**, read-write;
- its **private HOME, TMPDIR and harness-state directory**, read-write;
- your `read_only` / `read_write` paths.

Plus, always: connecting to a **pathname UNIX socket** created outside the domain is denied
(`RESOLVE_UNIX`, EACCES) — that includes the user's D-Bus and systemd sockets and SSH agents; an
**abstract UNIX socket** outside the domain is denied, and so is **signalling** a process outside it
(EPERM). Sockets the harness's own tools create stay reachable. Pre-opened descriptors are the other
half of the boundary: the child inherits **exactly its terminal** on descriptors 0–2, whatever the
embedding process holds without close-on-exec ([ADR-004](../internal/decisions/adr-004-thread-scoped-landlock.md)).

It does **not** give complete isolation. Landlock does not mediate `stat`, `chmod`, `chown`, `utime`,
`flock`, `fcntl` or `access`; other same-user processes' command lines and status stay readable through
`/proc`; a path you grant is shared with anyone else granted it; UDP and other non-TCP protocols are not
filtered (glibc falls back to DNS over UDP port 53); `connect_tcp: [443]` allows every HTTPS endpoint,
not just the model provider. Credentials you deliberately provision stay readable to the harness and its
tools.

## Refused, never downgraded

A request that cannot be honoured as asked fails **before the harness starts** — never an
uncontained run. `wrapper.Start` returns `ErrContainmentUnsupported` (not Linux) or
`ErrContainmentRefused` (both wrap `ErrInvalidConfig`, so the gateway answers 400 `invalid_config`),
naming the stage:

| Stage | Examples |
|---|---|
| `request` | unknown kind, relative path, ports without `restrict_tcp`, `min_abi` below 9, a reserved `pass_env` name; a login launch of any command but the profile's login and status commands |
| `profile` | no profile for the harness; a profile not yet activated; codex below the bypass rung; a claude login under `restrict_tcp` |
| `executable` | an install layout or version the profile does not know |
| `paths` | a missing grant; a read-only grant inside a writable one (it would not be read-only); a grant that exposes the managed-state directory; a writable grant that exposes cgroupfs; a resumed path that now resolves elsewhere |
| `state` | private or caller state unusable; a claude TMPDIR too long for its socket path; a login without a `state_dir` |
| `kernel` | Landlock missing, disabled at boot, blocked by seccomp, or below the required ABI |
| `supervision` | a stored conversation on a host that delegates no cgroup |

An `EACCES` from exec itself is `ErrLaunchDenied` (it matches `ErrPTYAllocation`): it keeps the errno
and names the binary, but does not prove Landlock was responsible. A missing binary stays
`ErrBinaryNotFound`.

## Harness profiles

Profiles are versioned manifests (`internal/contain/profiles/*.json`) checked against the versions
harness-wrapper pins:

| Harness | Identified by | Grants beyond the baseline | State root |
|---|---|---|---|
| claude-code 2.1.270 | the SHA-256 of its Bun-compiled ELF (release checksums per platform); a node-started layout is refused | the executable file; `/etc/claude-code` when present | `CLAUDE_CONFIG_DIR=$HOME/.claude` |
| codex 0.144.5 | `bin/codex.js` in an `@openai/codex` package root at that version, with the matching vendored platform package | the exact package root and the `node` its shebang resolves to; `/etc/codex` when present | `CODEX_HOME`, beside TMPDIR |

A contained **codex runs only at the bypass rung** (`--permission-mode bypass` / `danger-full-access`,
or `--dangerously-bypass-approvals-and-sandbox`): codex's bubblewrap sandbox needs user namespaces and
mounts the domain denies, so `read-only` and `workspace-write` (the manual, ask and auto rungs) are
refused rather than rewritten — codex would start and then fail every tool call. There, the domain is
the only boundary.

Each profile is **activated** only once its authenticated conformance runs pass (a real model turn
with file edits, a subagent, one stdio MCP server, resume, managed settings and a login in a caller
StateDir). Until then a contained launch of that harness is refused at the `profile` stage, except
its login (see [Signing in](#signing-in)).

## Private state

Every contained launch gets private state beneath `$XDG_STATE_HOME/harness-wrapper/contain`
(`~/.local/state/…`): a session root (0700, never granted to the harness) holding the lifecycle
record and a lock, and the granted `home/`, `tmp/` and harness-state directories. The harness gets a
**minimal environment**: `PATH`, locale and terminal variables, `USER`/`LOGNAME`/`SHELL`, its own
authentication variables (claude: `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`; codex:
`CODEX_API_KEY`, which is also seeded as an API-key `auth.json`), `IS_SANDBOX`, your `pass_env` names,
and the wrapper-controlled `HOME`, `TMPDIR`/`TMP`/`TEMP`, `PWD` and state root. Everything else —
unrelated credentials, proxies, socket paths — is absent.

Only minimal seeds are written: claude's onboarding flag and folder trust for the working directory
(and approval of an `ANTHROPIC_API_KEY`); codex's update-check, plugin and PTY-tool switches and
folder trust. Your own harness home, caches, hooks and MCP definitions are never copied, and rotating
OAuth credentials (claude's `.credentials.json`, a ChatGPT `auth.json`) are never duplicated — a
session that logs in that way runs from a `state_dir` that holds its own login, deliberately shared.

Wrapper-side readers — session-id discovery, history, transcripts, usage — read the harness's files
from the same private layout, never from your own `~/.claude` or `~/.codex`.

## Signing in

A contained session cannot use your own harness login: `~/.claude` and `~/.codex` are outside the
domain, and a copy of their rotating OAuth credentials would break both copies at the next refresh.
It authenticates in one of two ways:

- **From the environment**: `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` for claude,
  `CODEX_API_KEY` for codex.
- **With a login of its own**, kept in a `state_dir`. A person signs in once, and every session that
  names the same directory runs as that login and refreshes it in place.

`harness-wrapper contain-login` (or `wrapper.StartLogin`) creates that login. It runs the harness's
**own** login command inside the same boundary a session gets, with the `state_dir` as its HOME and
harness state:

| Harness | Command | The person | TCP |
|---|---|---|---|
| claude | `claude auth login` | opens the printed page, signs in, and pastes the code the page shows back at the prompt | Must stay unrestricted. claude starts a localhost callback listener even when the code is pasted, so its login is refused under `restrict_tcp`. |
| codex | `codex login --device-auth` | opens the printed page and enters the one-time code printed with it, valid for 15 minutes; the login then finishes by itself | `--contain-restrict-tcp --contain-allow-tcp 443` works. |

Then it runs the harness's status command (`claude auth status --json`, `codex login status`) in the
same directory, and reports a login only if that command finds one; `contain-login --status` runs just
this check. Neither command receives the harness's credential variables, so the answer is about the
directory alone.

A login launch runs only those two commands, which the versioned profile pins, and nothing else. That
is why it works before the profile is activated for sessions. The pinned commands and what the wrapper
reads from their output are part of the profile, so a harness version bump re-verifies them.

The directory holds live credentials (`home/.claude/.credentials.json` for claude, `codex/auth.json`
for codex) in 0700 directories. Every session that names it shares them, deliberately. Do not copy it:
give each independent login its own directory.

From Go:

```go
l, err := wrapper.StartLogin(ctx, wrapper.LoginConfig{
	Harness:    "codex",
	BinaryPath: codexPath,
	StateDir:   "/srv/hw/codex-login", // created 0700 when missing
	Containment: &wrapper.Containment{
		Kind: wrapper.ContainmentLandlock, RestrictTCP: true, ConnectTCP: []uint16{443},
	},
})
if err != nil {
	return err
}
p, err := l.Prompt(ctx) // p.URL, and p.UserCode for codex
if err != nil {
	return err
}
show(p) // to the person signing in
// claude: p.WantsCode is true; pass on the code the page showed:
//   err = l.SubmitCode(code)
res, err := l.Wait() // res.LoggedIn comes from the harness's status command
```

Sessions then name the directory: `Containment: &wrapper.Containment{Kind: wrapper.ContainmentLandlock,
StateDir: "/srv/hw/codex-login"}`. `wrapper.LoginStatus` reports whether a directory holds a login.

A session in a working directory the state directory has not seen before may ask claude's or codex's
folder-trust question once, as the harness does uncontained: the seeds that pre-answer it are written
only into a new state directory.

## Supervision and cleanup

Where the host delegates a cgroup (a unit or scope with `Delegate=yes`, or root in a container with a
writable cgroup namespace), each contained launch runs in its own **cgroup v2**: the harness is
created inside it, and no descendant can leave (the domain denies cgroupfs writes and the user
manager's sockets). However the session ends — Stop, cancellation, a failed launch, the harness's own
exit — it ends the same way: SIGTERM to the harness's process group, the grace period, then
`cgroup.kill`, and `Wait` returns only once the cgroup reports `populated 0`. Private state is then
deleted.

Without delegation (a login-session scope, a read-only cgroupfs) the launch still proceeds and says so:
`supervision: none`, and the private state is **kept**, reported as `cleanup: incomplete`, for you to
remove — nothing can prove the harness's detached descendants are gone.

## Stored conversations

A `chat.Open` with containment is a **stored contained conversation**: its containment record — the
normalized policy, profile version and private-state identity — is persisted to the Store *before* the
harness starts, the Store must implement `chat.ContainmentStore` (memstore does), and cgroup
supervision is required (every resume reuses the same private state). `chat.Reopen` inherits the
record; an explicit request must be the same policy, and a different one, a changed path target, a
deleted state or an unavailable profile is refused. Containment cannot be added to an existing
uncontained conversation. `chat.DeleteContainmentState` ends the last launch's cgroup and deletes the
state.

In a contained session the legacy `Session.HarnessSessionID` stays empty; the harness's id lives in
the containment record. Read it with `Session.HarnessID()`. An older harness-wrapper therefore sees no
id and refuses to resume a contained conversation, rather than resuming it unrestricted.

`harness.RunTurn`, `oneshot`, `structured-run`, harness-chatd and the CLI are single-launch callers:
their conversation is never reopened, so they do not require supervision.

## Seeing what applied

`Session.Containment()` (and `Conversation.Containment()`, `TurnResult.Containment`,
`Outcome.Containment`, the structured result's `containment` key, and the gateway's echoes) returns the
**applied policy**: kind, kernel and required ABI, profile and manifest version, every grant with its
canonical target, rights and source, TCP mode, scopes, state layout, supervision mode and cleanup
outcome, the names of the provisioned variables, omitted optional paths, and a **fingerprint** that is
stable across sessions under the same policy. The trace records `containment_applied`,
`containment_refused` (with the stage) and `containment_cleanup`. An absent policy means no
containment was applied.

`harness-wrapper contain-check [--json] [wrapper flags] <harness> -- [args]` prints the same policy
as a **preview** — placeholders for directories not yet allocated, the kernel ABI, whether supervision
is available, omitted paths and every requirement that would refuse the launch — without starting
anything. Its fingerprint matches the one a launch under the same policy reports.

## Compatibility with older components

A component built before containment would ignore a `containment` field it does not know — a silent
fail-open for a security option. So: the clients send containment only after `GET /v1/capabilities`
lists the kind (an older gateway answers 404, and the client refuses); callers speaking HTTP directly
must do the same, and the open and turn responses echo the applied policy so a skipped check is
visible; an older `harness-wrapper` binary or guest runner rejects the unknown `--contain` flags; and
an older reader of a stored conversation refuses to resume a contained record.
