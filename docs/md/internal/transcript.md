# Transcripts

`pkg/transcript` holds **read-only** parsers for each harness's own on-disk session log, plus the
canonical event model everything else in the repo speaks. When a harness records what the model
actually said as JSONL, that is a far higher-fidelity source than screen-scraped TUI text — so
[`History`](../guide/chat.md#history) prefers it.

The package is a **leaf**: it imports neither `pkg/turns` nor `pkg/chat`, so anything may parse a
harness log — including a tool that never starts a harness.

![Transcript pipeline](../diagrams/transcript-pipeline.svg)

## Why read the harness's log

Screen-scraped reply text is best-effort: the TUI re-renders, wraps, and decorates. The harness's own
JSONL records the canonical message. The reader is wired in through the adapter's
[`TranscriptReader`](turns.md#capability-interfaces) capability: once the
[session ID is extracted](../guide/chat.md#history), `History` calls
`ReadTranscript(harnessSessionID, workingDir)` and returns its parsed turns; otherwise it falls back
to the metadata `Store`.

## Per-harness logs

| Harness | On-disk path | Format | Status |
|---|---|---|---|
| **claude-code** | `~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl` | tool-aware Claude JSONL | ✅ |
| **codex** | `~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ts>-<uuid>.jsonl` | response-item roles | ✅ |
| **pi** | `~/.pi/agent/sessions/--<cwd-slug>--/<ts>_<uuid>.jsonl` | JSONL v3, typed content blocks | ✅ |
| **opencode** | — | per-message JSON → SQLite (migrating) | ❌ deferred |

Locating the file is harness-specific: claude-code encodes the working directory into the path; codex
walks the `YYYY/MM/DD` tree for the uuid suffix (and can locate the *latest* session for a working
directory, which is how a session id is recovered when the TUI stopped rendering the resume hint); pi
does a slug lookup with a directory-walk fallback and confirms the match against an in-file ID header,
guarding against shared-prefix false positives. **opencode** is deliberately omitted — its store is
mid-migration from per-message JSON files to SQLite, and a reader that silently breaks across that
change is worse than none.

## The reader interfaces

```go
type Reader interface {
	Read(harnessSessionID, workingDir string) ([]Event, error)
}

// Optional: implemented by readers that can total a session's tokens.
type UsageReader interface {
	ReadUsage(harnessSessionID, workingDir string) (*Usage, error)
}
```

`workingDir` matters only for harnesses that index by directory (claude-code); others ignore it.
Implementations must be safe for concurrent use, and a partial read — some lines parsed, then a
malformed one — must **error rather than silently truncate**. Half a transcript that looks complete is
worse than a failure.

## The canonical Event

Every parser translates its harness's native shape into one `Event` stream, so consumers handle a
single model:

```go
const SchemaVersion = 1

type Event struct {
	Seq       int             `json:"seq"`
	Timestamp time.Time       `json:"timestamp"`
	Role      string          `json:"role"`  // user | assistant | tool | system
	Type      string          `json:"type"`  // text | tool_use | tool_result | session_meta
	Text      string          `json:"text,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
	Output    string          `json:"output,omitempty"` // tool_result text
	UUID      string          `json:"uuid,omitempty"`

	// Internal metadata — never part of the public DTO:
	SchemaVersion int    `json:"-"`
	Source        string `json:"-"` // live | file | hook
	NativeID      string `json:"-"` // primary identity, parser-owned
}

func (e Event) ID() string
func TurnsFromEvents(events []Event) []Turn
```

One event per **content block**, so a single assistant message that thinks, edits a file, and reports
back becomes several events rather than one blob of text.

### Identity, and why it matters

`ID()` is the dedup key, and identity is **parser-owned**: a parser sets a kind-qualified native id,
falling back to the message UUID, then the tool-use id (kind-qualified, so a tool call and its result
never collapse into each other), and finally a content hash.

That fallback hash is deliberately **cross-source stable** — it excludes anything parser-local or
arrival-time, so the same logical event observed *live* (streamed from the harness's stdout) and
*from the file* (read back from the log) produces one row, not two. The
[hook-driven acquisition path](harness.md#hooks) depends on exactly that property. A per-tool hook's event (`hook`,
[per-tool hooks](harness.md#per-tool-hooks)) is the one deliberate exception: its native id,
`hook:<argument>:<tool_use_id>`, keeps the moment a tool started or ended apart from the transcript's
copy of the call.

### Two serializations

| Form | Carries | Used for |
|---|---|---|
| **public JSON** | the fields above with `json:"-"` omitted | what callers see — `transcript_entries` on a [structured-run result](turnproto.md), and the DTO a UI renders |
| **durable wire** | *every* field, including provenance and native id | the hook spool and any durable event store |

Dropping provenance on a round trip would silently corrupt acquisition — the authority filter keys on
`Source`, and dedup keys on `NativeID` — so the durable form persists them explicitly rather than
reusing the public shape.

### Projection to chat turns

`TurnsFromEvents` flattens the event stream into the coarser `Turn` model
(`Role` / `Text` / `Timestamp`) that [`chat.History`](../guide/chat.md#history) returns. Multi-block
messages are joined; tool-call entries that fit no conversational role become `system`.

## Token usage

```go
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	ReasoningOutputTokens    int `json:"reasoning_output_tokens"`
}
```

The union of both harnesses' token fields. Two properties are contractual:

- **All five keys always serialize**, including zeros — none of the inner fields carries `omitempty`.
  `omitempty` belongs only on a containing struct's `*Usage` field. This keeps the Go object
  byte-compatible with the TypeScript implementation, which emits all five unconditionally.
- **`input_tokens` means different things per harness, and that is not a bug to fix.** Claude's
  excludes cache reads and creations (they live in the `cache_*` fields, matching the Anthropic API);
  codex's *includes* its cached count, which is a subset rather than an addition. Do not re-add the
  cached number to the input total.

## Following a transcript as it grows

`Read` parses a whole file. A consumer that stores events while the harness is still writing — agentd
keeps a harness's events in its own database — uses a **`Follower`** instead: it reads the complete
records past a checkpoint, a batch at a time, and moves on only when the caller says the batch is
stored.

```go
f, err := claudecode.Follow(sessionID, workingDir, launchEnv, storedCheckpoint)

b, err := f.Poll()
// errors.Is(err, transcript.ErrTranscriptReset): record the fault; the next Poll starts a new generation
// errors.Is(err, fs.ErrNotExist): claude has not written its first entry yet
// b.Checkpoint == b.From: nothing new
// otherwise, in ONE transaction: store b.Events, b.Errors and b.Checkpoint — then
err = f.Ack(b)
```

Only claude-code has a follower. Codex gets one when something consumes it.

### Locating the file

`claudecode.Locate(sessionID, workingDir, env)` finds the transcript by the launch adapter's rules:
`CLAUDE_CONFIG_DIR` from the harness's launch env (the last occurrence, trimmed, resolved against
`workingDir` when relative, since claude takes it verbatim from its own cwd), else `~/.claude`; then
the project directory named for the realpath of `workingDir`, falling back to `workingDir` as given.
A nil `env` means the harness inherited this process's environment, as `exec` treats it. The rules
live in two packages — `pkg/transcript` cannot import the adapter — and a test holds them together.

`Follow` does not need the file to exist. Claude creates its transcript with the first entry, so a
follower started at launch waits where claude will write it, and `Poll` reports `fs.ErrNotExist`
until then.

### Batches and acknowledgement

`Poll` never moves the follower. It returns a `Batch` — the events of every complete record after
`From`, the records it could not read, and the `Checkpoint` that covers them — and only `Ack(b)`
moves the follower to `b.Checkpoint`. The caller commits the events, the source errors and the
checkpoint in one transaction, then acknowledges in memory:

| What happens | What the follower does |
|---|---|
| the transaction fails | the next `Poll` returns the same records with the same identities |
| the process dies before commit | restarted from the stored checkpoint, it reads the batch again |
| the process dies after commit, before `Ack` | restarted from the stored checkpoint, it reads on from there |
| a batch polled before an earlier `Ack` is acknowledged | `Ack` refuses it |

`MaxBatchBytes` (default 4 MiB) bounds a batch at a record boundary; a record longer than that still
comes whole. `Offset()` is a convenience; resuming takes the whole checkpoint.

### Partial and unreadable records

Bytes after the last newline are a record the harness is still writing: they wait for their newline.
This is where the follower and `Read` differ — `Read` parses a valid final record that has no newline.

A complete record the decoder cannot read becomes a **`SourceError`** — its offset, its length with
the newline, the SHA-256 of those bytes, and why — carried by the batch that moves past it, so it is
recorded, never silently skipped. For claude that is a line that is not JSON, or a user or assistant
entry whose message cannot be read. Other entries hold no events and are skipped however they are
shaped: the `system` / `api_error` entry claude writes for each retry of a failed API call carries an
object where `Line` expects the string `error`, and `Read` drops it for that.

### The checkpoint

| Field | Meaning |
|---|---|
| `version` | `FollowerVersion` (1) when it was made; a follower refuses a newer one |
| `session_id` | the harness session |
| `generation` | a UUID for this incarnation of the file, minted when the follower starts on it and after every reset |
| `inode` | the file's inode, where the platform has one (only the inode: device numbers change across remounts) |
| `offset` | bytes covered, always just past a newline |
| `next_seq` | the `Seq` of the next event: events are numbered from 0 in each generation, as `Read` numbers a file |
| `prefix_len`, `prefix_sha256` | the file's first bytes, growing with the offset up to 4 KiB and fixed from then on |
| `boundary_len`, `boundary_sha256` | the bytes just before `offset`, up to 4 KiB |

Every `Poll` checks the file against the checkpoint before reading on. A **reset** is any of: a
different inode (`replaced`), a file shorter than the offset (`shrunk`), or changed prefix or boundary
bytes (`prefix_changed`, `boundary_changed`). Between them they catch rotation, truncation and a
truncate-and-regrow, across a restart as well as live. They are **not** proof against rewriting in
place: a change that keeps both windows and the size intact goes unseen.

A harness only appends to its transcript, so a reset is a **source-integrity fault**. `Poll` returns
it as a `*ResetError` (matching `ErrTranscriptReset`) with the reason, the checkpoint the file no
longer extends, and the file's size and inode now. The caller records the fault and keeps what
evidence it wants — a copy of the file — rather than trusting that the log only grows. The follower
has already moved to a new generation at offset 0. The next batch reads the file from its first byte,
and its checkpoint carries the new generation into the same transaction as its events. A process that
dies before storing the new generation reports the reset again when it restarts.

### Identity

A follower sets each event's `NativeID`, so `Event.ID()` returns it, to a versioned **follower
identity**. It is the most native one the record offers, qualified by the event's kind:

| Record | Identity |
|---|---|
| a tool event with a tool-use id | `v1:<kind>:tool:<tool-use id>` |
| any other event from a record with a uuid | `v1:<kind>:line:<record uuid>:<block index>` |
| an event from a record with neither | `v1:<kind>:gen:<generation>:<record offset>:<block index>` |

The block index is the block's position in the record's content array, so a block that yields no
event (a `thinking` block) does not shift the blocks after it, and several blocks under one uuid stay
apart. No two kinds share an identity. The first two forms are the harness's own ids, the same across
batches, restarts and resets: after a reset the file is read again and its native identities repeat,
and the caller's dedup drops them. A record without an id is known only by where it sits, so it
belongs to its generation. agentd scopes the identity further, by agent and harness session.

Identities can repeat within one generation, too: claude writes a session's earlier entries again,
verbatim — same uuid, message and timestamp — when the session resumes (seen with claude 2.1.181 and
2.1.197). The store keeps one event per identity, and a repeat must be a no-op there, not an error that
fails the batch's transaction for good.

`Read` keeps its legacy ids for its existing callers — text as `file:text:<uuid>:<seq>`, numbered by
position in the whole file — so the follower's identities differ from `Read`'s. The follower is held
to `Read` by content instead: the same events, in the same order, with the same `Seq`s, however the
file is appended to and however often the follower restarts.

### Usage

A follower accounts no usage. Claude repeats one API call's usage on every content-block line, so a
sum over batches would count a call once per block. When usage is added, the set of message ids seen
and the totals must be persisted with the checkpoint, atomically — or rebuilt from the retained file
before reading on — with reset behaviour defined and a restart between two blocks of one call tested.

## Line-parsing helpers

`Line`, `ParseLine`, `ParseFromBytes`, `ParseFromFileAtLine`, `SliceFromLine`, `ExtractUserContent` and
`StripIDEContextTags` are the shared JSONL primitives the claude-code reader is built on —
tail-following from a byte offset, extracting user content from mixed block shapes, and stripping
IDE-injected context tags that would otherwise appear as user text.

These, the canonical `Event`, and the claude-code line parser are a port of `entireio/cli`'s transcript
package by way of loomcli's `internal/sessions/transcript`; the public fields and JSON tags are kept
identical so a consumer can serve a byte-identical DTO. See
[`pkg/transcript/ORIGIN.md`](https://github.com/olesho/harness-wrapper/blob/main/pkg/transcript/ORIGIN.md)
for the full attribution; the upstream is MIT-licensed (reproduced in `LICENSE.upstream`).

## Drift

A harness can change its on-disk schema between releases just as it changes its TUI. Readers are
written to tolerate more than one line shape where a harness ships variants; the corpus canary
re-records a short reply through the live CLI and re-parses the fresh JSONL to catch schema drift
early — see [Versions & Drift](versions-drift.md).
