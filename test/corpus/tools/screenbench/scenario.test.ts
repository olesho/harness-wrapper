// Port of internal/screenbench/scenario/scenario_test.go's test coverage for
// the TS port at ../screenbench/scenario.ts.

import { promises as fs } from "node:fs"
import * as os from "node:os"
import * as path from "node:path"
import { fileURLToPath } from "node:url"

import { afterEach, describe, expect, test } from "vitest"

import { discover, load, type Meta, writeMeta } from "./scenario.ts"

const here = path.dirname(fileURLToPath(import.meta.url))
const repoRoot = path.resolve(here, "../../../..")
const corpusRoot = path.join(repoRoot, "test", "corpus")

const tmpDirs: string[] = []

async function tmpDir(): Promise<string> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "screenbench-scenario-test-"))
  tmpDirs.push(dir)
  return dir
}

afterEach(async () => {
  while (tmpDirs.length) {
    const dir = tmpDirs.pop()!
    await fs.rm(dir, { recursive: true, force: true })
  }
})

describe("discover", () => {
  test("returns an empty array for test/corpus/pi without throwing", async () => {
    const found = await discover(path.join(corpusRoot, "pi"))
    expect(found).toEqual([])
  })

  test("returns scenarios sorted by harness then name", async () => {
    const found = await discover(corpusRoot)
    expect(found.length).toBeGreaterThan(0)

    const sorted = [...found].sort((a, b) => {
      if (a.meta.harness !== b.meta.harness) {
        return a.meta.harness < b.meta.harness ? -1 : 1
      }
      return a.name < b.name ? -1 : a.name > b.name ? 1 : 0
    })
    expect(found.map((s) => [s.meta.harness, s.name])).toEqual(sorted.map((s) => [s.meta.harness, s.name]))

    // Sanity: harnesses we know are recorded under test/corpus actually show up.
    const harnesses = new Set(found.map((s) => s.meta.harness))
    expect(harnesses.has("codex")).toBe(true)
    expect(harnesses.has("claude-code")).toBe(true)
    expect(harnesses.has("synth")).toBe(true)
  })
})

describe("load", () => {
  test("missing expected.txt loads as an empty string, not a throw", async () => {
    const dir = path.join(corpusRoot, "codex", "prompt-ready")
    await expect(fs.access(path.join(dir, "expected.txt"))).rejects.toThrow()

    const scenario = await load(dir)
    expect(scenario.expected).toBe("")
    expect(scenario.bytes.length).toBeGreaterThan(0)
    expect(scenario.name).toBe("prompt-ready")
  })

  test("missing or zero cols/rows default to 120/40", async () => {
    const dir = await tmpDir()
    await fs.writeFile(path.join(dir, "bytes.raw"), Buffer.from("hi"))

    await fs.writeFile(
      path.join(dir, "meta.json"),
      JSON.stringify({ harness: "synth", binary_version: "x", recorded_at: "2026-01-01T00:00:00Z" }),
    )
    let scenario = await load(dir)
    expect(scenario.meta.cols).toBe(120)
    expect(scenario.meta.rows).toBe(40)

    await fs.writeFile(
      path.join(dir, "meta.json"),
      JSON.stringify({ harness: "synth", binary_version: "x", recorded_at: "2026-01-01T00:00:00Z", cols: 0, rows: 0 }),
    )
    scenario = await load(dir)
    expect(scenario.meta.cols).toBe(120)
    expect(scenario.meta.rows).toBe(40)
  })

  test("explicit non-zero cols/rows are preserved", async () => {
    const dir = await tmpDir()
    await fs.writeFile(path.join(dir, "bytes.raw"), Buffer.from("hi"))
    await fs.writeFile(
      path.join(dir, "meta.json"),
      JSON.stringify({ harness: "synth", binary_version: "x", recorded_at: "2026-01-01T00:00:00Z", cols: 80, rows: 24 }),
    )
    const scenario = await load(dir)
    expect(scenario.meta.cols).toBe(80)
    expect(scenario.meta.rows).toBe(24)
  })

  test("missing meta.json propagates a clear error", async () => {
    const dir = await tmpDir()
    await fs.writeFile(path.join(dir, "bytes.raw"), Buffer.from("hi"))
    await expect(load(dir)).rejects.toThrow(/meta\.json/)
  })

  test("missing bytes.raw propagates a clear error", async () => {
    const dir = await tmpDir()
    await fs.writeFile(
      path.join(dir, "meta.json"),
      JSON.stringify({ harness: "synth", binary_version: "x", recorded_at: "2026-01-01T00:00:00Z" }),
    )
    await expect(load(dir)).rejects.toThrow(/bytes\.raw/)
  })
})

describe("writeMeta", () => {
  test("round-trips through load with the on-disk (snake_case) JSON shape", async () => {
    const dir = await tmpDir()
    const meta: Meta = {
      harness: "synth",
      binaryVersion: "screenbench-synth",
      recordedAt: "2026-01-01T00:00:00Z",
      cols: 80,
      rows: 24,
      notes: "a note",
    }
    await writeMeta(dir, meta)
    await fs.writeFile(path.join(dir, "bytes.raw"), Buffer.from("hi"))

    const onDisk = JSON.parse(await fs.readFile(path.join(dir, "meta.json"), "utf8"))
    expect(onDisk).toEqual({
      harness: "synth",
      binary_version: "screenbench-synth",
      recorded_at: "2026-01-01T00:00:00Z",
      cols: 80,
      rows: 24,
      notes: "a note",
    })

    const scenario = await load(dir)
    expect(scenario.meta).toEqual(meta)
  })

  test("creates the directory if it doesn't exist", async () => {
    const base = await tmpDir()
    const dir = path.join(base, "nested", "dir")
    await writeMeta(dir, {
      harness: "synth",
      binaryVersion: "x",
      recordedAt: "2026-01-01T00:00:00Z",
      cols: 80,
      rows: 24,
    })
    const stat = await fs.stat(path.join(dir, "meta.json"))
    expect(stat.isFile()).toBe(true)
  })
})
