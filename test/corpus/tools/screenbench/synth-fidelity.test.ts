// Fidelity gate — synthetic corpus. For each committed test/corpus/synth/*
// scenario (deterministic byte streams with hand-verified expected text),
// replay the bytes through the registered emulator and assert the normalized
// snapshot is byte-identical to the normalized expected text. These are the
// highest-confidence fidelity checks: the synthetic byte streams are
// hand-authored and the emulator must reproduce them exactly.

import * as path from "node:path"
import { fileURLToPath } from "node:url"

import { describe, expect, it } from "vitest"

import { registry } from "./emulators.ts"
import { normalize } from "./metrics.ts"
import { load } from "./scenario.ts"

const here = path.dirname(fileURLToPath(import.meta.url))
const synthRoot = path.resolve(here, "../../synth")

const SCENARIOS = [
  "short-reply",
  "long-markdown",
  "code-block",
  "interrupt-mid-stream",
  "alt-screen-toggle",
  "scrollback-overflow",
]

const EMULATOR = "xterm"

describe("synth fidelity (exact match)", () => {
  const factory = registry.get(EMULATOR)!

  it.each(SCENARIOS)(
    "synth/%s: normalized snapshot === normalized expected",
    async (name) => {
      const sc = await load(path.join(synthRoot, name))
      expect(sc.expected.trim(), `${name} must have expected.txt`).not.toBe("")

      const emu = factory(sc.meta.cols, sc.meta.rows)
      await emu.write(sc.bytes)
      const snap = await emu.snapshot()

      expect(normalize(snap)).toBe(normalize(sc.expected))
    },
  )
})
