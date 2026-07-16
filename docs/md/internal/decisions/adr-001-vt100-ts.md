# ADR-001 (TS addendum): vt100 emulator selection for `test/corpus/tools/screenbench`

**Status:** Accepted — `@xterm/headless` chosen (2026-07-16)

## Context

[ADR-001](adr-001-vt100.md) picked `vt10x` for the Go-side bake-off (`internal/screenbench/`) and
for `pkg/screen`. That port is now growing a TS-side sibling under
`test/corpus/tools/screenbench/*.ts`, reusing the same recorded corpus
(`test/corpus/{codex,claude-code,synth}/**/bytes.raw`). This addendum covers only the emulator
choice for the TS tooling's `emulators.ts` adapter registry; it does not redo the Go decision or
touch `adr-001-vt100.md`.

No TS terminal-emulator library had been vetted in this repo before this task — zero prior
`@xterm` (or equivalent) references existed anywhere in the tree.

## Decision

**The TS bench wraps `@xterm/headless` (the headless build of xterm.js), registered as `"xterm"`
in `test/corpus/tools/screenbench/emulators.ts`.**

It was evaluated directly against three checks, mirroring the Go ADR's own bake-off discipline of
trusting real messy PTY output over assumed feature richness:

1. **Plain-Node instantiation.** `new Terminal({ cols, rows, allowProposedApi: true })` from
   `@xterm/headless` runs under `tsx`/Node with no DOM shims, polyfills, or browser environment —
   confirmed by instantiating and writing to it in a bare Node script.
2. **Write path is asynchronous, not synchronous.** `Terminal#write(data, callback)` queues the
   chunk and parses it off the write buffer; the callback does **not** fire synchronously within
   the same tick as the `write()` call, and a `snapshot()` taken immediately after calling
   `write()` (without waiting for the callback) observes stale/unparsed screen state. This was
   confirmed empirically: writing a string and reading the buffer in the very next line still
   showed the pre-write (blank) screen, while reading after awaiting the write callback showed the
   expected text. Because of this, `emulators.ts`'s `BenchEmulator.write()` returns a `Promise<void>`
   that resolves only once xterm's own callback fires, and the adapter's `snapshot()` must only be
   called after `await`ing that promise — any future bench/scenario code must do the same or reads
   will race the parser.
3. **Survives real recordings.** The candidate adapter was fed six real corpus recordings directly
   via `fs.readFileSync` — `codex/short-reply`, `codex/long-markdown`, `codex/code-block`,
   `claude-code/interrupted-mid-reply`, `claude-code/multi-turn`, `claude-code/tool-call` (7.5 KB
   to 85 KB each, at their recorded 120×40 size) — with no crash and a non-empty snapshot in every
   case. This matches vt10x's outcome on the Go side; no analog of `charm-x-vt`'s reproducible
   panic was observed.

No fidelity/accuracy comparison was run (no second TS candidate exists yet, and `metrics.ts` /
`scenario.ts` — which would supply `expected.txt` comparison — are separate, downstream work). If
a second candidate is added later, it should register under its own key in the same
`emulators.ts` registry (mirroring how the Go registry holds both `vt10x` and `charm-x-vt`) and be
run through the same real-corpus crash-survival check before being trusted for fidelity numbers.

## Consequences

`test/corpus/tools/screenbench/emulators.ts` exposes `registry` (`Map<string, Factory>`),
`register(name, factory)`, and `names()` as its stable public surface; downstream bench code
(`bench.ts`/`main.ts`, added separately) looks up the `"xterm"` factory by name from there. Any
caller of the `xterm` adapter's `write()` must `await` it before calling `snapshot()` or
`cursor()`, per point 2 above.

## Addendum — bench/fidelity-gate wiring (HARNESS-WRAPPER-21)

Wiring `bench.ts`/`main.ts` (the port of `internal/screenbench/cmd/screenbench/main.go`) and the
two fidelity gate tests (`synth-fidelity.test.ts`, `corpus-fidelity.test.ts`) surfaced two things
the emulator-adapter task's crash-survival check (which never did a fidelity comparison — see the
Decision section's closing paragraph) could not have caught.

**1. `snapshot()` must read the visible viewport, not the top of the scrollback buffer.** The
original adapter read `buffer.active.getLine(0 … rows-1)`. `@xterm/headless`'s `buffer.active`
*retains scrollback*, so `getLine(0)` is the oldest scrolled-off line, not the top of what's on
screen — contradicting `snapshot()`'s own documented contract ("the current **visible-screen**
contents"). For scenarios that overflow the screen (`test/corpus/synth/scrollback-overflow`) this
snapshotted the wrong rows: the top-of-history `line 1 … line 23` instead of the visible
`line 8 … line 30`, giving `normalizedDistance` 0.287 where an exact 0 was expected. Fixed by
offsetting the read by `buffer.active.baseY` (`getLine(baseY + i)`), which also aligns the snapshot
with vt10x's fixed-size, no-scrollback screen — the model the corpus ground truth was authored
against. This is a one-line correctness fix to honor the method's existing contract; it does not
change the adapter's public surface and leaves the crash-survival test passing. All six
`test/corpus/synth/*` scenarios now reproduce exactly (NDist 0).

**2. codex/claude-code `expected.txt` was NOT re-bootstrapped; `corpus-fidelity` uses calibrated
ceilings instead.** The committed `expected.txt` for the real-harness scenarios is hand-curated
*final assistant text* (ADR-001 line 28; `scenario.ts`'s header calls it "ground-truth final
assistant text") — e.g. `codex/short-reply/expected.txt` is literally `Hi`. It is **not** a
full-screen vt10x snapshot, despite ADR-001's bake-off-table paragraph describing it that way (that
paragraph predates the 0.142.2 corpus re-bake; the current recordings differ in byte count from the
0.130.0 numbers in that table). `normalizedDistance(fullScreenSnapshot, terseCuratedText)` is
near the metric's maximum by construction for terse replies (it divides by
`max(len(snapshot), len(expected))`, and the snapshot is a whole TUI screen), so observed real-corpus
distances run ~0.36–0.999. Re-bootstrapping via `--write-expected` would zero these — but only by
overwriting curated ground truth with a full-screen xterm dump, turning the gate into a tautology
(xterm vs its own output) and mutating committed corpus files owned by another task. We therefore
**keep the curated ground truth** and calibrate `corpus-fidelity.test.ts`'s per-scenario ceilings to
~2× the observed `normalizedDistance` (capped at 0.999), derived from a real bench run
(`npx tsx test/corpus/tools/screenbench/main.ts --corpus test/corpus --format json`). The
discriminating fidelity signal lives in `synth-fidelity.test.ts` (exact match) plus the
longer-expected scenarios whose ceilings sit under the cap (`codex/code-block` 0.718,
`codex/long-markdown` 0.982); the terse-expected scenarios carry near-cap, smoke-level ceilings.
Scenarios without `expected.txt` (`codex/{interrupted-mid-reply,prompt-ready,update-notice}`) are
skipped from scoring via the same `expected.trim() !== ""` guard `bench.ts` uses, not by name.

**Timing caveat (restated for the bench).** Because the xterm write path is asynchronous (point 2
of the Decision), the bench's `--settle`, throughput, and alloc figures are relative/informational
only and are **not** comparable to the Go bench's synchronous-vt10x numbers. `main.ts` prints this
on stderr on every run and the table/markdown reports repeat it in-band.
