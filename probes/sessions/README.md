# Sessions side by side: the harness probe

Can several Sessions of one harness run at once in one agent's environment —
one config dir, one workspace, one staged credential — each in a Host of its
own? This probe answers that for the versions hw pins, before agentd or the
Harness Adapter Interface change. It is part of Step 0 of
[agentd: parallel Sessions in one agent](https://coplan.olehluchkiv.com/d/agentd-parallel-sessions-in-one-agent).
What it found is in [FINDINGS.md](FINDINGS.md), and the runs behind that in
[evidence/](evidence/).

It ships nothing. The two changes the plan asks of the profiles are made here
by hand: each Claude Code Session's hook spool is its own (the probe rewrites
`HW_EVENT_SPOOL` and `spool` in each Session's open configuration), and a
Codex home is initialized by one app-server before the others start (the probe
opens one Session alone first).

## What it checks

**Claude Code** (`TestClaudeSessions`). A control Session runs alone first:
what claude changes of `.claude.json` with no other Session there is not held
against the run. Then N Sessions open at once and take turns until the run's
time is up — a reply, a Bash call, a subagent, a background command and the
turn claude takes when it ends, streamed text — and one more opens at the end.

| Criterion | How |
|---|---|
| `.claude.json` parses after every write and keeps what was rendered | read every 20 ms; a state that does not parse is torn when it parses 300 ms later, corrupt when it still does not |
| no Session's start hangs | every open finishes inside two minutes |
| every transcript parses and holds only its own Session | each line JSON, each entry's `sessionId` the Session's; no transcript of no Session |
| every hook event lands in its own Session's spool | the hook helper is wrapped: a copy of each payload, named for the spool it reaches, must name that spool's Session; and each tool use's hook events reach its own Session's Host, none another's |
| a connector's headersHelper still runs at the end | the Session opened last sends the connector's header, read from its file, to the probe's MCP server |

**Codex** (`TestCodexSessions`). N app-servers start at once on a fresh
`CODEX_HOME`; then, in a second environment, one starts alone first and N
more at once, and those take turns: a reply, a shell call, streamed text, and
a goal codex works in turns of its own, which it keeps in its goals database.

| Criterion | How |
|---|---|
| every app-server starts behind the start lock, and the failure of [openai/codex#50290](https://github.com/openai/codex/issues/50290) shows without it | the opens' outcomes, in each environment |
| no turn fails on a busy database | every turn's outcome |
| `session_index.jsonl`, when codex writes it, parses with an entry per thread | each line JSON, each thread named in it |
| every rollout is whole | each line JSON, no line naming another thread |
| codex's databases pass SQLite's integrity check | `pragma integrity_check` on each, once every app-server stopped (needs `python3`) |

Both report each Session's peak resident memory — its harness and everything
beneath it, read from `/proc` on Linux — for the plan's sizing rule.

## Run it

The probe is a Go test package that skips without the binaries it names. The
save and load probe's `fetch.sh` downloads the pinned ones for this platform
and checks them against their publishers' checksums:

```sh
probes/saveload/fetch.sh /tmp/pinned
export HW_REAL_CLAUDE=/tmp/pinned/claude HW_REAL_CODEX=/tmp/pinned/codex

# Against the mock model API: no account, no network.
HW_SESSIONS_N=4 HW_SESSIONS_DURATION=30m \
  go test -count=1 -v -timeout 60m -run 'Sessions$' ./probes/sessions/
```

On a machine without the go tool, build the test binary and the hook helper
elsewhere and copy them there:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o sessions.test ./probes/sessions/
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o claude-code-hook ./cmd/claude-code-hook
HW_REAL_CLAUDE=… HW_CLAUDE_CODE_HOOK=$PWD/claude-code-hook ./sessions.test -test.v -test.run 'Sessions$'
```

A live run uses Anthropic's API and spends a few hundred short turns of the
account it is given:

```sh
HW_SESSIONS_CLAUDE_TOKEN_FILE=~/.config/agentd-smoke/token HW_SESSIONS_N=4 HW_SESSIONS_DURATION=5m \
  go test -count=1 -v -timeout 30m -run 'TestClaudeSessionsLive$' ./probes/sessions/
```

| Variable | What it does |
|---|---|
| `HW_SESSIONS_N` | How many Sessions run at once; 4 when unset. |
| `HW_SESSIONS_DURATION` | How long they take turns, as a Go duration; 2m when unset. |
| `HW_SESSIONS_EVIDENCE=DIR` | Writes each run's report to `DIR/<harness>-<mode>-n<N>.json` and `.md`. |
| `HW_SESSIONS_WORK=DIR` | Builds the environments under `DIR` and leaves them there. Not for a live run: they hold the credential. |
| `HW_CLAUDE_CODE_HOOK` | A built hook helper; otherwise it is built from this tree. |
| `HW_SESSIONS_CLAUDE_TOKEN_FILE` | A `claude setup-token` token: the live run's credential. |
| `HW_SESSIONS_CLAUDE_MODEL` | The live run's model; haiku when unset. |
