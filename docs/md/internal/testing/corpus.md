# Corpus

`test/corpus/` holds recorded PTY byte streams. It began as the substrate for the
[emulator bake-off](../decisions/adr-001-vt100.md) (pick an emulator by measurement, not reputation) and
is now the input to the Layer-2 [adapter replay tests](README.md) and the
[drift pipeline](../versions-drift.md).

## Layout

```
test/corpus/
  <harness>/<scenario>/
    bytes.raw          required  raw PTY byte stream captured from the harness
    meta.json          required  harness, recorded_at, terminal dims, binary version
    expected.txt       optional  ground-truth final assistant text (fidelity metrics)
    transcript.jsonl   optional  copy of the harness's own session log, for reference
```

`<harness>` is `codex`, `claude-code`, `pi`, …; `<scenario>` is a short kebab-case name. Every
`meta.json` records the binary version it was taken against — so a **mixed-version corpus is fine**:
the adapter tests assert version-independent structure, not text.

## Canonical scenarios

Each harness ships at least these six (the `SCENARIOS` list the [drift pipeline](../versions-drift.md)
iterates):

1. **`short-reply`** — single turn, short text-only reply.
2. **`long-markdown`** — long reply with headings, lists, inline code.
3. **`code-block`** — a fenced code block ≥ 30 lines.
4. **`interrupted-mid-reply`** — user hits Ctrl-C while the model streams.
5. **`tool-call`** — a turn that invokes a tool and shows its output region.
6. **`multi-turn`** — three exchanges in one session.

## Version-shape recordings

Beyond the six, a harness may ship a scenario recorded specifically to pin the SHAPE a release
renders, so upstream drift in that shape fails a test instead of hanging a run:

- claude-code **`settled-after-turn`** (2.1.247) — a settled post-turn screen whose end-of-turn
  summary carries the trailing status clause (`✻ Crunched for 2s · done 5:06 AM`) on a reply long
  enough that the `Claude Code` startup banner has scrolled out of the viewport. Both halves matter:
  the clause is what the end-anchored `thinkingRE` used to reject, and the missing banner is what
  `pkg/chat.readyForInput` used to require — so a finished turn had no way to complete at all. Driven
  by `test/scripts/claude/settled-after-turn.json`.

  2.1.247 runs its TUI on the **alternate screen**, so the tail of `bytes.raw` is the alt-screen exit
  (`CSI ?1049l`), which blanks the emulator. Replay this recording **incrementally** and read the last
  frame carrying the clause — which is what production does anyway, since the wrapper reads frames as
  they land and never sees the teardown. A single whole-file `Write` snapshots a blank screen.

## Adversarial recordings

Alongside the canonical scenarios, `test/corpus/<harness>/adversarial/` holds **negative** recordings —
cases where the assistant *echoes the marker shape in its reply* and the regex must **not** fire.
`TestAdapter_AdversarialNoFire` locks these in. Examples: claude-code's `thinking-line-mid-reply` (an
intermediate `✻` marker mid-turn), codex's `prefix-only-marker` (a `Token usage:` line without the
full footer) and `partial-stream-no-footer`. There are also `synth/` fixtures (synthetic streams:
`scrollback-overflow`, `alt-screen-toggle`, …) for emulator-level edge cases.

## Recording

Most refreshes go through `make rebake-corpus` (scripted, see [Versions & Drift](../versions-drift.md)).
For a one-off **interactive** recording, drive the harness by hand:

```bash
go run ./internal/screenbench/cmd/screenbench-record \
    --harness codex \
    --bin "$(which codex)" \
    --out test/corpus/codex/short-reply \
    --cols 120 --rows 40 \
    --binary-version "$(codex --version)" \
    --notes "single-turn short reply"
```

The recorder launches the harness in the foreground under wrapper supervision; interact naturally, and
on exit `bytes.raw` + `meta.json` are written. Populate `expected.txt` by hand with the final
assistant message (copy from the harness's own [session JSONL](../transcript.md) if convenient).

## Running the bake-off

```bash
go run ./internal/screenbench/cmd/screenbench --corpus test/corpus
go run ./internal/screenbench/cmd/screenbench --corpus test/corpus --format markdown > bench-report.md
```

Metrics per (scenario × emulator): **Exact / Distance / NDist** (final extracted text vs
`expected.txt`; NDist is Levenshtein / max-runes), **Time / MB/s**, and **Alloc (MB)**.

## Privacy

Recordings may contain whatever you typed and whatever the model said. Treat scenarios as **public**
before checking them in — strip secrets, paths, and internal info. Prefer scripted prompts that
exercise TUI features over real conversations.
