# Transcripts

`pkg/transcript` holds **read-only** parsers for each harness's own on-disk session log. When a harness
records what the model actually said as JSONL, that is a far higher-fidelity source than
screen-scraped TUI text — so [`History`](../guide/chat.md#history) prefers it.

## Why read the harness's log

Screen-scraped reply text is best-effort: the TUI re-renders, wraps, and decorates. The harness's own
JSONL records the canonical message. The transcript reader is wired in through the adapter's
[`TranscriptReader`](turns.md#capability-interfaces) capability: once the
[session ID is extracted](../guide/chat.md#history), `History` calls
`ReadTranscript(harnessSessionID, workingDir)` and returns its parsed turns; otherwise it falls back
to the metadata `Store`.

## Per-harness logs

| Harness | On-disk path | Format | Status |
|---|---|---|---|
| **claude-code** | `~/.claude/projects/<encoded-cwd>/<session-uuid>.jsonl` | tool-aware Claude JSONL | ✅ |
| **codex** | `~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ts>-<uuid>.jsonl` | response-item roles | ✅ |
| **gemini** | `~/.gemini/tmp/<project>/chats/session-<ts>-<short-id>.jsonl` | dual-shape (parts / type+message) | ✅ |
| **pi** | `~/.pi/agent/sessions/--<cwd-slug>--/<ts>_<uuid>.jsonl` | JSONL v3, typed content blocks | ✅ |
| **opencode** | — | per-message JSON → SQLite (migrating) | ❌ deferred |

Locating the file is harness-specific: claude-code encodes the working directory into the path; codex
walks the `YYYY/MM/DD` tree for the uuid suffix; gemini and pi do a slug lookup with a directory-walk
fallback and confirm the match by an in-file ID header (guarding against shared-prefix false
positives). **opencode** is deliberately omitted — its store is mid-migration from per-message JSON
files to SQLite, and a reader that silently breaks across that change is worse than none.

## Canonical Event model

The line-parsing helpers (`line.go`, `parse.go`, `strip_tags.go`, and the claude-code line→Event
parser) are a port of `entireio/cli`'s transcript package, by way of loomcli's
`internal/sessions/transcript`. The canonical `Event` (one event per content block, tool-aware) is
field-compatible with loomcli's promoted `Event`, so the parser yields equivalent output. See
[`pkg/transcript/ORIGIN.md`](https://github.com/olesho/harness-wrapper/blob/main/pkg/transcript/ORIGIN.md)
for the full attribution; the upstream is MIT-licensed (reproduced in `LICENSE.upstream`).

## Drift

A harness can change its on-disk schema between releases just as it changes its TUI. The gemini reader
already tolerates two line shapes; `make schema-canary-gemini` re-records a short reply through the
live CLI and re-parses the fresh JSONL to catch schema drift early — see
[Versions & Drift](versions-drift.md).
