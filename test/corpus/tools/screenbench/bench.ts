// Core replay/scoring logic for the TS screenbench bake-off, ported from
// internal/screenbench/cmd/screenbench/main.go (the `runOne`,
// `measureStability`, and `emit*` functions plus the top-level orchestration).
// The thin CLI wrapper lives in main.ts.
//
// IMPORTANT — timing caveat: the Go bench replays bytes through vt10x, whose
// write/parse path is synchronous, so its `duration`, `throughput`, and
// `settle` numbers reflect real parse cost. The TS emulator adapter chosen for
// this port (`@xterm/headless`, see docs/md/internal/decisions/adr-001-vt100-ts.md)
// parses asynchronously off an internal write buffer; `write()` returns a
// Promise that only resolves once xterm's own callback fires. As a result the
// settle/throughput/alloc figures produced here are RELATIVE / INFORMATIONAL
// only and are NOT apples-to-apples comparable with the Go bench's own numbers.
// This is stated again in the report headers surfaced to users.

import { promises as fs } from "node:fs"
import * as path from "node:path"
import { performance } from "node:perf_hooks"

import { registry, names } from "./emulators.ts"
import type { BenchEmulator } from "./emulators.ts"
import {
  normalize,
  exactMatch,
  levenshtein,
  normalizedDistance,
} from "./metrics.ts"
import { discover } from "./scenario.ts"
import type { Scenario } from "./scenario.ts"

/**
 * One replay result for a (scenario, emulator) pair. Mirrors Go's `result`
 * struct field-for-field; durations are carried in milliseconds (Go used
 * time.Duration nanoseconds) and re-derived to nanoseconds only in JSON output.
 */
export interface BenchResult {
  scenario: string
  harness: string
  emulator: string
  hasExpected: boolean
  exact: boolean
  distance: number
  normDistance: number
  bytes: number
  extractRunes: number
  expectRunes: number
  durationMs: number
  throughput: number // bytes/sec
  stableAfterMs: number
  allocMB: number
  snapshot: string
  err: string // empty when no error
}

/** One-line informational caveat surfaced with every report/format. */
export const TIMING_NOTE =
  "settle/throughput/alloc are informational for the async @xterm/headless write path; NOT comparable to the Go bench's synchronous vt10x numbers."

const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms))

function heapUsed(): number {
  return process.memoryUsage().heapUsed
}

/**
 * Replays one scenario through one emulator and scores it. Mirrors Go's
 * `runOne` (main.go:113-164), including the `HasExpected` guard that skips
 * distance/exactness scoring for scenarios without an expected.txt.
 */
export async function runOne(
  sc: Scenario,
  emuName: string,
  settleMs: number,
): Promise<BenchResult> {
  const r: BenchResult = {
    scenario: sc.path,
    harness: sc.meta.harness,
    emulator: emuName,
    hasExpected: sc.expected.trim() !== "",
    exact: false,
    distance: 0,
    normDistance: 0,
    bytes: sc.bytes.length,
    extractRunes: 0,
    expectRunes: 0,
    durationMs: 0,
    throughput: 0,
    stableAfterMs: 0,
    allocMB: 0,
    snapshot: "",
    err: "",
  }

  const factory = registry.get(emuName)
  if (!factory) {
    r.err = `emulator "${emuName}" not registered`
    return r
  }

  try {
    const emu = factory(sc.meta.cols, sc.meta.rows)

    // Best-effort allocation delta. Go reads runtime.MemStats.TotalAlloc after
    // a forced GC; in Node we can only sample heapUsed (and force GC only when
    // the process was started with --expose-gc). This is noisy and optional.
    const gc = (globalThis as { gc?: () => void }).gc
    if (gc) gc()
    const m0 = heapUsed()

    const start = performance.now()
    await emu.write(sc.bytes)
    r.durationMs = performance.now() - start

    r.stableAfterMs = await measureStability(emu, settleMs)

    const m1 = heapUsed()
    r.allocMB = (m1 - m0) / (1024 * 1024)

    if (r.durationMs > 0) {
      r.throughput = sc.bytes.length / (r.durationMs / 1000)
    }

    const snap = await emu.snapshot()
    r.snapshot = snap
    r.extractRunes = Array.from(normalize(snap)).length

    if (r.hasExpected) {
      r.expectRunes = Array.from(normalize(sc.expected)).length
      r.exact = exactMatch(snap, sc.expected)
      r.distance = levenshtein(normalize(snap), normalize(sc.expected))
      r.normDistance = normalizedDistance(snap, sc.expected)
    }
  } catch (err) {
    r.err = err instanceof Error ? err.message : String(err)
  }
  return r
}

