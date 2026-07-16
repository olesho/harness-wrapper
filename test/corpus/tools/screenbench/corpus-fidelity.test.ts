// Fidelity gate — real recorded corpus. Unlike the synthetic corpus (which
// must match exactly, see synth-fidelity.test.ts), real harness recordings are
// scored against a calibrated normalizedDistance ceiling per scenario rather
// than requiring an exact match.
//
// Scenarios come from scenario.ts's `discover("test/corpus")`. Any scenario
// without an expected.txt (HasExpected === false — e.g.
// codex/{interrupted-mid-reply,prompt-ready,update-notice}) is skipped from
// scoring using the SAME `expected.trim() !== ""` guard bench.ts uses (NOT a
// by-name special case). test/corpus/pi/ has no meta.json anywhere, so
// `discover()` never surfaces it here — no special handling needed.
//
// CALIBRATION (evidence-based, not guessed). The ceilings below were derived
// from an actual bench run against the committed corpus with the xterm adapter:
//
//   npx tsx test/corpus/tools/screenbench/main.ts --corpus test/corpus --format json
//
// Each ceiling is ~2x the observed normalizedDistance for that scenario,
// capped at 0.999 (the metric saturates at 1.0). Observed values are
// deterministic — the byte streams and the emulator are deterministic and
// normalizedDistance has no timing dependence — so these ceilings are stable,
// not flaky.
//
// WHY THE REAL-CORPUS DISTANCES ARE LARGE (and why expected.txt was NOT
// re-bootstrapped): the committed codex/claude-code expected.txt files are
// hand-curated *final assistant text* (ADR-001 line 28; scenario.ts's header:
// "ground-truth final assistant text") — e.g. codex/short-reply's expected.txt
// is literally "Hi". They are NOT full-screen vt10x snapshots, despite the
// ADR-001 bake-off-table paragraph (and this task's premise) describing them
// that way. `normalizedDistance(fullScreenSnapshot, terseCuratedText)` is
// therefore near the metric's maximum by construction for terse replies (it
// divides by max(len(snapshot), len(expected)), and the snapshot is a whole
// TUI screen). Re-bootstrapping expected.txt via `--write-expected` would zero
// these distances, but only by overwriting curated ground truth with a
// full-screen xterm dump and turning this gate into a tautology (xterm vs its
// own output) — and by mutating committed corpus files owned by another task.
// We keep the curated ground truth and calibrate ceilings instead. See the TS
// addendum in docs/md/internal/decisions/adr-001-vt100-ts.md for the full note.
//
// The meaningful fidelity signal thus lives in (a) synth-fidelity.test.ts
// (exact match) and (b) the longer-expected scenarios below whose ceilings sit
// clearly under the cap (codex/code-block 0.718, codex/long-markdown 0.982);
// the terse-expected scenarios carry near-cap, smoke-level ceilings that only
// catch catastrophic divergence.

import * as path from "node:path"

import { describe, expect, it } from "vitest"

import { registry } from "./emulators.ts"
import { normalizedDistance } from "./metrics.ts"
import { discover } from "./scenario.ts"

const EMULATOR = "xterm"
const CORPUS = "test/corpus"

// key = scenario path relative to the corpus root (unambiguous even for nested
// adversarial/* scenarios). ceiling = ~2x observed normalizedDistance, capped
// at 0.999. See the calibration note above.
const THRESHOLDS: Record<string, number> = {
  // real recorded corpus (observed -> ~2x, cap 0.999)
  "claude-code/interrupted-mid-reply": 0.999, // observed 0.838
  "claude-code/multi-turn": 0.999, // observed 0.635
  "claude-code/adversarial/thinking-line-mid-reply": 0.999, // observed 0.995
  "claude-code/tool-call": 0.999, // observed 0.790
  "codex/code-block": 0.718, // observed 0.359
  "codex/long-markdown": 0.982, // observed 0.491
  "codex/multi-turn": 0.999, // observed 0.998
  "codex/adversarial/partial-stream-no-footer": 0.999, // observed 0.999
  "codex/adversarial/prefix-only-marker": 0.999, // observed 0.996
  "codex/short-reply": 0.999, // observed 0.997
  "codex/tool-call": 0.999, // observed 0.898
  // synthetic corpus reproduces exactly (observed 0.000)
  "synth/short-reply": 0.001,
  "synth/long-markdown": 0.001,
  "synth/code-block": 0.001,
  "synth/interrupt-mid-stream": 0.001,
  "synth/alt-screen-toggle": 0.001,
  "synth/scrollback-overflow": 0.001,
}

// Top-level await: discover synchronously registers per-scenario tests below.
const scenarios = await discover(CORPUS)
const scored = scenarios.filter((sc) => sc.expected.trim() !== "")
const skipped = scenarios.filter((sc) => sc.expected.trim() === "")
const relKey = (p: string): string => path.relative(CORPUS, p).split(path.sep).join("/")

describe("corpus fidelity (calibrated normalizedDistance ceilings)", () => {
  const factory = registry.get(EMULATOR)!

  it("discovered the real recorded scenarios", () => {
    expect(scored.length).toBeGreaterThanOrEqual(8)
  })

  it("skips scenarios without expected.txt (no crash, no false score)", () => {
    // The codex scenarios lacking expected.txt must be present but excluded
    // from scoring — guarded purely by the empty-expected check, not by name.
    const keys = skipped.map((sc) => relKey(sc.path))
    expect(keys).toContain("codex/interrupted-mid-reply")
    expect(keys).toContain("codex/prompt-ready")
    expect(keys).toContain("codex/update-notice")
    // None of the skipped scenarios is scored.
    for (const sc of skipped) {
      expect(sc.expected.trim()).toBe("")
    }
  })

  for (const sc of scored) {
    const key = relKey(sc.path)
    it(`${key}: normalizedDistance within calibrated ceiling`, async () => {
      const threshold = THRESHOLDS[key]
      expect(
        threshold,
        `no calibrated threshold for "${key}"; run \`npx tsx test/corpus/tools/screenbench/main.ts --corpus test/corpus --format json\` and add one (~2x observed)`,
      ).toBeDefined()

      const emu = factory(sc.meta.cols, sc.meta.rows)
      await emu.write(sc.bytes)
      const snap = await emu.snapshot()

      const nd = normalizedDistance(snap, sc.expected)
      expect(
        nd,
        `${key}: normalizedDistance ${nd.toFixed(4)} exceeds calibrated ceiling ${threshold}`,
      ).toBeLessThanOrEqual(threshold)
    })
  }
})
