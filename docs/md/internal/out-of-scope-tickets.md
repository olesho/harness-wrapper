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

## HARNESS-WRAPPER-28 — blocked-backlog re-file (4th), duplicate of HARNESS-WRAPPER-22 / -27

**Filed as:** `[observer] blocked backlog growing (backlog:blocked)`, observer
signature `obs-sig:de2a6070ed` — the **same** signature already triaged as
HARNESS-WRAPPER-22 (closed/merged/done) and HARNESS-WRAPPER-27 (at `review`).
This is the **fourth** occurrence of one anomaly.

**Why it is out of scope (pointer, not a re-analysis):** the full write-up
already lives in §HARNESS-WRAPPER-27 above (Cause A stray `bun.lock`, already
root-fixed at `.gitignore:24` / `8e2964d`; Cause B cross-repo `meta-harness`
mismatch with no in-repo code surface; `-25` mis-counted under its own signature
`obs-sig:92d69f4a2f`). Nothing in that analysis has changed at this base
(`f32e256`): `git check-ignore bun.lock` still passes, `git log --all` over the
`meta-harness` paths (`src/chat/**`, `src/wrapper/**`, `src/discovery/**`,
`src/cli/run.ts`, `test/chat/fakeharness.ts`) is still 0 commits, and
`internal/screenbench/{metrics,scenario,cmd,emulator}` still exists. There is no
new un-actioned in-repo code defect to fix. Re-deriving §-27 here would be the
"pure noise" the observer itself warned of, so this entry is deliberately a
cross-reference only.

**The recurring defect is external.** The reason this keeps re-filing is
`orche`'s `@orche/agent` observer (`packages/agent/src/observer.ts`):
`fileAnomaly`'s convergence guard is gated to `a.kind === 'reopen-loop'` and does
not suppress a `backlog:blocked` signature whose prior ticket is already
`closed`/`done` or sitting in `review`, and the `backlog:blocked` count still
includes a stale-blocked ticket (`-19`) and a foreign-signature ticket (`-25`).
Both are the same class flagged in §HARNESS-WRAPPER-23 and §HARNESS-WRAPPER-26.

**Resolution:** no source change and no re-analysis made in this repo. For the
human at the `review` gate: (1) **close this as a duplicate** of HARNESS-WRAPPER-22
/ -27; (2) make the still-pending Cause B `meta-harness` routing decision; (3)
operationally re-queue `HARNESS-WRAPPER-19` against current base (no code — its
clean-tree check now passes); (4) route the real observer dedup/convergence fix
to `orche` (`packages/agent/src/observer.ts`, with tests in
`packages/agent/test/observer.unit.test.ts`), extending the convergence guard to
cover `backlog:blocked` and excluding stale-blocked/foreign-signature tickets
from the count. This dedup pointer is the only edit made here.

## HARNESS-WRAPPER-29 — blocked-backlog re-file (5th), duplicate of HARNESS-WRAPPER-22 / -27 / -28

**Filed as:** `[observer] blocked backlog growing (backlog:blocked)`, observer
signature `obs-sig:de2a6070ed` — the **same** signature already triaged as
HARNESS-WRAPPER-22 (closed/merged/done), HARNESS-WRAPPER-27 (at `review`), and
HARNESS-WRAPPER-28 (deduped). This is the **fifth** occurrence of one anomaly and
was itself escalated to `review` for human sign-off.

**Why it is out of scope (pointer, not a re-analysis):** the full write-up already
lives in §HARNESS-WRAPPER-27 above (Cause A stray `bun.lock`, already root-fixed at
`.gitignore:24` / `8e2964d`; Cause B cross-repo `meta-harness` mismatch with no
in-repo code surface; `-25` mis-counted under its own signature `obs-sig:92d69f4a2f`),
and §HARNESS-WRAPPER-28 already deduped the 4th firing against it. Nothing in that
analysis has changed at this base (`53f4cce`), re-verified here:
`git check-ignore bun.lock` still passes (exit 0), `.gitignore:24` = `/bun.lock`,
`git log --all` over the `meta-harness` paths (`src/chat/**`, `src/wrapper/**`,
`src/discovery/**`, `src/cli/run.ts`, `test/chat/fakeharness.ts`) is still 0 commits,
`go.mod` still declares `module github.com/olesho/harness-wrapper` (a **Go** repo),
`internal/screenbench/{metrics,scenario,cmd,emulator}` still exists, and a
`grep` for `fileAnomaly`/`obs-sig`/`backlog:blocked`/`de2a6070` over the tree still
returns nothing. There is no new un-actioned in-repo code defect to fix. Re-deriving
§-27 here would be the "pure noise" the observer itself warned of, so this entry is
deliberately a cross-reference only.

