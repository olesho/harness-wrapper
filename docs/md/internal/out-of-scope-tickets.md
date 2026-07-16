# Out-of-scope tickets

A log of fleet-db tickets filed against this repo (`harness-wrapper`) whose
root cause lives in another repo, with no corresponding code change made
here. Kept so a human/integrator can see at a glance that the ticket was
investigated, not silently skipped.

## HARNESS-WRAPPER-23 — dead-spawner re-alert false positive

**Filed as:** `[observer] crashed/dead spawner worker left HARNESS-WRAPPER-5
working (dead-spawner:worker:HARNESS-WRAPPER-5)`

**Why it landed here:** the observer's dead-spawner detector fires against
the *task* it's watching (`HARNESS-WRAPPER-5`, which lives in this repo's
fleet-db workspace), so the ticket gets filed here even though the detector
itself lives elsewhere.

**Actual defect location:** `orche`'s `@orche/agent` package,
`packages/agent/src/observer.ts` — specifically `fileAnomaly`'s convergence
guard (currently gated to `a.kind === 'reopen-loop'`), which needs to also
cover `dead-spawner` by comparing the re-fetched task's `assignee` against
the dead agent id in `anomaly.facts.agentId`, so it stops re-posting "still
occurring" once a different live agent has taken over the task.

**Confirmed:** `ORCHE_CANONICAL_REPO=/Users/oleh/repos/harness-wrapper.git`
for this ticket, and `grep -ri "observer|spawner|dead-spawner"` across this
worktree (excluding `node_modules`) turns up nothing but two coincidental
`IntersectionObserver` hits in bundled docs JS
(`docs/gen/assets/app.js`, `scripts/docs/assets/app.js`). No observer,
spawner, or dead-spawner-detector code exists in this repo.

**Resolution:** no source change made in this repo — there is nothing here
to change. This needs to be redirected to the `orche` project so the fix
described above can be applied to `packages/agent/src/observer.ts` (and a
test added to `packages/agent/test/observer.unit.test.ts` covering the
dead-spawner + assignee-changed convergence case).

## HARNESS-WRAPPER-26 — dead-spawner false positive on a *completed* worker

**Filed as:** `[observer] crashed/dead spawner worker left HARNESS-WRAPPER-25
working (dead-spawner:worker:HARNESS-WRAPPER-25)`

**Why it landed here:** same mechanism as HARNESS-WRAPPER-23 above — the
observer's dead-spawner detector fires against the *task* it's watching
(`HARNESS-WRAPPER-25`, which lives in this repo's fleet-db workspace), so the
ticket gets filed here even though the detector itself lives elsewhere.

**What actually happened (false positive, not a crash):** the detector
flagged worker `agent:worker:f847ae17-…` as "crashed/dead" because it saw the
agent still `working` on HARNESS-WRAPPER-25 with no heartbeat/release for
678s. But that entire silence window is *post-completion*: the worker had
already finished the task, committed, and handed off. Concretely — it posted
its completion comment at 14:15:26 UTC (`worker: implemented on
work/HARNESS-WRAPPER-25 @ 8fcde4b`), landed commit `8fcde4b`
("HARNESS-WRAPPER-25: wire JS/TS gate (vitest + tsc) into make test"), the
task was relabeled `implemented`, and it was reassigned to integrator
`f216fbb8` (whose branch `agent/HARNESS-WRAPPER-25-f216fbb8` is a live
worktree containing `8fcde4b`). The detector read "went quiet because it
finished and released" as "went quiet because it crashed."

**Actual defect location:** `orche`'s `@orche/agent` package,
`packages/agent/src/observer.ts` — same `fileAnomaly` convergence guard as
HARNESS-WRAPPER-23.

**Refinement over HARNESS-WRAPPER-23:** HARNESS-WRAPPER-23 framed the fix as
suppressing *re-posts* ("still occurring") once a live agent takes over. But
`dead-spawner:worker:HARNESS-WRAPPER-25` is a **brand-new signature — an
initial file, not a re-post** — so a re-file-only convergence guard would not
have prevented it. The assignee-/state-advanced check must gate the **initial
detection**, not just re-alert convergence: before filing (and before
re-posting) a `dead-spawner` anomaly, re-fetch the task and suppress if
`task.assignee !== anomaly.facts.agentId` (a different live agent — here
integrator `f216fbb8` — has taken over) **or** the task is already in a
terminal/handed-off state (`implemented`). Extend the convergence guard from
`kind === 'reopen-loop'` to also cover `dead-spawner`.

**Confirmed:** `grep -rniE "dead-spawner|fileAnomaly|reopen-loop|spawner|anomaly"`
across this worktree (excluding `node_modules`) returns only coincidental
matches — a `// … parented to the spawner.` comment in
`pkg/harness/claude/subagent_test.go` and the company name
`anomalyco/opencode #21941` in a code comment in
`pkg/harness/opencode/opencode.go`; none is a fleet spawner/heartbeat/reaper.
The worker did its job correctly (disproving "crashed/dead"): commit
`8fcde4b` exists (`git cat-file -e 8fcde4b`) and at that commit the Makefile
gains `test: test-js`, wiring `vitest` + `tsc --noEmit` — a clean
merged-forward implementation being consumed by a live integrator, the
opposite of an abandoned task.

**Resolution:** no source change made in this repo. The reported "make test
JS/TS gate" gap this class once surfaced is already fixed by `8fcde4b`;
re-touching it would duplicate landed work. Redirect the real fix to `orche`
(`packages/agent/src/observer.ts`), and add a test to
`packages/agent/test/observer.unit.test.ts` where an agent is silent past the
dead-spawner threshold **but the watched task's `assignee` has changed to a
different live agent (and/or the task is labeled `implemented`)**, asserting
that no `dead-spawner` anomaly is filed (and no "still occurring" comment is
added). That single guard stops both the false positive and the recursive
re-flag loop.
