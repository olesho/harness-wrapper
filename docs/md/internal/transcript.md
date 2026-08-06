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
	Source        string `json:"-"` // live | file
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
[hook-driven acquisition path](harness.md#hooks) depends on exactly that property.

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

## Line-parsing helpers

`Line`, `ParseFromBytes`, `ParseFromFileAtLine`, `SliceFromLine`, `ExtractUserContent` and
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