**The recurring defect is external (unchanged from §-28).** This keeps re-filing
because `orche`'s `@orche/agent` observer (`packages/agent/src/observer.ts`) does not
suppress a `backlog:blocked` signature whose prior ticket is already
`closed`/`done`/`review`, and the `backlog:blocked` count still includes a
stale-blocked ticket (`-19`) and a foreign-signature ticket (`-25`). Same class as
§HARNESS-WRAPPER-23 / §HARNESS-WRAPPER-26 / §HARNESS-WRAPPER-28.

**Resolution:** no source change and no re-analysis made in this repo. For the human
at the `review` gate: (1) **close this as a duplicate** of HARNESS-WRAPPER-22 / -27 /
-28; (2) make the still-pending Cause B `meta-harness` routing decision (the
architectural call with no single correct answer); (3) operationally re-queue
`HARNESS-WRAPPER-19` against current base (no code — its clean-tree check now passes);
(4) route the real observer dedup/convergence fix to `orche`
(`packages/agent/src/observer.ts`, tests in
`packages/agent/test/observer.unit.test.ts`), extending the convergence guard to cover
`backlog:blocked` and excluding stale-blocked/foreign-signature tickets from the count.
This dedup pointer is the only edit made here.

## HARNESS-WRAPPER-97 — release-slot wedge: a **real** defect, in the wrong repo

**Filed as:** `[observer] release branch behind base (dev..main) — not promoting
(release-lag:dev..main)`, escalated to `review`.

**How this differs from every entry above.** The other out-of-scope tickets here
are observer *false positives* — the anomaly did not exist. This one is
**genuine**: the release cron agent's single concurrency slot really is wedged,
`main` really is not being promoted, and the lag really is growing (27 → 32
commits during triage alone). Only the *location* is wrong. It is also distinct
from [HARNESS-WRAPPER-56](../../triage/HARNESS-WRAPPER-56.md), which carried the
same `release-lag:dev..main` signature but was a detector artifact (late-arriving
commits mis-aged); here promotion is actually dead.

**Why it landed here:** the observer files a `release-lag` anomaly against the
workspace whose release branch is lagging (`HARNESS-WRAPPER`), so the ticket
lands in this repo's fleet-db workspace even though the release *machinery* that
wedged lives in `orche`.

**Actual defect location:** `orche`'s `@orche/agent` package — a cron slot in
`packages/agent/src/spawner.ts:640-648` that is released only by its own
`runTick` promise settling, so a tick that never settles owns the sole slot for
the process lifetime, plus the disabled/mis-scoped max-run watchdog
(`spawner.ts:1436-1498`) and the unbounded release agent
(`examples/.orche/agents/release.ts:558-569`) that let it go unnoticed.

**Confirmed:** none of those files exist in this repo — it is a Go module
(`module github.com/olesho/harness-wrapper`). A search of the tree (excluding
`node_modules`) for `runTick`, `maxRunMs`, and `at_capacity` returns **zero**
hits, and `spawner` turns up only two coincidental English-prose comments
(`internal/env/openshell/openshell.go:266` "a real process spawner";
`pkg/harness/claude/subagent_test.go:55` "parented to the spawner") — no cron,
slot-tracking, or watchdog code exists here. All five defects and the live-fleet
evidence were re-verified against the real orche source and the running
supervisor at implementation time.

