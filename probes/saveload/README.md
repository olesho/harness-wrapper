# Save and Load: the feasibility probe

Can a Session of Claude Code, or of Codex, be saved as files and continued in
a fresh environment? This probe answers that for the versions hw pins, before
any archive API is built. It is Step 1 of
[Save and load an agent's conversation: Claude Code and Codex](https://coplan.olehluchkiv.com/d/save-and-load-an-agents-conversation-cla).
What it found is in [FINDINGS.md](FINDINGS.md), and the runs behind that in
[evidence/](evidence/).

It ships nothing. There is no archive format here and no API: the probe copies
files by hand, which is what Step 2 replaces.

## Run it

The probe is a Go test package that skips without the binaries it names.
`fetch.sh` downloads the pinned ones for this platform and checks them against
their publishers' checksums:

```sh
probes/saveload/fetch.sh /tmp/pinned
export HW_REAL_CLAUDE=/tmp/pinned/claude HW_REAL_CODEX=/tmp/pinned/codex

# Both harnesses against the mock model API: no account, no network.
go test -count=1 -v -run 'SaveLoad$' ./probes/saveload/

# Beside the gate: what codex does with an active goal.
go test -count=1 -v -run TestCodexActiveGoal ./probes/saveload/
```

A live run uses the harness's real model API and spends a few short turns of
the account it is given. Each needs a credential file; without one the test
skips, and that check is pending:

```sh
# Claude Code: a `claude setup-token` token. The model is haiku unless
# HW_SAVELOAD_CLAUDE_MODEL names another.
HW_SAVELOAD_CLAUDE_TOKEN_FILE=~/.config/agentd-smoke/token \
  go test -count=1 -v -timeout 30m -run 'TestClaudeCodeSaveLoadLive$' ./probes/saveload/

# Codex: an OpenAI API key, or a ChatGPT workspace's Codex access token
# (HW_SAVELOAD_CODEX_ACCESS_TOKEN_FILE). HW_SAVELOAD_CODEX_MODEL names a model.
HW_SAVELOAD_CODEX_API_KEY_FILE=/path/to/key \
  go test -count=1 -v -timeout 30m -run 'TestCodexSaveLoadLive$' ./probes/saveload/

# Codex, on the ChatGPT login of a codex on this machine.
HW_SAVELOAD_CODEX_LOGIN=~/.codex/auth.json \
  go test -count=1 -v -timeout 30m -run 'TestCodexSaveLoadLive$' ./probes/saveload/
```

A borrowed ChatGPT login is not one of the adapter's credential kinds, so that
run differs from an agentd agent's in how codex gets its credential, and in
nothing else: the probe sets codex's credential store to a file and gives each
environment an `auth.json` holding the login's access token. It never copies
the refresh token, which is spent when it is used: a copy that refreshed would
log the lender out. The run works while the access token lasts.

| Variable | What it does |
|---|---|
| `HW_SAVELOAD_EVIDENCE=DIR` | Writes each run's evidence to `DIR/<harness>-<mode>.json` and `.md`. |
| `HW_SAVELOAD_WORK=DIR` | Builds the environments under `DIR` and leaves them there. Not for a live run: each environment holds a copy of the credential. |
| `HW_SAVELOAD_EXPORT=DIR` | Also leaves the archives in `DIR`, with what a load on another machine needs to check them. |
| `HW_SAVELOAD_IMPORT=DIR` | Saves nothing: loads the archives another machine exported. The check `source.elsewhere` fails if it is the same machine. |
| `HW_CLAUDE_CODE_HOOK=FILE` | A built `claude-code-hook`, for a machine without the Go tool. |
| `HW_SAVELOAD_REVISION=REV` | Names the tree a test binary was built from, where it runs away from it. |

To save on one machine and load on another, build the test once and run it on
both (the harness binaries must be the same versions there):

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o saveload.test ./probes/saveload/
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o claude-code-hook ./cmd/claude-code-hook

# on the first machine
HW_SAVELOAD_EXPORT=$PWD/export ./saveload.test -test.v -test.run 'SaveLoad$'
# copy export/ to the second machine, then there
HW_SAVELOAD_IMPORT=$PWD/export ./saveload.test -test.v -test.run 'SaveLoad$'
```

## What a run does

For one harness, in one mode (`mock` or `live`):

1. **Source.** It makes an environment the way agentd does — five roots, a
   harness distribution, the spec agentd's API renders — and opens a fresh
   Session through the Harness Adapter. Two turns follow: one puts nonce A
   into the conversation, one runs a tool that prints nonce T. Against the
   mock, Claude Code takes a third that hands a task to a subagent.
2. **Stop.** It closes the Session as a park does and waits until no process
   names the source. For Codex it then names the thread and gives it a paused
   goal, through an app-server of its own.
3. **Save.** It lists every file the source holds, and copies each variant's
   files into a standalone tar with a manifest of paths, sizes and hashes. The
   first variant is the candidate recipe; the others take it apart.
4. **Take the source away.** The source directory is moved and made
   unreadable. From here on nothing may read it, and the last check proves
   nothing did.
5. **Load.** Into an environment whose every root has another path: restore
   the archive through the relocation rules, render the configuration with a
   fresh credential, read the restored record to its end with the adapter's
   record handle and no harness running, then reopen the Session by its native
   id from that checkpoint.
6. **Continue.** One turn; stop; reopen from the new checkpoint; two more. None
   of the prompts names a nonce.
7. **Negative control.** The same load with nothing restored.

## The checks

Required checks are the gate: a run fails unless all of them pass. The others
(`info`) record what was found.

| Check | Passes when |
|---|---|
| `source.turns` | Both source turns completed: a reply with nonce A, a tool result with nonce T. |
| `source.stopped` | Close reported stopped and drained, and no process names the source. |
| `source.aux` | Codex only: the thread's name and goal were set and read back. |
| `archive.standalone` | The candidate's files are in the archive, each with its size and hash. |
| `source.inaccessible` | The source's paths are gone before the restore starts. |
| `source.elsewhere` | Import only: the archive was saved on another machine. |
| `load.record-read` | The record handle reads the restored record to its end: the saved prompt, reply, tool call and tool result, a checkpoint at the file's last byte, no reset, rescan or fault. |
| `load.history` | Mock: the first resumed model request carries the saved turns and the tool result, in order, before the new prompt. Live: the model recalls nonce A without a tool. |
| `load.same-session` | The Session reopened under its saved native id. |
| `load.new-events-only` | The reopened Session delivered only its own turn's record events: no id the restored record held, none dated before its end. |
| `load.second-reopen` | After a stop and a second reopen, the conversation also carries what the restored Session added, and again only new record events follow. |
| `load.new-cwd` | A tool run after the restore printed the new workspace. |
| `load.no-rewrite` | The continued record still starts with the saved bytes: it was appended to, not rewritten. |
| `load.fresh-credential` | Mock only: every request after the load carried the new environment's credential, none the source's. |
| `load.aux`, `load.aux-kept` | Codex only: the name and goal read back after the load, and again after the second reopen. |
| `negative.harness-refuses` | With nothing restored, the harness alone refuses to resume the id. |
| `negative.no-context` | With nothing restored, a reopen through the adapter fails or lacks the earlier context. |
| `source.untouched` | The source's path is still absent, and the copy kept out of reach is unchanged. |

## Keep the artifacts private

A run's archives hold whole transcripts: system prompts, and in a live run
whatever the model said. They stay in the run's work directory, which a test's
end removes. The evidence is what is meant to be kept: paths by the run's own
names (`$WORK`), the start of each text, nothing shaped like a credential.
