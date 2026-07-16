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