**Resolution:** no source change made in this repo — there is nothing here to
change. The full triage, the five-part root cause, the fix plan (A–E) with its
test plan, and the required operator actions are recorded in
[`docs/triage/HARNESS-WRAPPER-97.md`](../../triage/HARNESS-WRAPPER-97.md). For
the human at the `review` gate: (1) **re-file in the `ORCHE` workspace**, which
already carries the sibling tickets ORCHE-14/15/26 on this subsystem; (2)
**restart the wedged HARNESS-WRAPPER supervisor (pid 65669)** to free the slot —
this is the only thing that restores promotion, it is not a code change, and it
kills every in-flight agent in the fleet, so it must be scheduled; (3) file a
separate ticket for the 46 retained `agent-release-*` worktrees.

## HARNESS-WRAPPER-98 — dead-spawner, genuinely wedged lease (out of repo)

**Filed as:** `[observer] crashed/dead spawner plan-reviewer left
HARNESS-WRAPPER-79 working (dead-spawner:plan-reviewer:HARNESS-WRAPPER-79)`,
escalated to `review`.

**This one is confirmed real — unlike §-23 and §-26.** Both of those were
false positives: the watched task had already moved on (a different live
agent held it, or it carried a hand-off label), so "went quiet" meant
"finished and released". Here neither is true. HARNESS-WRAPPER-79 is still
`status: in_progress` with `assignee: agent:plan-reviewer:f9f5694a-680a-427d-a043-97dc9534cf9d`
— the same agent the anomaly names — no hand-off label (`implemented`) ever
appeared, and attempt 2's worktree
(`~/.orche/worktrees/agent-plan-reviewer-f9f5694a-…`) shows no file activity
and no comment. The lease is genuinely stranded: the plan-reviewer stopped
heartbeating for 308s while still holding the task, and nothing will release
it. The anomaly is filing correctly. Only the *location* of the defect is
wrong, which is what puts it here rather than in the false-positive class.

