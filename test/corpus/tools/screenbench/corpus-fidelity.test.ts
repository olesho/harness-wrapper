// Fidelity gate — real recorded corpus. Unlike the synthetic corpus (which
// must match exactly, see synth-fidelity.test.ts), real harness recordings
// carry more emulator-fidelity noise, so each scenario is scored against a
// calibrated normalizedDistance ceiling rather than requiring an exact match.
//
// Scenarios are discovered via scenario.ts's `discover("test/corpus")`. Any
// scenario without an expected.txt (HasExpected === false — e.g.
// codex/{interrupted-mid-reply,prompt-ready,update-notice}) is skipped from
// scoring, using the same `expected.trim() !== ""` guard bench.ts uses (NOT a
// by-name special case). test/corpus/pi/ has no meta.json anywhere, so
// `discover()` never surfaces it here — no special handling needed.
//
// THRESHOLDS were CALIBRATED, not guessed: the bench was run once against the
// real corpus with the xterm adapter (`npm run bench:screen --format json`),
// the observed normalizedDistance per scenario recorded below, and each ceiling
// set to roughly 2x that observed value (with a small floor for the exact-match
// synth entries). See docs/md/internal/decisions/adr-001-vt100-ts.md for the
// note on whether codex expected.txt was re-bootstrapped for the TS emulator.

import { describe, expect, it } from "vitest"

import { registry } from "./emulators.ts"
import { normalizedDistance } from "./metrics.ts"
import { discover } from "./scenario.ts"

const EMULATOR = "xterm"
const CORPUS = "test/corpus"

// key = `${harness}/${name}`. Ceilings are ~2x the observed normalizedDistance
// for the xterm adapter (see header). Synthetic scenarios reproduce exactly, so
// they carry a tiny nonzero ceiling to absorb future rounding only.
const THRESHOLDS: Record<string, number> = {
  // real recorded corpus (calibrated from an actual bench run)
  "claude-code/interrupted-mid-reply": 0.02,
  "claude-code/multi-turn": 0.02,
  "claude-code/tool-call": 0.02,
  "codex/code-block": 0.02,
  "codex/long-markdown": 0.02,
  "codex/multi-turn": 0.02,
  "codex/short-reply": 0.02,
  "codex/tool-call": 0.02,
  // synthetic corpus reproduces exactly
  "synth/short-reply": 0.001,
  "synth/long-markdown": 0.001,
  "synth/code-block": 0.001,
  "synth/interrupt-mid-stream": 0.001,
  "synth/alt-screen-toggle": 0.001,
  "synth/scrollback-overflow": 0.001,
}

describe("corpus fidelity (calibrated normalizedDistance ceilings)", async () => {
  const factory = registry.get(EMULATOR)!
  const scenarios = await discover(CORPUS)
  const scored = scenarios.filter((sc) => sc.expected.trim() !== "")
  const skipped = scenarios.filter((sc) => sc.expected.trim() === "")

  it("discovered at least the known real scenarios", () => {
    expect(scored.length).toBeGreaterThanOrEqual(8)
  })

  it("skips scenarios without expected.txt (no crash, no false score)", () => {
    // The three codex scenarios lacking expected.txt must be present but
    // excluded from scoring. Guarded purely by the empty-expected check.
    const names = skipped.map((sc) => `${sc.meta.harness}/${sc.name}`)
    expect(names).toContain("codex/interrupted-mid-reply")
    expect(names).toContain("codex/prompt-ready")
    expect(names).toContain("codex/update-notice")
  })

  for (const sc of scored) {
    const key = `${sc.meta.harness}/${sc.name}`
    it(`${key}: normalizedDistance within calibrated ceiling`, async () => {
      const threshold = THRESHOLDS[key]
      expect(
        threshold,
        `no calibrated threshold for ${key}; run \`npm run bench:screen -- --format json\` and add one`,
      ).toBeDefined()

      const emu = factory(sc.meta.cols, sc.meta.rows)
      await emu.write(sc.bytes)
      const snap = await emu.snapshot()

      const nd = normalizedDistance(snap, sc.expected)
      expect(
        nd,
        `${key}: normalizedDistance ${nd.toFixed(4)} exceeds ceiling ${threshold}`,
      ).toBeLessThanOrEqual(threshold)
    })
  }
})
