// Thin CLI entrypoint for the TS screenbench bake-off, mirroring how Go
// separates `package main`'s `main()` from the reusable logic. All the real
// work (discover -> replay -> score -> format) lives in bench.ts; this file
// only parses flags, maps fatal errors to exit codes, and prints the report.
//
// Ported from internal/screenbench/cmd/screenbench/main.go (the flag block and
// top-level `main`). Same flags:
//
//   screenbench --corpus test/corpus
//   screenbench --corpus test/corpus --emulator xterm --scenario codex/short-reply
//   screenbench --corpus test/corpus --format markdown --dump-snapshots > report.md
//   screenbench --corpus test/corpus --emulator xterm --write-expected

import { parseArgs } from "node:util"

import { runBench, format, TIMING_NOTE } from "./bench.ts"
import type { BenchOptions } from "./bench.ts"

/** Parses a duration in milliseconds. Accepts a plain number ("200") or a
 * Go-style suffix ("200ms", "2s") for friendliness with main.go's --settle. */
function parseDurationMs(s: string): number {
  const trimmed = s.trim()
  let m = /^(\d+(?:\.\d+)?)ms$/.exec(trimmed)
  if (m) return Number(m[1])
  m = /^(\d+(?:\.\d+)?)s$/.exec(trimmed)
  if (m) return Number(m[1]) * 1000
  const n = Number(trimmed)
  if (Number.isFinite(n)) return n
  throw new Error(`invalid --settle duration: ${s}`)
}

async function main(): Promise<void> {
  let values
  try {
    ;({ values } = parseArgs({
      options: {
        corpus: { type: "string", default: "test/corpus" },
        emulator: { type: "string", default: "" },
        scenario: { type: "string", default: "" },
        format: { type: "string", default: "table" },
        "dump-snapshots": { type: "boolean", default: false },
        settle: { type: "string", default: "200" },
        "write-expected": { type: "boolean", default: false },
      },
      allowPositionals: false,
    }))
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err)
    process.stderr.write(`screenbench: ${msg}\n`)
    process.exit(2)
    return
  }

  const opts: BenchOptions = {
    corpus: values.corpus as string,
    emulator: values.emulator as string,
    scenario: values.scenario as string,
    settleMs: parseDurationMs(values.settle as string),
    writeExpected: values["write-expected"] as boolean,
  }
  const fmt = values.format as string
  const dumpSnap = values["dump-snapshots"] as boolean

  let results
  try {
    results = await runBench(opts)
  } catch (err) {
    const e = err as Error & { code?: string }
    switch (e.code) {
      case "NO_SCENARIOS":
        // Go exits 0 here: an empty corpus is not a failure.
        process.stderr.write(`screenbench: ${e.message}\n`)
        process.stderr.write(
          "  record some with `screenbench-record` first; see test/corpus/README.md\n",
        )
        process.exit(0)
        return
      case "UNKNOWN_EMULATOR":
      case "WRITE_EXPECTED_NO_EMULATOR":
        process.stderr.write(`screenbench: ${e.message}\n`)
        process.exit(2)
        return
      default:
        process.stderr.write(`screenbench: ${e.message}\n`)
        process.exit(1)
        return
    }
  }

  // Surface the timing caveat on stderr regardless of format so it is present
  // even for machine-readable JSON output on stdout.
  process.stderr.write(`screenbench: note: ${TIMING_NOTE}\n`)

  process.stdout.write(format(results, fmt, dumpSnap))
}

main().catch((err) => {
  const msg = err instanceof Error ? err.stack || err.message : String(err)
  process.stderr.write(`screenbench: ${msg}\n`)
  process.exit(1)
})
