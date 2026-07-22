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

## HARNESS-WRAPPER-97 / -100 / -110 — release-slot wedge: a **real** defect, in the wrong repo

**Filed as:** `[observer] release branch behind base (dev..main) — not promoting
(release-lag:dev..main)`, escalated to `review`. **Re-fired as HARNESS-WRAPPER-100
and again as HARNESS-WRAPPER-110** under the identical observer signature
`obs-sig:1bf9fcd2c6` — see the amendments at the end of this section.

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

**Amendment — re-fired as HARNESS-WRAPPER-100 (2026-07-22).** The signature
re-fired unchanged (`obs-sig:1bf9fcd2c6`) because **operator action (2) above has
not happened**: supervisor **pid 65669** is still alive (started 11:32:51, same
process), still holding the sole cron slot, and the lag has grown **27 → 52 → 57**
commits (`git -C /Users/oleh/repos/harness-wrapper.git rev-list --count main..dev`
= 57; `dev..main` = 0, so `main` remains a clean ancestor — the gate would simply
promote if it ran). The re-fire is expected behaviour, not new information: this
signature will keep re-firing on every observer sweep until the supervisor is
restarted, regardless of what is merged here. HARNESS-WRAPPER-100 is therefore a
duplicate with **no in-repo deliverable** beyond this amendment — the triage, the
five-part root cause, the A–E fix plan and its tests are already recorded in
[`docs/triage/HARNESS-WRAPPER-97.md`](../../triage/HARNESS-WRAPPER-97.md) and were
re-verified against orche HEAD `737ea45` and the live fleet on 2026-07-22. **Do not
open a second triage document for this signature**; amend this section instead.

**Amendment — re-fired a third time as HARNESS-WRAPPER-110 (2026-07-22 21:53).**
Operator action (2) *still* has not happened, so the signature fired again,
unchanged. Live state re-verified in the -110 worktree at **21:55** (measured here,
not carried over from the ticket): supervisor **pid 65669** is alive and has never
been restarted (`ps -p 65669` → start **Wed Jul 22 11:32:51**, elapsed **10:22**,
`agent-cli.ts up --dir …/.orche --workspace HARNESS-WRAPPER`); `main` is still at
**`6281927`** (10:15:03, the last promotion); the lag is **82 total / 41
first-parent** commits (`rev-list --count main..dev` = 82, `--first-parent` = 41),
while `dev..main` = **0**, so `main` remains a clean ancestor and the gate would
simply promote if it ran. The progression across filings is **27 → 32 → 52/57 →
78/40 (ticket, 21:53) → 82/41 (verified, 21:55)** — it grows between the ticket
being written and the work being done, which is itself the point: nothing is
draining. A tree-wide grep for `runTick` / `at_capacity` / `maxRunMs` over `*.go`
still returns **zero** hits.

Two findings are genuinely new to this filing:

- **No `ORCHE` ticket covers Fix A–D.** The sibling tickets that share this
  signature label — `fleet-db://ORCHE/ORCHE-31` and `…/ORCHE-40`, resolved during
  -110 triage — are both **closed** and both fix the *detector* (first-parent
  commit count; clamped `oldestUnpromotedMs`); neither touches the wedged cron
  slot. (Not re-run here: a worker must not issue `orche` commands.) The
  wedged-slot defect is therefore still **unfiled anywhere**, and operator action
  (1) above remains outstanding. A useful corollary: because ORCHE-31/-40 already
  removed the two known phantom-fire paths, this fire cannot be a detector
  artifact — the 41-commit first-parent count and the 210-minute oldest-unpromoted
  age are post-fix, trustworthy values.
- **Worktree accumulation is still untracked** — **46** retained `agent-release-*`
  directories under `cleanup: 'on-success'`, including the wedged 15:40 tick
  (`agent-release-8df3c501-…`, frozen at mtime 15:40, `.pid` = 65669, staged
  `.claude/` present, `node_modules` absent) and the failed 13:40 tick
  (`agent-release-3a870b7e-…`). Operator action (3) above has not been taken.

**This signature will re-fire on every observer sweep until pid 65669 is restarted
or Fix A lands in `orche`. No change made in this repository — of any kind — can
affect it.** HARNESS-WRAPPER-110 has no in-repo deliverable beyond this amendment.

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
   `pkg/turns`, `pkg/chat`, `pkg/discovery`, plus corpus verification — and
   partly overtaken since: the launch-time knob shipped in HW-95/96
   (`--permission-mode` / `permission_mode`) and detection shipped in HW-105
   (claude-code) and HW-106 (codex), leaving **mid-session switching** as the
   open half, which is what still makes decomposing it worthwhile). Two
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

