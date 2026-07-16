// Regenerates the synth corpus with the TS port of
// internal/screenbench/cmd/screenbench-synth/main.go (../synth-corpus.ts)
// into a scratch directory, then byte-compares bytes.raw/expected.txt
// against the committed test/corpus/synth/<name>/ directories. meta.json is
// deliberately excluded from the comparison: recordedAt is stamped with the
// current time on every run and can never be byte-identical across runs.

import { spawnSync } from "node:child_process"
import { promises as fs } from "node:fs"
import * as os from "node:os"
import * as path from "node:path"
import { fileURLToPath } from "node:url"

import { afterAll, beforeAll, describe, expect, test } from "vitest"

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, "../../../..")
const committedSynthRoot = path.join(repoRoot, "test", "corpus", "synth")
const toolPath = path.join(repoRoot, "test", "corpus", "tools", "synth-corpus.ts")

const SCENARIOS = [
  "short-reply",
  "long-markdown",
  "code-block",
  "interrupt-mid-stream",
  "alt-screen-toggle",
  "scrollback-overflow",
]

let scratchRoot: string
let scratchOut: string

beforeAll(async () => {
  scratchRoot = await fs.mkdtemp(path.join(os.tmpdir(), "screenbench-synth-test-"))
  scratchOut = path.join(scratchRoot, "corpus")

  const result = spawnSync("npx", ["tsx", toolPath, "--out", scratchOut], {
    cwd: repoRoot,
    encoding: "utf8",
  })
  if (result.status !== 0) {
    throw new Error(`synth-corpus.ts failed (status ${result.status}):\n${result.stdout}\n${result.stderr}`)
  }
}, 60_000)

afterAll(async () => {
  if (scratchRoot) {
    await fs.rm(scratchRoot, { recursive: true, force: true })
  }
})

describe("synth-corpus.ts regeneration", () => {
  for (const name of SCENARIOS) {
    test(`${name}: bytes.raw is byte-identical to the committed corpus`, async () => {
      const got = await fs.readFile(path.join(scratchOut, "synth", name, "bytes.raw"))
      const want = await fs.readFile(path.join(committedSynthRoot, name, "bytes.raw"))
      expect(got.equals(want)).toBe(true)
    })

    test(`${name}: expected.txt is byte-identical to the committed corpus`, async () => {
      const got = await fs.readFile(path.join(scratchOut, "synth", name, "expected.txt"))
      const want = await fs.readFile(path.join(committedSynthRoot, name, "expected.txt"))
      expect(got.equals(want)).toBe(true)
    })
  }

  test("meta.json is still written (shape only, not byte-compared)", async () => {
    for (const name of SCENARIOS) {
      const meta = JSON.parse(await fs.readFile(path.join(scratchOut, "synth", name, "meta.json"), "utf8"))
      expect(meta.harness).toBe("synth")
      expect(meta.cols).toBe(80)
      expect(meta.rows).toBe(24)
    }
  })
})