/**
 * Polls the emulator over the settle window to detect whether its state is
 * still changing after the byte stream ended. Mirrors Go's `measureStability`
 * (main.go:171-181). For the async xterm adapter the state is already
 * quiescent by the time `write()` resolves, so this normally returns the full
 * settle window — see the module-level timing caveat.
 */
export async function measureStability(
  emu: BenchEmulator,
  settleMs: number,
): Promise<number> {
  const before = await emu.snapshot()
  const start = performance.now()
  const deadline = start + settleMs
  while (performance.now() < deadline) {
    await sleep(20)
    if ((await emu.snapshot()) !== before) {
      return performance.now() - start
    }
  }
  return settleMs
}

/** Options for a full bench run (post-flag-parse). Mirrors main.go's flags. */
export interface BenchOptions {
  corpus: string
  emulator: string // "" = all registered emulators
  scenario: string // substring filter against scenario path/name
  settleMs: number
  writeExpected: boolean
}

/**
 * Discovers scenarios, selects emulators, replays every (scenario, emulator)
 * pair, and optionally bootstraps expected.txt. Mirrors main.go:59-101. Throws
 * on a fatal setup error (unknown corpus / unknown emulator / bad
 * --write-expected combo); the caller maps these to process exit codes.
 */
export async function runBench(opts: BenchOptions): Promise<BenchResult[]> {
  const scenarios = await discover(opts.corpus)
  if (scenarios.length === 0) {
    const e = new Error(`no scenarios found under ${opts.corpus}`) as Error & {
      code: string
    }
    e.code = "NO_SCENARIOS"
    throw e
  }

  let emus = names()
  if (opts.emulator !== "") {
    if (!registry.has(opts.emulator)) {
      const e = new Error(
        `unknown emulator "${opts.emulator}" (have: ${names().join(", ")})`,
      ) as Error & { code: string }
      e.code = "UNKNOWN_EMULATOR"
      throw e
    }
    emus = [opts.emulator]
  }

  if (opts.writeExpected && opts.emulator === "") {
    const e = new Error(
      "--write-expected requires --emulator to pick a source",
    ) as Error & { code: string }
    e.code = "WRITE_EXPECTED_NO_EMULATOR"
    throw e
  }

  const results: BenchResult[] = []
  for (const sc of scenarios) {
    if (
      opts.scenario !== "" &&
      !sc.path.includes(opts.scenario) &&
      !sc.name.includes(opts.scenario)
    ) {
      continue
    }
    for (const name of emus) {
      const r = await runOne(sc, name, opts.settleMs)
      if (opts.writeExpected && r.err === "") {
        const p = path.join(sc.path, "expected.txt")
        const text = normalize(r.snapshot) + "\n"
        try {
          await fs.writeFile(p, text)
          process.stderr.write(
            `wrote ${p} (${normalize(r.snapshot).length} bytes, from ${name})\n`,
          )
        } catch (err) {
          const msg = err instanceof Error ? err.message : String(err)
          process.stderr.write(`screenbench: write ${p}: ${msg}\n`)
        }
      }
      results.push(r)
    }
  }
  return results
}

// ---- output formatters (main.go:185-276) ----

function padEnd(s: string, n: number): string {
  return s.length >= n ? s : s + " ".repeat(n - s.length)
}

function padStart(s: string, n: number): string {
  return s.length >= n ? s : " ".repeat(n - s.length) + s
}

