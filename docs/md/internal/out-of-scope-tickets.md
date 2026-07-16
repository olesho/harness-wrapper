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
