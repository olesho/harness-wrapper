# ADR-001 (TS addendum): vt100 emulator selection for `test/corpus/tools/screenbench`

**Status:** Accepted (2026-07-16) — `@xterm/headless` chosen; extended by the bench and
fidelity-gate wiring (HARNESS-WRAPPER-21)

**Intent:** principle 1, *the screen is a contract we don't own*
([INTENT](../../../../INTENT.md#design-principles)) — and the success criterion that one turn reads
the same in both languages: the Go and TS benches replay one recorded corpus.

## Context

[ADR-001](adr-001-vt100.md) picked `vt10x` for the Go-side bake-off (`internal/screenbench/`) and
for `pkg/screen`. The Go bench has a TS-side sibling under `test/corpus/tools/screenbench/*.ts`,
reusing the same recorded corpus (`test/corpus/{codex,claude-code,synth}/**/bytes.raw`). This
addendum covers only the emulator choice for the TS tooling's `emulators.ts` adapter registry, and
the fidelity gates built on it; it does not redo the Go decision or touch `adr-001-vt100.md`.

No TS terminal-emulator library had been vetted in this repo before — zero prior `@xterm` (or
equivalent) references existed anywhere in the tree.

## Decision

1. **The TS bench wraps `@xterm/headless`** (the headless build of xterm.js), registered as
   `"xterm"` in `test/corpus/tools/screenbench/emulators.ts`.
2. **`snapshot()` reads the visible viewport, not the top of the scrollback buffer.**
   `@xterm/headless`'s `buffer.active` *retains scrollback*, so `getLine(0)` is the oldest
   scrolled-off line, not the top of what's on screen. The adapter reads `getLine(baseY + i)`,
   offsetting by `buffer.active.baseY`. That honours `snapshot()`'s documented contract ("the current
   **visible-screen** contents") and aligns it with vt10x's fixed-size, no-scrollback screen — the
   model the corpus ground truth was authored against.
3. **The real-harness `expected.txt` stays hand-curated; `corpus-fidelity` uses calibrated
   ceilings.** `corpus-fidelity.test.ts` holds each scenario to ~2× its observed
   `normalizedDistance` (capped at 0.999), derived from a real bench run
   (`npx tsx test/corpus/tools/screenbench/main.ts --corpus test/corpus --format json`).
   `synth-fidelity.test.ts` asserts an exact match on the synthetic scenarios.

## Alternatives

- **A second TS emulator.** None exists yet, so no fidelity/accuracy comparison was run. If one is
  added later, it registers under its own key in the same `emulators.ts` registry (mirroring how the
  Go registry holds both `vt10x` and `charm-x-vt`) and is run through the same real-corpus
  crash-survival check before being trusted for fidelity numbers.
- **Re-bootstrapping `expected.txt` via `--write-expected`** — rejected. The committed
  `expected.txt` for the real-harness scenarios is hand-curated *final assistant text*
  (`scenario.ts`'s header calls it "ground-truth final assistant text") — e.g.
  `codex/short-reply/expected.txt` is literally `Hi`. Re-bootstrapping would zero every distance,
  but only by overwriting curated ground truth with a full-screen xterm dump, turning the gate into
  a tautology (xterm vs its own output) and mutating committed corpus files owned by another task.

## Evidence

`@xterm/headless` was evaluated against three checks, mirroring the Go ADR's discipline of trusting
real messy PTY output over assumed feature richness:

1. **Plain-Node instantiation.** `new Terminal({ cols, rows, allowProposedApi: true })` from
   `@xterm/headless` runs under `tsx`/Node with no DOM shims, polyfills, or browser environment —
   confirmed by instantiating and writing to it in a bare Node script.
2. **Write path is asynchronous, not synchronous.** `Terminal#write(data, callback)` queues the
   chunk and parses it off the write buffer; the callback does **not** fire synchronously within
   the same tick as the `write()` call, and a `snapshot()` taken immediately after calling
   `write()` (without waiting for the callback) observes stale/unparsed screen state. Confirmed
   empirically: writing a string and reading the buffer in the very next line still showed the
   pre-write (blank) screen, while reading after awaiting the write callback showed the expected
   text.
3. **Survives real recordings.** The candidate adapter was fed six real corpus recordings directly
   via `fs.readFileSync` — `codex/short-reply`, `codex/long-markdown`, `codex/code-block`,
   `claude-code/interrupted-mid-reply`, `claude-code/multi-turn`, `claude-code/tool-call` (7.5 KB
   to 85 KB each, at their recorded 120×40 size) — with no crash and a non-empty snapshot in every
   case. This matches vt10x's outcome on the Go side; no analog of `charm-x-vt`'s reproducible
   panic was observed.

The viewport rule (decision 2) came from wiring `bench.ts`/`main.ts` — the port of
`internal/screenbench/cmd/screenbench/main.go` — and the two fidelity gates, which did the fidelity
comparison the crash-survival check never had. For scenarios that overflow the screen
(`test/corpus/synth/scrollback-overflow`) the original `getLine(0 … rows-1)` read snapshotted the
wrong rows: the top-of-history `line 1 … line 23` instead of the visible `line 8 … line 30`, giving
`normalizedDistance` 0.287 where an exact 0 was expected. With the `baseY` offset all six
`test/corpus/synth/*` scenarios reproduce exactly (NDist 0); the adapter's public surface is
unchanged and the crash-survival test still passes.

## Consequences

- **Stable surface.** `test/corpus/tools/screenbench/emulators.ts` exposes `registry`
  (`Map<string, Factory>`), `register(name, factory)`, and `names()`; downstream bench code
  (`bench.ts`/`main.ts`) looks up the `"xterm"` factory by name from there.
- **Every caller awaits the write.** `emulators.ts`'s `BenchEmulator.write()` returns a
  `Promise<void>` that resolves only once xterm's own callback fires. `snapshot()` and `cursor()`
  must only be called after `await`ing it — any future bench/scenario code must do the same or reads
  will race the parser.
- **Real-corpus ceilings are smoke-level for terse replies.**
  `normalizedDistance(fullScreenSnapshot, terseCuratedText)` is near the metric's maximum by
  construction (it divides by `max(len(snapshot), len(expected))`, and the snapshot is a whole TUI
  screen), so observed real-corpus distances run ~0.36–0.999. The discriminating fidelity signal
  lives in `synth-fidelity.test.ts` (exact match) plus the longer-expected scenarios whose ceilings
  sit under the cap (`codex/code-block` 0.718, `codex/long-markdown` 0.982).
- **Scenarios without `expected.txt` are skipped from scoring**
  (`codex/{interrupted-mid-reply,prompt-ready,update-notice}`), via the same
  `expected.trim() !== ""` guard `bench.ts` uses, not by name.
- **Timing figures are not comparable with Go's.** Because the xterm write path is asynchronous,
  the bench's `--settle`, throughput, and alloc figures are relative/informational only and are
  **not** comparable to the Go bench's synchronous-vt10x numbers. `main.ts` prints this on stderr on
  every run and the table/markdown reports repeat it in-band.