/** Plain fixed-width table (main.go:185-207). */
export function emitTable(rs: BenchResult[]): string {
  const out: string[] = []
  out.push(`# note: ${TIMING_NOTE}`)
  out.push(
    `${padEnd("SCENARIO", 40)} ${padEnd("HARNESS", 12)} ${padEnd("EMULATOR", 12)} ${padStart("BYTES", 8)} ${padStart("DIST", 8)} ${padStart("NDIST", 8)} ${padStart("MB/s", 10)}`,
  )
  for (const r of rs) {
    let dist = "-"
    let ndist = "-"
    if (r.hasExpected) {
      dist = String(r.distance)
      ndist = r.normDistance.toFixed(3)
    }
    const mbs = r.throughput / (1024 * 1024)
    let short = r.scenario
    if (short.length > 40) {
      short = "..." + short.slice(short.length - 37)
    }
    const errStr = r.err ? " ERR=" + r.err : ""
    out.push(
      `${padEnd(short, 40)} ${padEnd(r.harness, 12)} ${padEnd(r.emulator, 12)} ${padStart(String(r.bytes), 8)} ${padStart(dist, 8)} ${padStart(ndist, 8)} ${padStart(mbs.toFixed(2), 10)}${errStr}`,
    )
  }
  return out.join("\n") + "\n"
}

/** ISO-8601/RFC3339 timestamp with seconds precision (no milliseconds). */
function rfc3339(now: Date): string {
  return now.toISOString().replace(/\.\d+Z$/, "Z")
}

/** Per-scenario markdown report (main.go:209-255). */
export function emitMarkdown(
  rs: BenchResult[],
  dumpSnap: boolean,
  now: Date = new Date(),
): string {
  const out: string[] = []
  out.push("# screenbench results")
  out.push("")
  out.push(`Generated: ${rfc3339(now)}`)
  out.push("")
  out.push(`> Note: ${TIMING_NOTE}`)
  out.push("")

  const groups = new Map<string, BenchResult[]>()
  const keys: string[] = []
  for (const r of rs) {
    if (!groups.has(r.scenario)) {
      keys.push(r.scenario)
      groups.set(r.scenario, [])
    }
    groups.get(r.scenario)!.push(r)
  }
  keys.sort()

  for (const k of keys) {
    out.push(`## ${k}`)
    out.push("")
    const g = groups.get(k)!
    if (g.length > 0) {
      out.push(`Harness: \`${g[0].harness}\` | Bytes: ${g[0].bytes}`)
      out.push("")
    }
    out.push(
      "| Emulator | Exact | Distance | NDist | Extract runes | Expect runes | Time | MB/s | Alloc (MB) |",
    )
    out.push("|---|---|---|---|---|---|---|---|---|")
    for (const r of g) {
      let dist = "-"
      let ndist = "-"
      let exact = "n/a"
      if (r.hasExpected) {
        dist = String(r.distance)
        ndist = r.normDistance.toFixed(3)
        exact = String(r.exact)
      }
      const errStr = r.err ? " ⚠ " + r.err : ""
      out.push(
        `| ${r.emulator}${errStr} | ${exact} | ${dist} | ${ndist} | ${r.extractRunes} | ${r.expectRunes} | ${r.durationMs.toFixed(3)}ms | ${(r.throughput / (1024 * 1024)).toFixed(2)} | ${r.allocMB.toFixed(2)} |`,
      )
    }
    out.push("")
    if (dumpSnap) {
      for (const r of g) {
        out.push(`### ${r.emulator} — extracted snapshot`)
        out.push("")
        out.push("```")
        out.push(normalize(r.snapshot))
        out.push("```")
        out.push("")
      }
    }
  }
  return out.join("\n") + "\n"
}

/** JSON array of result objects (main.go:257-276). */
export function emitJSON(rs: BenchResult[]): string {
  const objs = rs.map((r) => ({
    scenario: r.scenario,
    harness: r.harness,
    emulator: r.emulator,
    bytes: r.bytes,
    has_expected: r.hasExpected,
    exact: r.exact,
    distance: r.distance,
    norm_distance: Number(r.normDistance.toFixed(4)),
    extract_runes: r.extractRunes,
    expect_runes: r.expectRunes,
    duration_ns: Math.round(r.durationMs * 1e6),
    throughput_bps: Number(r.throughput.toFixed(2)),
    alloc_mb: Number(r.allocMB.toFixed(2)),
    stable_after_ns: Math.round(r.stableAfterMs * 1e6),
    err: r.err,
  }))
  return JSON.stringify(objs, null, 2) + "\n"
}

/** Dispatches to the requested formatter. */
export function format(
  rs: BenchResult[],
  fmt: string,
  dumpSnap: boolean,
): string {
  switch (fmt) {
    case "markdown":
      return emitMarkdown(rs, dumpSnap)
    case "json":
      return emitJSON(rs)
    default:
      return emitTable(rs)
  }
}
