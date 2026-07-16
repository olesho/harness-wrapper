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

## HARNESS-WRAPPER-27 — blocked-backlog re-file, duplicate of HARNESS-WRAPPER-22

**Filed as:** `[observer] blocked backlog growing (backlog:blocked)`, observer
signature `obs-sig:de2a6070ed`.

**Why it landed here / why it is out of scope:** this is a **re-file of
HARNESS-WRAPPER-22** — identical observer signature (`obs-sig:de2a6070ed`) — and
that anomaly is already closed/merged/done. Grounding every contributing ticket
against the current tree shows there is **no un-actioned in-repo code defect
left**, so there is nothing safe for an automated worker to code. It was
escalated to `review` for human sign-off; this entry records the dedup so the
ticket is visibly investigated, not silently no-op'd.

**The 7 `blocked` tickets and their real status:**

- **Cause A — stray `bun.lock` (already root-fixed).** The integrator's
  post-merge clean-tree check saw an untracked `bun.lock` (a bun/npm bootstrap
  artifact of the root no-op `package.json`; no Bun usage exists in tracked
  source). That was fixed and merged by HARNESS-WRAPPER-22: `.gitignore:24` now
  contains `/bun.lock` (commit `8e2964d`, an ancestor of this worktree's base),
  and `git check-ignore bun.lock` confirms it is ignored. The one ticket it
  still blocks, `HARNESS-WRAPPER-19`, is **stale-blocked** — it exhausted its
  single auto-retry on the pre-fix `?? bun.lock` block and was never retried
  after the fix landed. Its task body targets `internal/screenbench/` (which
  **exists** in-repo: `internal/screenbench/{metrics,scenario,cmd,emulator}`),
  a legitimate in-repo Go→TS port. It needs an **operational re-queue, not a
  code change** — once retried, the merge gate's `git status --porcelain` will
  report nothing for `bun.lock`.

- **Cause B — cross-repo mismatch (not fixable by any commit here).** Plan
  `HARNESS-WRAPPER-1`'s children `-7/-14/-3` (blocking `-2/-4` transitively via
  plan-decompose/critic loops) were scoped against the TypeScript `meta-harness`
  module surface (`src/chat/**`, `src/wrapper/**`, `src/discovery/**`,
  `src/cli/run.ts`, `test/chat/fakeharness.ts`) — **none of which has ever
  existed in this repo** (`git log --all` over those paths → 0 commits). This
  workspace provisions worktrees against the **Go** `harness-wrapper` repo
  (`go.mod` → `module github.com/olesho/harness-wrapper`). No commit to this
  repository can supply another repo's dependency graph. The genuine fix is an
  **operator routing decision with no single obviously-correct answer** —
  repoint the `HARNESS-WRAPPER` workspace's `project-dir` at
  `/Users/oleh/Work/aether/meta-harness`, stand up a dedicated workspace, or
  re-scope `-1` — already flagged 5+ times and escalated in HARNESS-WRAPPER-22;
  **still pending**.

- **`HARNESS-WRAPPER-25` — mis-attributed, do not count.** It carries a
  **different** observer signature (`obs-sig:92d69f4a2f`,
  `dead-spawner:integrator:HARNESS-WRAPPER-5`), is already `triaged`/`implemented`
  (see HARNESS-WRAPPER-26 above), and must **not** be counted as a
  `backlog:blocked` cause — listing it double-counts a separate anomaly.

**Resolution:** no source change made in this repo — Cause A is already merged
(`.gitignore:24` / `8e2964d`) and Cause B has no in-repo code surface. For the
human reviewing: (1) dedup this against HARNESS-WRAPPER-22; (2) re-queue
`HARNESS-WRAPPER-19` against current base (low-risk, no code — its clean-tree
check now passes); (3) make the pending `meta-harness` routing decision for
`-7/-14/-3` — **do not** port `meta-harness` dependencies into `harness-wrapper`
and do not mark those resolved on the basis of any change here; (4) leave `-25`
to its own signature. Adding or re-touching application code under this ticket
would be a mis-port; the only edit made here is this dedup note.