## HARNESS-WRAPPER-99 — dead-spawner false positive: right diagnosis, wrong repo

**Filed as:** `[observer] crashed/dead spawner plan-critic left HARNESS-WRAPPER-78
working (dead-spawner:plan-critic:HARNESS-WRAPPER-78)`, escalated to `review`.

**Why it landed here:** identical misrouting to §HARNESS-WRAPPER-23 and
§HARNESS-WRAPPER-26 — the observer files a `dead-spawner` against the *task* it is
watching (`HARNESS-WRAPPER-78`, in this repo's fleet-db workspace), so the ticket
lands here even though the detector lives in `orche`.

**How this differs from the earlier entries in the class.** HARNESS-WRAPPER-23 and
-26 both proposed the same assignee-based grounding guard and both stopped there.
HARNESS-WRAPPER-99 goes further and finds the *systemic* cause the other two missed:
the observer's own bus lag. Its `tick` drains with one un-looped, un-limited
`pull()` per 300s (`observer.ts:242`) against a backend whose `pull` is single-page
by construction (`fleet.ts:258`), so once arrivals exceed one 1000-message page the
cursor lag grows monotonically — and `dead-spawner`, being an *absence* detector,
cannot distinguish "no heartbeat exists" from "I haven't read the heartbeats yet".
Every actively-working agent progressively reads as dead. Three signatures firing in
one digest (`:-78`, `:-79`, `:-89`) is that fault, not three coincident crashes.

**Actual defect location:** `orche` — `packages/agent/src/observer.ts` (the `tick`
drain and `fileAnomaly`'s grounding check), with the single-page contract in
`packages/queue/src/fleet.ts`. Every load-bearing claim was re-verified against real
orche source; all hold.

**Confirmed:** none of those files exist here — this is a Go module. `grep -rn
"spawner\|observer\|heartbeat" --include="*.go"` returns only unrelated matches (an
`exec` process spawner at `internal/env/openshell/openshell.go:266`, an activity
observer callback at `pkg/harness/run.go:78`, screen-change observers, and a mock
harness's `--api-error-heartbeat` flag). No bus, queue, anomaly detector, or
`fileAnomaly` exists in this repo.

**One correction to the ticket, and it matters.** The ticket's proposed **Fix #2**
(skip the `dead-spawner` scan when `now - max(ts over windowEvents)` exceeds
`deadSpawnerMs`) is unsound as written. With `windowMs` 30 min and `deadSpawnerMs`
4.5 min, a fleet whose only `working` agents are the dead ones has its newest
in-window event *be* the dead agent's last heartbeat — so the guard suppresses the
scan across the entire genuine detection band, then the acquire event ages out and
the anomaly can never fire. It goes blind in exactly the outage it exists to catch.
The ticket's own proposed test passes only because it seeds heartbeats from *other*
live agents. Fix by keying freshness on the drain (did the pull loop reach empty?)
rather than on event age. Details in
[`docs/triage/HARNESS-WRAPPER-99.md`](../../triage/HARNESS-WRAPPER-99.md).

**Resolution:** no source change made in this repo — there is nothing here to
change. For the human at the `review` gate: (1) **decide the disposition** —
close-as-invalid vs. re-file against `ORCHE` as a follow-up to ORCHE-130;
HARNESS-WRAPPER-24 (2026-07-16) already directed that this class not be re-filed
here; (2) if re-filed, **land the ownership guard (Fix #1) alone first** — a
strict-subset check needing no new I/O, which suppresses this ticket while provably
still filing the genuinely wedged `:-79`, and which requires extending the existing
`observer.unit.test.ts:725` fixture with an `assignee` or it starts failing; (3)
**do not land Fix #2 verbatim** — take its drain-to-empty loop, drop its event-age
guard; (4) confirm the `:-78`/`:-89` false-positive claim against live fleet history
before closing them, which could not be re-verified here (running `orche` commands
was out of scope for this task).

## HARNESS-WRAPPER-111 — the observer dismissed the anomaly and it was filed anyway

**Filed as:** `[observer] crashed/dead spawner worker left HARNESS-WRAPPER-109
working (dead-spawner:worker:HARNESS-WRAPPER-109)`.

**Why it landed here:** unchanged from §HARNESS-WRAPPER-23, §HARNESS-WRAPPER-26 and
§HARNESS-WRAPPER-99 — the observer files a `dead-spawner` against the *task* it is
watching (`HARNESS-WRAPPER-109`, in this repo's fleet-db workspace), so the ticket
lands in this workspace even though every line of the defect lives in `orche`. This
is the **fourth** recurrence of the class.

**What is new, and it is not more of the same noise.** The three prior sweeps all
argued the detector is too noisy. This one finds something worse and much cheaper to
fix: the observer **correctly judged the anomaly a false positive and wrote an
explicit `DISMISS` verdict**, which was then silently discarded. The matcher at
`packages/agent/examples/observer.ts:181` is
``reply.includes(`DISMISS ${sig}`)`` — a strict substring test that any markdown
between the keyword and the signature defeats. The observer wrote
``**DISMISS `dead-spawner:worker:HARNESS-WRAPPER-109`**``; the backtick broke the
match; `fileAnomaly` ran at `:187`. `extractInvestigation` (`:229`) locates blocks by
`l.includes(sig)`, which *does* tolerate the decoration, so the dismissal was then
posted as the ticket's own comment at `:196`. The prompt's contract is line-oriented
(`prompts/observer.md:43`); the implementation is character-exact. The
"a parse miss errs toward filing" default at `:170-173` is right for **silence** and
inverted here: the strongest signal that a ticket should not exist is the exact input
that produces it.

**This ticket is its own reproduction.** Its description is verbatim `renderBugBody`
output and its sole comment (`agent:observer:07f8a7c5-…`, 2026-07-22T19:59:11Z) opens
with that `DISMISS` line. Both artifacts come from the same `onComplete` pass reading
the same `reply` string, so no fleet-state reconstruction or ordering assumption is
needed to prove the defect.

**Actual defect location:** `orche` — `packages/agent/examples/observer.ts:181` and
`:237` (primary), plus the two re-verified secondary causes: no ownership grounding in
`fileAnomaly` (`packages/agent/src/observer.ts:822`/`:852`, the still-unlanded
HARNESS-WRAPPER-99 Fix #1) and the un-looped single-page drain
(`examples/observer.ts:117`, `src/observer.ts:242`, `packages/queue/src/fleet.ts:256-261`).
The underlying event was benign and self-healed: `agent:worker:d10a71e6` blew its run
deadline after preserving two commits, and orche released and re-leased the task to
`agent:worker:53ee7ce1` under `timeout-attempt-1`.

**Confirmed:** none of those files exist here — this is a Go module.
`grep -rniE 'dead-spawner|fileAnomaly|obs-sig|deadSpawnerMs' --include='*.go' .`
returns **zero** hits; the only tree-wide matches are this log and the triage records
under `docs/triage/`. No bus, queue, anomaly detector, or verdict parser exists in
this repo.

**Resolution:** no source change made in this repo — documentation only, and no human
gate requested, because the root cause is unambiguous. The fix is staged as a
ready-to-apply cross-repo bundle,
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md)
(Patch A: parse the verdict by line, decoration-tolerant, anchored so mid-sentence
prose cannot suppress; Patch B: the assignee grounding guard, land it this time), with
the full evidence chain in
[`docs/triage/HARNESS-WRAPPER-111.md`](../../triage/HARNESS-WRAPPER-111.md). That
bundle must be committed / PR'd **in `orche`** under its own ticket, as a follow-up to
ORCHE-130. Once handed off, the correct disposition for this ticket in this workspace
is **close-as-invalid** — HARNESS-WRAPPER-24 (2026-07-16) already directed that this
class not be re-filed here. One item is carried forward for a human and is not a code
defect: HARNESS-WRAPPER-79's decomposition children keep exceeding run deadlines
(`-79` twice, `-109` once), and two more park the task at `blocked`/`stuck` per
`MAX_CONSECUTIVE_TIMEOUTS` (`packages/agent/src/finalize.ts:60`).

## HARNESS-WRAPPER-112 — dead-spawner false positive, measured: the heartbeats existed

**Filed as:** `[observer] crashed/dead spawner plan-reviewer left HARNESS-WRAPPER-103
working (dead-spawner:plan-reviewer:HARNESS-WRAPPER-103)`.

**Why it landed here:** unchanged from §HARNESS-WRAPPER-23, §HARNESS-WRAPPER-26,
§HARNESS-WRAPPER-98, §HARNESS-WRAPPER-99 and §HARNESS-WRAPPER-111 — the observer
files a `dead-spawner` against the *task* it is watching (`HARNESS-WRAPPER-103`, in
this repo's fleet-db workspace), so the ticket lands in this workspace even though
every line of the defect lives in `orche`. This is the **fifth** recurrence of the
class.

**The observer itself dismissed this signature.** As on §HARNESS-WRAPPER-111, the
verdict was written and the anomaly was filed anyway — so this ticket is a second
instance of the verdict-parse defect bundled in
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md),
not just of the drain defect.

**What is new: the false positive is now measured, not asserted.** Every prior record
in this class argued from source that the observer's cursor lags. This one proves it
from 69 local trace spans for `agent:plan-reviewer:20f348b0-…`. The `agent.run_task`
span is **706,435 ms** and its `onComplete` comment POSTs at **19:57:35.781Z**, fixing
the run window at ≈**19:45:49Z → 19:57:36Z**. Inside that window the agent issued
**17** lock heartbeats at the 40 s interval implied by `heartbeatMs = ttlSeconds*1000/3`
(`spec.ts:628`, `ttlSeconds ?? 120` at `spec.ts:589`) — ≈**680 s of the 706 s run**.
The lock heartbeat and the 30 s screen heartbeat that feeds `lastSeen`
(`SCREEN_HEARTBEAT_MS`, `spawner.ts:109`/`:824-831`) are cleared in the **same**
`finally` (`spawner.ts:1105-1108`), so the 17 lock beats prove `screenBeat` was still
firing every 30 s through 19:56. The digest's claimed 287 s of silence at ≈19:56:05Z
puts the observer's `lastSeen` at ≈19:51:18Z — **roughly five minutes behind the bus**.
Two `dead-spawner` signatures fired in that one digest (this one and
`:worker:HARNESS-WRAPPER-109`), which is systemic view staleness, not coincident
crashes.

**Correction to the record: the assignee guard would NOT have suppressed this one.**
The observer's comment on this ticket claims §-99 Fix #1 would have caught 4 of 5
signatures. It does not catch this one. HARNESS-WRAPPER-103 was `in_progress` and
assigned to `agent:plan-reviewer:20f348b0-…` at file time and *is still*
`status: blocked · assignee: agent:plan-reviewer:20f348b0-…`, so
`task.assignee !== a.facts.agentId` is `false` and the guard is a no-op. Only the
drain fix suppresses this signature. Fix #1 stays sound and worth landing — it is just
not the fix here.

**Actual defect location:** `orche` — `packages/agent/src/observer.ts:242`, the
un-looped, un-limited single `pull()` per 300 s tick, against a backend whose `pull` is
single-page by construction (`packages/queue/src/fleet.ts:256-261`, cap
`MAX_PAGE_LIMIT = 1000` at `fleet.ts:63`). `lastSeen` is a max over *drained* events
(`apps/screen/src/state.ts:346`) and the predicate at `observer.ts:537` compares it to
wall-clock `now`, so what it actually measures is **agent silence + observer lag**.
Secondary: `fileAnomaly` (`observer.ts:816-857`) grounds only on
`DEAD_SPAWNER_TERMINAL_STATUSES` (`observer.ts:111`, `:852`), never on ownership.
Tertiary: `emitScreen` (`spawner.ts:1828-1830`) swallows failed heartbeat publishes
with a bare `.catch(() => {})` — no log, no retry, no counter.

**Confirmed:** none of those files exist here — this is a Go module
(`module github.com/olesho/harness-wrapper`, no `packages/` or `apps/` tree).
`git grep -niE "dead-spawner|fileAnomaly|task_released|agent_stopped|obs-sig|deadSpawnerMs|lastSeen"`
over tracked source, excluding these docs, returns exactly four hits, all in
`pkg/wrapper/session.go:521-557` — a `classifierState.lastSeen` **byte counter** for
PTY output-change detection, unrelated to fleet liveness. No bus, queue, lease,
anomaly detector, or `fileAnomaly` exists in this repo.

**Resolution:** no source change made in this repo — documentation only, and no human
gate requested. The drain-to-empty fix (`observer.ts:242`: loop `pull` → fold →
`ackThrough` until empty, bounded cap, `stderr` warn on cap; gate the `dead-spawner`
scan on **drain state**, never on §-99's rejected event-age guard) is now on its
**fourth consecutive re-derivation** — §-99, §-111, the observer's own comment here,
and this record — and is **still unfiled in `ORCHE`**; `orche list --workspace ORCHE
--limit 500` carries no open observer/drain ticket and ORCHE-130 is closed/merged.
Filing that follow-up is the outstanding action, and no worker in this worktree can
perform it. Full evidence chain, arithmetic and test plan in
[`docs/triage/HARNESS-WRAPPER-112.md`](../../triage/HARNESS-WRAPPER-112.md). Once the
follow-up is filed, the correct disposition for this ticket in this workspace is
**close-as-invalid** — HARNESS-WRAPPER-24 (2026-07-16) already directed that this class
not be re-filed here.

## HARNESS-WRAPPER-113 — same discarded `DISMISS`, and the block-bleed is now confirmed

**Filed as:** `[observer] crashed/dead spawner plan-critic left HARNESS-WRAPPER-102
working (dead-spawner:plan-critic:HARNESS-WRAPPER-102)`.

**Why it landed here:** the same misrouting as §HARNESS-WRAPPER-23,
§HARNESS-WRAPPER-26, §HARNESS-WRAPPER-98, §HARNESS-WRAPPER-99 and
§HARNESS-WRAPPER-111 — the observer files a `dead-spawner` against the *task* it is
watching (`HARNESS-WRAPPER-102`, in this repo's fleet-db workspace), so the ticket
lands in this workspace even though every line of the defect lives in `orche`.

**Fifth recurrence of the class — and the third signature from one reply.** As in
§HARNESS-WRAPPER-111, the observer explicitly dismissed this exact signature and the
dismissal was discarded: it wrote
``**DISMISS `dead-spawner:plan-critic:HARNESS-WRAPPER-102`**`` as the **first** verdict
line of its 2026-07-22T19:59:11Z reply, and the substring matcher at
`packages/agent/examples/observer.ts:181` could not read it past the backtick. That one
reply carried three decorated `DISMISS` lines and produced three tickets —
HARNESS-WRAPPER-113 (`:102`), -112 (`:103`) and -111 (`:109`).

**The `:237` block-bleed §-111 predicted is now measured.** Each of the three tickets'
investigation comments is exactly the reply's suffix from its own verdict line to EOF —
`:109` at 19:59:11.682143Z, `:103` at `.693216Z` (`:103`+`:109`), `:102` at `.702469Z`
(`:102`+`:103`+`:109`). Three strictly nested suffixes is what a block terminator that
never fires produces, and nothing else: `extractInvestigation` finds the block *start*
by `l.includes(sig)` (decoration-tolerant, `:231`) but anchors its terminator at the
line start (`:237`), so decorated verdicts never terminate a block.

**The anomaly itself is false.** `agent:plan-critic:024391fc` posted its two-chunk
critique at 19:52:15.74Z and released; HARNESS-WRAPPER-102 was `open`/`assignee: none`
at file time and is now `implemented` under `agent:integrator:9208258e`. The digest's
`ageMs` of 287 s implies the observer's `lastSeen` was ≈19:51:18Z — it had not drained
events that already existed, the release among them.

**The assignee guard would have suppressed this one.** Unlike -112 (`:103`), where the
assignee never changed and only Patch A or the drain fix helps, HARNESS-WRAPPER-113 is
dropped by HARNESS-WRAPPER-99's still-unlanded Fix #1 on its own — making it the direct
regression anchor for that guard. One correction this triage established for the bundle:
its Patch B companion fixture list is **three** tests, not one (`observer.unit.test.ts`
`:730`, `:777`, `:790`), each of which stubs `getTask` with no `assignee` and each of
which needs `assignee: 'a1'`.

**Actual defect location:** `orche`, re-verified at HEAD `737ea45` —
`packages/agent/examples/observer.ts:181` and `:237` (primary), plus the missing
ownership grounding in `fileAnomaly` (`packages/agent/src/observer.ts:825`/`:852`) and
the un-looped single-page drain (`examples/observer.ts:117`, `src/observer.ts:242`,
`packages/queue/src/fleet.ts:256-261`).

**Confirmed:** none of those files exist here — this is a Go module.
`grep -rniE 'dead-spawner|fileAnomaly|obs-sig|deadSpawnerMs|verdictFor|extractInvestigation' --include='*.go' .`
returns **zero** hits; the only tree-wide matches are this log, the triage records under
`docs/triage/` and the cross-repo bundle.

**Resolution:** no source change made in this repo — documentation only, no human gate
requested. **No second bundle was created**: the existing
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md)
was amended in place with the three-fixture correction, the confirmed `:237` bleed and
the three-signature test shape. Full evidence chain in
[`docs/triage/HARNESS-WRAPPER-113.md`](../../triage/HARNESS-WRAPPER-113.md). Once the
bundle is handed to `orche`, the correct disposition here is **close-as-invalid**, per
the standing human ruling on HARNESS-WRAPPER-24 (2026-07-16).

## HARNESS-WRAPPER-114 — dead-spawner false positive (6th): the deployed observer is a second, untested tick

**Filed as:** `[observer] crashed/dead spawner bug-reviewer left HARNESS-WRAPPER-112
working (dead-spawner:bug-reviewer:HARNESS-WRAPPER-112)`.

**Why it landed here:** unchanged from §HARNESS-WRAPPER-23, §HARNESS-WRAPPER-26,
§HARNESS-WRAPPER-98, §HARNESS-WRAPPER-99, §HARNESS-WRAPPER-111,
§HARNESS-WRAPPER-112 and §HARNESS-WRAPPER-113 — the detector files against the *task*
it is watching (`HARNESS-WRAPPER-112`, in this repo's fleet-db workspace), so the
ticket lands in this workspace even though every line of the defect lives in `orche`.
`grep -rniE 'dead-spawner|fileAnomaly|obs-sig|deadSpawnerMs|task_released|agent_stopped'
--include='*.go' .` returns **zero** hits in this Go module. This is the **sixth**
recurrence of the class and the third in a single evening; as in §HARNESS-WRAPPER-111
and §HARNESS-WRAPPER-113 the observer **explicitly dismissed** the signature in the
same `onComplete` pass and it was filed anyway — the ticket's only comment is that
dismissal, written 4 ms after the ticket itself. The accusation was already false at
file time: the accused bug-reviewer had delivered its triage and released, and the
integrator had merged `117c0b1` ~2 minutes earlier.

**What is new — the hardening has been landing in code the fleet does not run.** The
deployed chain resolves to `makeObserver` in
`packages/agent/examples/observer.ts:62`, a *second*, hand-rolled tick implementation
that never calls `observe()`. It is missing the library's persistence gate
(`packages/agent/src/observer.ts:294-304`), the probe filter and the `ignoreSpawners`
mute knob, and `incidentId` correlation — and it has **zero test coverage** (no test
references `makeObserver` or `examples/observer`; all 35 `observe(` call sites in
`observer.unit.test.ts` drive the library). That is the meta-reason five prior triages
did not stop this.

**Also new — the class now feeds itself.** -114 is the first instance where the accused
agent was a **bug-reviewer triaging an observer-filed ticket**. Each observer bug
occupies a bug-reviewer for a long, quiet turn, which is exactly the shape the
lag-contaminated liveness predicate misreads, so each filed ticket manufactures the
next false positive. Muting the spawner is unavailable: `ignoreSpawners` exists only
in `observe()`.

**Actual defect location:** `orche` — the un-drained single-page `pull`
(`examples/observer.ts:117`, `src/observer.ts:242`,
`packages/queue/src/fleet.ts:256-261`) leaves the bus view ≥ 6 min stale while the
*fleet* view fetched three lines above the filing (`src/observer.ts:825`) is current
and already contradicts the claim; plus the still-unlanded assignee grounding in
`fileAnomaly` (`src/observer.ts:852`), the substring verdict matcher
(`examples/observer.ts:181`), and the missing persistence gate.

**Resolution:** no source change made in this repo — documentation only, and no human
gate requested. The fix is staged additively in the existing bundle
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md)
(new **Patch C**: port the persistence gate to the deployed tick as an exported pure
`gateByPersistence`, so that path becomes testable at all; plus the `in_progress`
*and* reassigned row in Patch B's matrix — the anchor proving the ownership guard
cannot be approximated by a status check). Full evidence chain in
[`docs/triage/HARNESS-WRAPPER-114.md`](../../triage/HARNESS-WRAPPER-114.md). Once the
bundle is handed to `orche`, the correct disposition here is **close-as-invalid** —
HARNESS-WRAPPER-24 (2026-07-16) already directed that this class not be re-filed in
this workspace. Carried forward for a human: the drain-to-empty fix is now on its
**fifth** consecutive re-derivation and is still unfiled in `ORCHE`, and no agent in
this worktree can file there.
