# Bake-off corpus

This directory holds recorded PTY byte streams used to evaluate vt100
emulator candidates for `pkg/screen`. Phase-1 of the chat-extension plan
selects an emulator by measurement, not by reputation — this corpus is
the measurement substrate.

## Layout

```
test/corpus/
  <harness>/<scenario>/
    bytes.raw          required  raw PTY byte stream captured from the harness
    meta.json          required  harness, recorded_at, terminal dims, binary version
    expected.txt       optional  ground-truth final assistant text (used for fidelity metrics)
    transcript.jsonl   optional  copy of the harness's own session log, for reference
```

`<harness>` is `codex`, `claude-code`, …. `<scenario>` is a short kebab-case
name describing the interaction.

## Canonical scenarios

Each harness should ship at least these six recordings so we evaluate
emulators on a representative spread of TUI behaviors:

1. **`short-reply`** — single user turn, short text-only assistant reply.
2. **`long-markdown`** — long assistant reply with headings, lists, inline code.
3. **`code-block`** — assistant reply that emits a fenced code block ≥ 30 lines.
4. **`interrupted-mid-reply`** — user hits Ctrl-C while the model is streaming.
5. **`tool-call`** — a turn that invokes a tool (shell, file edit, etc.) and shows the tool output region.
6. **`multi-turn`** — three user/assistant exchanges in one session.

More are welcome (especially edge cases that have caused TUI re-render bugs upstream).

## Recording

```sh
go run ./internal/screenbench/cmd/screenbench-record \
    --harness codex \
    --bin "$(which codex)" \
    --out test/corpus/codex/short-reply \
    --cols 120 --rows 40 \
    --binary-version "$(codex --version)" \
    --notes "single-turn short reply"
```

The recorder launches the harness in the foreground under wrapper
supervision. Interact naturally with the TUI. When you exit, `bytes.raw`
and `meta.json` are written; you populate `expected.txt` by hand with the
final assistant message text (copy from the harness's own session JSONL
if convenient — for Codex see `~/.codex/sessions/`, for Claude Code see
`~/.claude/projects/<encoded-cwd>/`).

## Running the bake-off

```sh
go run ./internal/screenbench/cmd/screenbench --corpus test/corpus
go run ./internal/screenbench/cmd/screenbench --corpus test/corpus --format markdown > bench-report.md
```

Metrics reported per (scenario × emulator) cell:

- **Exact / Distance / NDist** — final-screen extracted text vs `expected.txt`. NDist is Levenshtein / max-runes, in [0,1].
- **Time / MB/s** — wall-clock and throughput playing the entire byte stream.
- **Alloc (MB)** — `runtime.MemStats.TotalAlloc` delta over the run.

The chosen emulator is recorded in `docs/adr-001-vt100.md`.

## Privacy

Recordings may contain whatever you typed and whatever the model said.
Treat scenarios as **public** before checking them in — strip secrets,
paths, internal info. Prefer scripted prompts that exercise TUI features
rather than real conversations.
