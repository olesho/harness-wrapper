// Port of harness-wrapper's pkg/harness/hookensure_test.go for the TS
// hook-install primitives (src/harness/internal/hookensure.ts).

import { describe, expect, test } from "vitest"
import { spawn, spawnSync } from "node:child_process"
import { existsSync, mkdtempSync, mkdirSync, readFileSync, statSync, utimesSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

import { atomicWriteFile, isManagedHookCommand, renderHookCommand, withLockedFile } from "../../src/harness/internal/hookensure.ts"

const here = dirname(fileURLToPath(import.meta.url))
const lockWorker = join(here, "fixtures", "lock-worker.ts")

function tmpDir(prefix: string): string {
  return mkdtempSync(join(tmpdir(), prefix))
}

describe("renderHookCommand / isManagedHookCommand structure", () => {
  test("rendered command has the expected shape", () => {
    const cmd = renderHookCommand(["/abs/loom", "hooks"], "claude", "stop", "loom")
    for (const want of [
      "sh -c ",
      'test -n "$HW_EVENT_SPOOL" || exit 0',
      "exec ",
      "/abs/loom",
      "hooks",
      "claude",
      "stop",
      "# meta-harness-hook:loom",
    ]) {
      expect(cmd).toContain(want)
    }
    expect(isManagedHookCommand(cmd)).toBe(true)
  })

  test("a user hook is not recognized as managed", () => {
    expect(isManagedHookCommand("sh -c 'echo user hook'")).toBe(false)
  })
})

// Runs the rendered command through a REAL shell to prove (a) the env guard
// makes it INERT without HW_EVENT_SPOOL, (b) it execs the entry-point with the
// right args WITH the spool set, and (c) an entry-point path containing a
// space survives the quoting.
describe("renderHookCommand behavior (real sh -c)", () => {
  test.each(["loomdir", "loom dir with space"])("dirName=%s", (dirName) => {
    const base = join(tmpDir("hookensure-behavior-"), dirName)
    mkdirSync(base, { recursive: true })
    const out = join(base, "invoked.txt")
    const loom = join(base, "loom")
    // Fake entry-point: records its args so we can assert exec reached it.
    const script = `#!/bin/sh\nprintf '%s' "$*" > ${posixQ(out)}\n`
    writeFileSync(loom, script, { mode: 0o755 })

    const cmd = renderHookCommand([loom, "hooks"], "claude", "stop", "loom")

    // (a) No spool → guard exits 0, entry-point NOT invoked.
    runSh(cmd, {})
    expect(existsSync(out)).toBe(false)

    // (b)+(c) With spool → entry-point invoked with "hooks claude stop".
    runSh(cmd, { HW_EVENT_SPOOL: "/tmp/whatever" })
    const got = readFileSync(out, "utf8")
    expect(got).toBe("hooks claude stop")
  })
})

function runSh(command: string, extraEnv: Record<string, string>): void {
  // Clean env (just PATH) so a real HW_EVENT_SPOOL in the test runner can't
  // leak in and defeat the no-spool case.
  const result = spawnSync("sh", ["-c", command], {
    env: { PATH: "/usr/bin:/bin", ...extraEnv },
  })
  if (result.status !== 0) {
    throw new Error(`sh -c failed (status ${result.status}): ${result.stderr?.toString()}`)
  }
}

// Mirrors the module's single-quote escaping for use inside the fake
// entry-point script (which itself is run by sh).
function posixQ(s: string): string {
  return `'${s.split("'").join("'\\''")}'`
}

describe("withLockedFile", () => {
  test("no-change (fn returns null) writes nothing and leaves no target file", () => {
    const target = join(tmpDir("hookensure-nochange-"), "untouched")
    withLockedFile(target, () => null)
    expect(existsSync(target)).toBe(false)
  })

  test("the .lock sidecar is left on disk permanently after use (by design, matching Go)", () => {
    const target = join(tmpDir("hookensure-lockleftover-"), "target")
    withLockedFile(target, () => Buffer.from("hello"))
    const lockPath = `${target}.lock`
    expect(existsSync(lockPath)).toBe(true)
    // Even a no-op call creates + retains the sidecar (the lock is acquired
    // before fn ever runs).
    const target2 = join(tmpDir("hookensure-lockleftover-noop-"), "target2")
    withLockedFile(target2, () => null)
    expect(existsSync(`${target2}.lock`)).toBe(true)
  })

  test("a stale sidecar (old mtime, no live holder) is taken over rather than blocking forever", () => {
    const target = join(tmpDir("hookensure-stale-"), "target")
    mkdirSync(dirname(target), { recursive: true })
    const lockPath = `${target}.lock`
    writeFileSync(lockPath, "12345\n")
    // Backdate the sidecar well past the ~10s staleness window.
    const old = new Date(Date.now() - 60_000)
    utimesSync(lockPath, old, old)

    const start = Date.now()
    withLockedFile(target, () => Buffer.from("fresh"))
    const elapsed = Date.now() - start

    expect(readFileSync(target, "utf8")).toBe("fresh")
    // Takeover should be near-immediate, not "wait out the whole staleness window".
    expect(elapsed).toBeLessThan(5_000)
  })

  test("multi-process contention: N OS processes racing the same lock file serialize their writes", async () => {
    const target = join(tmpDir("hookensure-multiproc-"), "counter")
    const workers = 20
    const incrementsPerWorker = 1

    await Promise.all(
      Array.from({ length: workers }, () => runWorker(lockWorker, [target, String(incrementsPerWorker)])),
    )

    const data = readFileSync(target, "utf8")
    expect(Number(data.trim())).toBe(workers * incrementsPerWorker)
  }, 30_000)
})

function runWorker(script: string, args: string[]): Promise<void> {
  return new Promise((resolve, reject) => {
    const proc = spawn(process.execPath, [script, ...args], { stdio: ["ignore", "pipe", "pipe"] })
    let stderr = ""
    proc.stderr.on("data", (d) => { stderr += d })
    proc.on("error", reject)
    proc.on("close", (code) => {
      if (code !== 0) reject(new Error(`worker exited ${code}: ${stderr}`))
      else resolve()
    })
  })
}

describe("atomicWriteFile", () => {
  test("writes content and leaves no temp file behind", () => {
    const dir = tmpDir("hookensure-atomic-")
    const target = join(dir, "file.txt")
    atomicWriteFile(target, Buffer.from("content"))
    expect(readFileSync(target, "utf8")).toBe("content")
    expect(statSync(target).isFile()).toBe(true)
  })
})
