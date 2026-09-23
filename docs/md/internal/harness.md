# Harness profiles & runs

`pkg/harness` is the **per-harness orchestration and capability layer**. It sits above
[`pkg/wrapper`](wrapper.md) and beside [`pkg/chat`](../guide/chat.md), and answers a different question
than either: *given this harness binary, this argv and this working directory, what can we actually
do with it on this run — and how do we get its transcript out?*

> **Not to be confused with `pkg/turns/harness/*`.** Those are the TUI *turn adapters* (screen markers,
> busy detection). `pkg/harness/*` is about **capabilities and orchestration**: session-id extraction
> from a machine-readable stream, resume argv, hook installation, transcript acquisition.

![pkg/harness entry points, profiles and acquisition](../diagrams/harness-runtime.svg)

## Two entry points

```go
func Run(ctx context.Context, cfg Config) (Result, error)          // headless supervision
func RunTurn(ctx context.Context, cfg TurnConfig) (TurnResult, error) // exactly one interactive turn
```

They are independent — neither calls the other.

| | `Run` | `RunTurn` |
|---|---|---|
| Builds on | `wrapper.Start` + a durable line tap | `chat.Open` (which builds on the wrapper) |
| Interaction | none — the harness runs to completion | one prompt, one assistant turn |
| Output | a stream of transcript envelopes, plus a supervised `wrapper.Result` | the completed `chat.Turn` + history |
| Used by | external hosts (this is the library surface an orchestrator consumes) | [`harness-wrapper run`](../guide/cli.md#one-shot-run), [`POST /v1/turns`](../guide/gateway.md#one-shot-post-v1turns), [`pkg/oneshot`](oneshot.md) |

## The profile registry

A **Profile** is a harness's capability descriptor. Profiles self-register in `init()`; importing
`pkg/harness/all` pulls in every built-in one (`claude`, `codex`, `opencode`, `pi`).

```go
type Profile interface {
	Name() string
	Resolve(ctx ResolveContext) ResolvedProfile
}

func Register(name string, p Profile)   // panics on a duplicate name
func For(name string) (Profile, bool)
func Registered() []string              // sorted
```

`Resolve` is called **once per run** with the concrete inputs, so a capability can be confirmed rather
than assumed:

```go
type ResolveContext struct {
	BinaryPath  string   // the resolved executable
	Args        []string
	Env         []string
	Cwd         string   // the harness's working directory
	ConfigRoots []string // directories that may be probed non-destructively
}

type ResolvedProfile struct {   // a non-nil field means "confirmed for THIS run"
	SessionID SessionIDExtractor // ExtractSessionID(line string) (string, bool)
	Resume    Resumer            // ResumeArgs(sessionID string) []string
	Stream    StreamParser       // ParseStreamLine(line string) []transcript.ParsedEvent
	Hooks     HookProvider       // see below
}
```

### Capability matrix

| Harness | SessionID | Resume | Stream | Hooks | Resume argv |
|---|:--:|:--:|:--:|:--:|---|
| `claude` | ✅ | ✅ | ✅ | ✅ | `--resume <id>` |
| `codex` | — | ✅ | — | — | `resume <id>` |
| `opencode` | ✅ | ✅ | — | — | `--session <id>` |
| `pi` | ✅ | ✅ | ✅ | — | `--session <id>` |

Every `ResumeArgs` returns `nil` for an empty id. Session-id extraction reads the harness's own
machine-readable line: claude's `system`/`init` record, pi's session header, opencode's session field
(either spelling).

Claude uses positional `--resume` deliberately — its `--session-id` flag is rejected unless paired
with `--continue`/`--resume` *and* `--fork-session`.

## `Run` — headless supervision with transcript acquisition

```go
type Config struct {
	Wrapper wrapper.Config // embedded; Wrapper.Harness selects the Profile

	RunID           string // stamped into every envelope
	ResumeSessionID string // "" = fresh session
	TranscriptMode  Mode   // TranscriptOff | TranscriptStreamParse | TranscriptHooks | TranscriptAuto
	HookCommand     []string // e.g. {"/abs/path/loom", "hooks"} — required for the hooks strategy
	Yield           *YieldControl

	OnDisplayLine    func(line string)                    // best-effort, drop-oldest
	OnEvent          func(transcript.EventEnvelope) error // durable, synchronous
	OnActivity       func(wrapper.Snapshot)
	ActivityInterval time.Duration // 0 = DefaultActivityInterval (10s)
}

type Result struct {
	wrapper.Result
	TranscriptStrategy  string // "stream" | "hooks" | "none" — what was ACTUALLY used
	HarnessSessionID    string
	DisplayLinesDropped uint64
	RetryAfter          time.Duration
	SawAPIError         bool
}
```

`harness.Run` **owns `Wrapper.OnLine`** — it overwrites whatever the caller set, because the durable
line tap is how session ids and streamed events are captured.

### Choosing an acquisition strategy

The mode is a *request*; the resolved profile decides what is possible.

| `TranscriptMode` | With `Hooks` capability | With only `Stream` | With neither |
|---|---|---|---|
| `TranscriptOff` (zero value) | none | none | none |
| `TranscriptStreamParse` | stream | stream | none |
| `TranscriptHooks` | hooks (+ a stream *shadow buffer* when Stream also exists) | stream | none |
| `TranscriptAuto` | hooks (+ shadow) | stream | none |

No `OnEvent` sink ⇒ always `none`, whatever the mode.

The **shadow buffer** exists for a specific failure: hooks may install correctly yet deliver nothing
for the parent conversation (a config that didn't take, a harness that never fired the event). At
drain time, if the spool produced zero parent-conversation events, the buffered stream events are
flushed instead and `Result.TranscriptStrategy` reports `"stream"`. You are told which one actually
produced the transcript, never which one was requested.

### Delivery guarantees

- `OnEvent` is **durable and synchronous**. It runs on the PTY read path; a returned error aborts the
  run (the harness is killed and `Run` reports a delivery failure), and a panic is converted into that
  same error rather than crashing the process.
- `OnDisplayLine` is **best-effort**. It is fed through a bounded queue that drops the *oldest* line
  under pressure and reports the count as `Result.DisplayLinesDropped`, so human-facing output can
  never back-pressure the harness.
- Every emitted event is stamped with a monotonic sequence number, the current schema version, and the
  captured harness session id before it reaches the sink.

## `RunTurn` — exactly one interactive turn

```go
type TurnConfig struct {
	Harness     string // required — also selects the chat adapter unless TurnHarness is set
	TurnHarness string // override for the adapter name
	BinaryPath  string // required
	Args        []string
	WorkingDir  string
	Env         []string

	Effort, Model, PermissionMode string
	Prompt                        string
	ExitAfterTurn                 bool // false = the Conversation stays live and the caller closes it
	Cols, Rows, EventBuffer       int

	InputPolicy               *chat.InputPolicy
	OnInputRequest            func(chat.InputRequest) (chat.InputAnswer, bool)
	AutoSkipCodexUpdateNotice bool
	Output                    io.Writer // best-effort copy of the PTY stream
}

type TurnResult struct {
	Turn                    chat.Turn
	Session                 chat.Session
	History                 []chat.Turn
	HistorySource           chat.HistorySource // "transcript" | "store"
	WrapperResult           wrapper.Result     // only when ExitAfterTurn
	ProcessStoppedAfterTurn bool
	Conversation            *chat.Conversation // non-nil only when !ExitAfterTurn
}

var ErrTurnErrored = errors.New("harness: turn errored")
```

The sequence: open a conversation with an in-memory store → optionally tee the PTY to `Output` →
acquire control, send the prompt, wait for that turn to reach `complete` or `errored` → if
`ExitAfterTurn`, quit the harness gracefully (bounded, so a harness that ignores `/quit` cannot hang
the call), re-read the session and history, close, and wait for the process.

A turn that ends in `errored` returns `ErrTurnErrored` **with a populated `TurnResult`** — the reply
text and reason are still there. Callers print the reply first and then report the failure.

The graceful quit is what makes the harness flush and close its own session log, which is why an
`ExitAfterTurn` result usually has `HistorySource == "transcript"` rather than the lossy store
fallback.

`TurnConfig.Harness` uses the CLI's spelling; internally `claude` maps to the `claude-code` adapter
name. `TurnConfig.Args` is passed **verbatim** — `RunTurn` never injects permission flags; that policy
lives at the [CLI boundary](../guide/permissions.md#the-two-halves-of-bypass).

## Hooks

Hooks are how a harness that supports them reports structured events *while it works*, instead of only
leaving a log behind. `pkg/harness` installs the hook configuration, hands the harness a command to
run, and drains what that command spooled.

```go
type HookProvider interface {
	HookSpec() *HookSpec
	ParseHookPayload(ctx HookContext, event string, stdin []byte) ([]transcript.ParsedEvent, error)
	EnsureConfig(worktreePath string, loomArgv []string) error
}

type HookSpec struct {
	ConfigPath string      // worktree-relative, e.g. ".claude/settings.json"
	Events     []HookEntry // NativeEvent, Matcher, Arg
	Yield      *HookEntry  // optional pre-tool guard
	Owner      string      // marker stamped on managed entries
}
```

### What gets written where

`EnsureConfig` writes the harness's own settings file inside the **worktree** — for claude,
`<worktree>/.claude/settings.json`. The write is:

- **locked** — a stable `<file>.lock` sidecar is `flock`ed for the whole read-modify-write, so two
  concurrent runs in the same worktree cannot interleave;
- **atomic** — rendered to a temp file next to the target and renamed into place, at `0600`;
- **surgical** — only entries carrying this repo's owner marker are replaced. User-authored matchers,
  unrelated events, and unknown top-level keys survive untouched. Managed entries are grouped by
  native event first, so one event carrying several managed matchers doesn't delete its own siblings.

The command written into the config is self-disarming:

```sh
sh -c 'test -n "$HW_EVENT_SPOOL" || exit 0; exec <quoted hook argv> <harness> <arg>'
```

If the harness is later launched *without* the orchestrator's environment, the hook is a no-op instead
of an error. Argument quoting is POSIX single-quoting throughout, so paths with spaces or quotes are
safe.

### The environment contract

| Variable | Set when | Read for |
|---|---|---|
| `HW_EVENT_SPOOL` | always, under the hooks strategy | the spool directory — **absent means the hook handler is inert** |
| `HW_HOOK_CWD` | always | the harness's working directory |
| `HW_HOME` | always | the user home, used to locate the harness's transcript root |
| `HW_HARNESS_CONFIG_DIR` | never set here; read if present | overrides the derived config directory |
| `HW_HARNESS_SESSION_ID` | only when resuming | arms the resume-session guard |
| `HW_YIELD_FILE` | only when a `YieldControl` is configured | cooperative preemption |

The hook subprocess entry point is:

```go
func HandleHookEvent(harnessName, event string, env []string, stdin []byte) (HookOutcome, error)
```

It reads its context **only** from those variables — never from the process's own working directory —
because a hook runs wherever the harness happened to spawn it.

### The spool

Each hook invocation writes one JSON file into the spool directory. The writer creates a unique temp
file at `0600`, fsyncs it, renames it into place and fsyncs the directory. A reader therefore never
sees a partial event, and once the hook reports success the file survives a crash.

There are two ways to consume the spool:

```go
func DrainSpool(spoolDir string) ([]transcript.ParsedEvent, error)

func ReadSpool(spoolDir string) (SpoolContents, error)
func AckSpool(spoolDir string, receipts ...SpoolReceipt) error
```

- **`DrainSpool`** reads and removes the files in one step. It is destructive and not transactional:
  a file is gone before the caller has done anything with its events, so a crash in between loses
  them. `Run` uses it because its consumer lives and dies with the run. Malformed files are skipped
  and *counted* in the returned error rather than silently dropped, and a missing directory is not an
  error.
- **`ReadSpool` then `AckSpool`** serve a consumer that must not lose events, such as agentd, which
  never calls `DrainSpool`.
  - `ReadSpool` returns each file's events with a receipt and leaves the file in place. The receipt
    holds the file's name in the spool, its size and its SHA-256 digest.
  - The consumer commits the events durably, then passes the receipts to `AckSpool`.
  - `AckSpool` deletes each file only while it still holds exactly the bytes that were read, then
    fsyncs the directory.

  A crash at any point replays the unacknowledged files under the same receipts, so the consumer can
  recognise what it has already committed. Repeating an ack is a no-op.

The reader treats the spool as untrusted, because under agentd the hook runs as a less trusted user
than the reader:

- It refuses a spool directory that is itself a symlink, and confines every lookup to the directory
  (`os.Root`).
- It reads only regular, singly linked files of at most `MaxSpoolFileBytes` (64 MiB). It never follows
  a symlink and never blocks on a FIFO.
- It quarantines any file it cannot use: another kind of file, an oversize or hard-linked one, a name
  `AckSpool` would refuse, or contents that do not parse.
  - The file moves into the spool's `quarantine/` directory and is reported, with the reason, in
    `SpoolContents.Quarantined`.
  - The quarantine keeps at most `MaxSpoolQuarantine` (32) files. Once it is full, further files are
    deleted, and the report says so.
- It leaves in place a file it cannot open, such as one it lacks permission to read, and names it in
  the returned error.
- One call reads at most 64 MiB and 1024 files, and sets `SpoolContents.More` when it leaves files
  for the next call.

`AckSpool` refuses a receipt, deleting nothing, in two cases. The error wraps `ErrSpoolReceipt`.

- The name is not a plain `.json` name directly inside the spool.
- The file changed or was replaced since it was read, including by a symlink.

Two filters keep a drained spool honest:

- **Resume guard** — when resuming a known session, parent events belonging to a *different* session
  are dropped. Subagent events are always kept, since their parent id is what identifies them.
- **Authority filter** — when the same logical event can arrive twice (once live, once from the file),
  exactly one copy is admitted: under the stream strategy the live copy wins; under hooks the file copy
  wins for conversation content, while non-conversation events (session metadata and the like) are
  still accepted live. Subagent events bypass the filter entirely.

### Yield: cooperative preemption

```go
type YieldControl struct{ /* … */ }

func NewYieldControl() (*YieldControl, error)
func (y *YieldControl) Request(reason string) error
func (y *YieldControl) FilePath() string
func (y *YieldControl) Clear() error
func (y *YieldControl) Close() error
```

An orchestrator that needs the agent to stop — a higher-priority task, a release slot — writes a
reason into the yield file. The harness's pre-tool hook reads it and returns a *block* decision with a
human-readable reason, so the agent stops at its next tool call instead of being killed mid-edit. A
missing or unreadable file simply doesn't block.

## Transcript modes

```go
type Mode int
const (
	TranscriptOff Mode = iota // zero value — the safe default
	TranscriptStreamParse
	TranscriptHooks
	TranscriptAuto
)
```

`TranscriptAuto` is resolved to a concrete strategy before any event is filtered, so the authority
filter only ever sees `StreamParse` or `Hooks`.

## Tunables

| Knob | Default | Meaning |
|---|---|---|
| `DefaultActivityInterval` | 10s | `OnActivity` polling cadence when `ActivityInterval` is zero |
| graceful-quit wait | 3s | bound on the `/quit` write **and** on the subsequent exit wait in `RunTurn` |
| display queue depth | 1024 lines | drop-oldest bound behind `OnDisplayLine` |

Everything else about process supervision — idle thresholds, stale advisories, the SIGTERM→SIGKILL
grace — comes from the embedded [`wrapper.Config`](wrapper.md#config).
