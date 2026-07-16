// Port of internal/screenbench/scenario/scenario.go: loads recorded bake-off
// scenarios from disk.
//
// A scenario is a directory laid out as:
//
//   scenarios/<harness>/<name>/
//       bytes.raw          required: raw PTY byte stream captured from the harness
//       meta.json          required: harness, recorded_at, terminal dims, binary version
//       expected.txt       optional: ground-truth final assistant text
//       transcript.jsonl   optional: copy of the harness's own session log
//
// The bench replays bytes.raw through each emulator and compares the
// resulting screen snapshot against expected.txt.

import { promises as fs } from "node:fs"
import * as path from "node:path"

/** Parsed contents of meta.json. */
export interface Meta {
  harness: string // e.g. "codex", "claude-code"
  binaryVersion: string // e.g. "codex 0.42.1"
  recordedAt: string
  cols: number
  rows: number
  notes?: string
}

/** On-disk JSON shape of meta.json (Go's `encoding/json` field names). */
interface RawMeta {
  harness: string
  binary_version: string
  recorded_at: string
  cols?: number
  rows?: number
  notes?: string
}

/** One loaded corpus entry. */
export interface Scenario {
  name: string
  path: string
  meta: Meta
  bytes: Buffer
  expected: string // contents of expected.txt; empty if missing
}

function wrapError(label: string, err: unknown): Error {
  const message = err instanceof Error ? err.message : String(err)
  return new Error(`${label}: ${message}`, { cause: err })
}

function isNotFound(err: unknown): boolean {
  return typeof err === "object" && err !== null && (err as NodeJS.ErrnoException).code === "ENOENT"
}

/** Loads a single scenario directory. */
export async function load(dir: string): Promise<Scenario> {
  let metaBytes: Buffer
  try {
    metaBytes = await fs.readFile(path.join(dir, "meta.json"))
  } catch (err) {
    throw wrapError("read meta.json", err)
  }

  let raw: RawMeta
  try {
    raw = JSON.parse(metaBytes.toString("utf8"))
  } catch (err) {
    throw wrapError("parse meta.json", err)
  }

  const meta: Meta = {
    harness: raw.harness,
    binaryVersion: raw.binary_version,
    recordedAt: raw.recorded_at,
    cols: raw.cols || 120,
    rows: raw.rows || 40,
    notes: raw.notes,
  }

  let rawBytes: Buffer
  try {
    rawBytes = await fs.readFile(path.join(dir, "bytes.raw"))
  } catch (err) {
    throw wrapError("read bytes.raw", err)
  }

  let expected = ""
  try {
    expected = (await fs.readFile(path.join(dir, "expected.txt"))).toString("utf8")
  } catch (err) {
    if (!isNotFound(err)) {
      throw wrapError("read expected.txt", err)
    }
  }

  return {
    name: path.basename(dir),
    path: dir,
    meta,
    bytes: rawBytes,
    expected,
  }
}

async function walk(dir: string, acc: string[]): Promise<void> {
  let entries: import("node:fs").Dirent[]
  try {
    entries = await fs.readdir(dir, { withFileTypes: true })
  } catch (err) {
    if (isNotFound(err)) {
      return
    }
    throw err
  }
  acc.push(dir)
  for (const entry of entries) {
    if (entry.isDirectory()) {
      await walk(path.join(dir, entry.name), acc)
    }
  }
}

/**
 * Walks root and returns every scenario directory found. A directory
 * qualifies as a scenario iff it directly contains a meta.json file.
 */
export async function discover(root: string): Promise<Scenario[]> {
  const dirs: string[] = []
  await walk(root, dirs)

  const out: Scenario[] = []
  for (const dir of dirs) {
    try {
      await fs.stat(path.join(dir, "meta.json"))
    } catch {
      continue
    }
    try {
      out.push(await load(dir))
    } catch (err) {
      throw wrapError(`load ${dir}`, err)
    }
  }

  out.sort((a, b) => {
    if (a.meta.harness !== b.meta.harness) {
      return a.meta.harness < b.meta.harness ? -1 : 1
    }
    if (a.name !== b.name) {
      return a.name < b.name ? -1 : 1
    }
    return 0
  })

  return out
}

/** Convenience used by the recorder to emit meta.json. */
export async function writeMeta(dir: string, meta: Meta): Promise<void> {
  await fs.mkdir(dir, { recursive: true })
  const raw: RawMeta = {
    harness: meta.harness,
    binary_version: meta.binaryVersion,
    recorded_at: meta.recordedAt,
    cols: meta.cols,
    rows: meta.rows,
  }
  if (meta.notes) {
    raw.notes = meta.notes
  }
  const json = JSON.stringify(raw, null, 2)
  await fs.writeFile(path.join(dir, "meta.json"), json)
}
