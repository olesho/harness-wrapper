// Standalone worker process spawned by hookensure.test.ts to prove
// withLockedFile serializes ACROSS OS PROCESSES (not just within one), since a
// single Node process is single-threaded and can't demonstrate contention on
// its own. Usage: `node lock-worker.ts <targetPath> <incrementCount>` —
// increments a counter stored as decimal text in targetPath, once per fn call.

import { withLockedFile } from "../../../src/harness/internal/hookensure.ts"

const [, , targetPath, countArg] = process.argv
const count = Number(countArg)

for (let i = 0; i < count; i++) {
  withLockedFile(targetPath, (existing) => {
    const cur = existing !== null ? Number(existing.toString("utf8").trim()) || 0 : 0
    return Buffer.from(String(cur + 1), "utf8")
  })
}