**Why it landed here:** same mechanism as §-23/§-26 — the dead-spawner
detector files against the *task* it is watching (`HARNESS-WRAPPER-79`, in
this repo's fleet-db workspace), so the ticket lands in this repo even though
the detector, the heartbeat emitter, and the lease machinery all live in
`orche`.

**Actual defect location:** `orche`'s `@orche/agent` package. The chain is
entirely out-of-repo: a run heartbeats via `packages/agent/src/spawner.ts:823`
(`emitScreen(s => s.agentHeartbeat(...))`); a hard kill (SIGKILL/OOM/host
death) stops it without reaching finalize, so no in-process JS runs and
neither `agent_stopped` nor `task_released` is ever published;
`apps/screen/src/state.ts:365-417` can only leave `'working'` via those two
events, so the record wedges; the observer replays the same bus
(`packages/agent/src/observer.ts:519-545`) and flags every
`status === 'working' && now - agent.lastSeen > deadSpawnerMs` agent.
`fileAnomaly`'s grounding guard (`observer.ts:846-854`) drops a
`dead-spawner` only when the task status is in `DEAD_SPAWNER_TERMINAL_STATUSES`
(`closed`, `tombstone`; `observer.ts:111`) — and `in_progress` is
deliberately *not* terminal (ORCHE-130) — so the anomaly correctly files.

**Correction to the observer's own suggestions.** Two of the four are
**already implemented** in `orche` (grounded against that tree at `737ea45`);
a worker acting on the comment verbatim would write redundant code:

- *"It comments and dies while keeping the assignment"* is **inaccurate for
  the deadline path**: `spawner.ts:1219-1221` runs `onTimeout`, `:1226-1235`
  posts the `agent run <outcome>: <reason>` comment, and `:1237-1258` calls
  `finalize(client, { outcome, reopenOnTimeout, priorLabels, … })` — the
  lease is always released, the task reopened/transitioned only if owned. It
  is accurate only for a hard kill, where no JS runs at all.
- *"Bound the retries"* is **already done**:
  `packages/agent/src/finalize.ts:60` `MAX_CONSECUTIVE_TIMEOUTS = 3`, tracked
  via the `timeout-attempt-<n>` label prefix (`finalize.ts:61,133`) and
  routed to `blocked` + `stuck` at `finalize.ts:426-444`. The
  `timeout-attempt-1` label on HARNESS-WRAPPER-79 is that machinery working:
  attempt 1 (`agent:plan-reviewer:f7a7b413`, `agent run timeout: context
  deadline exceeded` at 17:45:30Z) timed out, released, reopened; attempt 2
  claimed.

**The two genuine remaining gaps:**

1. **No out-of-band reaper** for an agent that dies *without* reaching
   `finalize` — the gap ORCHE-130 explicitly left open.
   `apps/screen/src/state.ts:530-541`'s `orphanMs` reaper prunes only the
   screen app's local dashboard state; `observer.ts` never calls `prune()`,
   and fleet-db's `assignee`/`in_progress` is never touched. A hard-killed
   run therefore wedges the ticket in **both** the replayed registry and
   fleet-db, indefinitely, with no timer that can ever clear it. This is
   exactly what happened to HARNESS-WRAPPER-79.
2. **The §-26 refinement is still unfiled in ORCHE.** `fileAnomaly`
   (`observer.ts:818-855`) compares only `task.status`; it never compares
   `task.assignee` against `a.facts.agentId`, nor checks hand-off labels like
   `implemented`. That is the fix specified in §HARNESS-WRAPPER-26 above and
   still not landed. Note it would **not** have suppressed this ticket — the
   assignee is unchanged here — which is the correct behaviour and must stay
   that way.

**Confirmed out of repo.** Re-verified at this base (`c29a129`):
`git grep -niE "dead-spawner|fileAnomaly|task_released|agent_stopped|heartbeat.*release|spawner"`
over tracked source returns exactly **two coincidental hits** —
`internal/env/openshell/openshell.go:266` (`// … a real process spawner.`) and
`pkg/harness/claude/subagent_test.go:55` (`// … parented to the spawner.`);
neither is a fleet spawner, heartbeat emitter, lease reaper, or anomaly
detector. `go.mod` declares `module github.com/olesho/harness-wrapper` — a Go
repo with no `packages/` or `apps/` tree, so `packages/agent/src/spawner.ts`,
`packages/agent/src/observer.ts`, `packages/agent/src/finalize.ts`, and
`apps/screen/src/state.ts` do not exist here. This repo's surface
(`pkg/wrapper`, `pkg/screen`, `pkg/turns`, `pkg/chat`, `cmd/harness-chatd`)
supervises a *single* harness PTY and has no notion of fleet leases,
assignees, or agent liveness.

**Routing.** File as a follow-up to `ORCHE-130` in the **ORCHE** workspace,
per the standing human directive on HARNESS-WRAPPER-24 (2026-07-16): a human
(oleh) already ruled on this exact class — *"Do not reopen in
HARNESS-WRAPPER"* — and re-filed it as `fleet-db://ORCHE/ORCHE-130`.

**Resolution:** no source change made in this repo — adding fleet/lease/
observer logic to a Go PTY-supervision library would be a mis-port, and this
dedup/out-of-scope entry is the only safe and correct in-repo edit. For the
human at the `review` gate:

1. **Unblock HARNESS-WRAPPER-79 now** — unassign
   `agent:plan-reviewer:f9f5694a-…` and set it back to `open` so a fresh
   reviewer can claim it. It is pinned to a non-progressing agent with no
   timer that will ever release it. This is an operational fleet-db write
   against another agent's live lease, which an automated worker must not
   perform unilaterally.
2. **Consider decomposing HARNESS-WRAPPER-79.** It is an oversized plan
   (permission-mode detection *and* mid-session switching, spanning
   `pkg/turns`, `pkg/chat`, `pkg/discovery`, plus corpus verification). Two
   reviewers have now exceeded the run deadline on it; a third will hit the
   same wall and burn the third `timeout-attempt` slot, parking it at
   `blocked`/`stuck`.
3. **Route the code fix to ORCHE** as a follow-up to ORCHE-130, with tests in
   `packages/agent/test/`: (a) an out-of-band reaper test — an agent stuck
   `working` past a liveness threshold with no `agent_stopped`/`task_released`
   gets its fleet-db assignment released and the task reopened; (b) an
   `observer.unit.test.ts` case asserting `fileAnomaly` drops a
   `dead-spawner` when `task.assignee !== anomaly.facts.agentId` or the task
   carries a hand-off label (`implemented`), while **still filing** when the
   assignee is unchanged — the HARNESS-WRAPPER-79 shape must keep coming
   through.
