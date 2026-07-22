# HARNESS-WRAPPER-97 — Triage record: real defect, wrong repository (route to `ORCHE`)

**Ticket:** `[observer] release branch behind base (dev..main) — not promoting (release-lag:dev..main)`
**Verdict:** The anomaly is **real** — unlike its sibling [HARNESS-WRAPPER-56](HARNESS-WRAPPER-56.md),
which was a detector false positive. But the defect lives entirely in the **orche** tooling repo
(`/Users/oleh/Work/new/orche`), not in harness-wrapper. No file in this repository participates in
the reported behavior, so this repository receives no code change; this document is the only
deliverable. It records the completed, re-verified triage so the routing decision and the fix plan
survive in the ticket history.

**The `orche`-side fix is specified as a patch bundle:**
[`crossrepo/orche/HARNESS-WRAPPER-118-release-slot-wedge.md`](../../crossrepo/orche/HARNESS-WRAPPER-118-release-slot-wedge.md)
— written at the sixth re-fire of this signature (HARNESS-WRAPPER-118), because five filings of
prose analysis gave no `orche` implementer anything to pick up. It carries Patches A–D (hard slot
deadline; `liveness.maxRunMs` on `release`; watchdog scope + `AbortSignal` into
`HarnessSession.open()`; observable `at_capacity`) and their tests against pinned `orche` file:line
anchors. This record remains the canonical evidence chain for `obs-sig:1bf9fcd2c6`; the amendment
log for each re-fire lives in
[`docs/md/internal/out-of-scope-tickets.md`](../md/internal/out-of-scope-tickets.md).

Two things a worker in this worktree cannot do, and which this record exists to hand off:

1. **Re-file in the `ORCHE` workspace.** A `HARNESS-WRAPPER` worker is spawned in a checkout of the
   Go repo; `packages/agent/src/spawner.ts` and `packages/agent/examples/.orche/agents/release.ts`
   do not exist here. `ORCHE` already carries the sibling tickets ORCHE-14/15/26 on this subsystem.
2. **Restore promotion.** That needs an operator action, not a code change — see
   [Operator actions](#operator-actions-human-only) below. The wedged slot is held by the live
   supervisor, and freeing it kills every in-flight agent in the fleet.

## What was reported

The release cron agent's single concurrency slot is permanently wedged. A cron tick fired at 15:40
never settled its `runTick` promise, so `Spawner.accept()` has rejected every subsequent fire with
`at_capacity`, silently. `main` has had zero promotions since, with no self-recovery path.

## Re-verification at implementation time

Every claim below was independently re-checked against the live fleet and the real orche source
while writing this record — nothing is carried over on trust:

| Claim | Status |
| --- | --- |
| Supervisor still wedged | **Confirmed** — pid 65669, elapsed 08:29:45, running `agent-cli.ts up --workspace HARNESS-WRAPPER` |
| Lag still growing | **Worse** — `git rev-list --count main..dev` on the hub is now **32** (27 when filed). Still zero promotions |
| `main` tip unmoved | **Confirmed** — `6281927`, exactly as filed; `dev` at `656e440` |
| Oldest unpromoted commit | **Confirmed** — `40e7251` (HARNESS-WRAPPER-75, 2026-07-22 18:16) |
| Wedged tick's worktree | **Confirmed** — `agent-release-8df3c501-aa2b-412a-87b7-6d1decccb39e` frozen at mtime 15:40, contains a staged `.claude/` but **no `node_modules`** |
| Stranded release worktrees | **46** currently under `~/.orche/worktrees/` (ticket said 47) |

The worktree evidence is what localizes the hang: a staged `.claude/` means `sandbox.open` *and*
`stageSkills` both completed; a missing `node_modules` means the promotion gate's install stage
never started. The hang is therefore *after* `stageSkills` and at or inside the harness-session
open — the exact call `runAbort` cannot interrupt (defect 4).

**Ruled out:** not a RED gate and not a stale supervisor — no ticket carries the `release-gate-red`
or `release-supervisor-stale` signature labels, consistent with the gate never having run. The cron
*is* firing on schedule.

## Root cause (all in the orche repo)

Four defects compound. #1 is structural; #2–#4 are why nothing above it caught the hang. Line
numbers below were re-read from the current source, not copied from the ticket.

**1. The cron slot has no owner other than the tick promise.** `packages/agent/src/spawner.ts:640-648`:

```ts
if (this.tracked.size >= this.spec.maxConcurrent) return decide('at_capacity', false);
const { settle, runAbort } = this.track(taskId);
void this.runTick(taskId, agentId, runAbort)
  .catch((err) => this.retireOnReject(`tick ${taskId}`, agentId, err))
  .finally(() => settle());
```

`track()` (`spawner.ts:700-715`) inserts into `this.tracked` and returns an idempotent `settle()`
invoked **only** from that `.finally()`. A `runTick` promise that never settles holds the map entry —
and thus the only slot — for the process lifetime. `drain()` and `abortActive()` are shutdown-only
paths; neither runs on a healthy supervisor.

**2. The only wall-clock cap is off by default and unset for `release`.** `LivenessConfig.maxRunMs`
(`spec.ts:268-273`) defaults to `0` (`spec.ts:630`: `maxRunMs: spec.liveness?.maxRunMs ?? 0`), and `0`
disables the watchdog at `spawner.ts:1437` (`if (this.spec.maxRunMs > 0)`). Verified: the release
agent's `defineAgent` block (`release.ts:558-569`) declares **no `liveness` block** — `worker.ts` is
the only agent that sets one. A tick also has no idle/lease watchdog by design (`spawner.ts:1435`:
*"Max-run watchdog only (a tick has no idle/lease semantics)"*), so a release tick has **no timer of
any kind**.

**3. Even when armed, the watchdog covers the wrong span of the tick.** It is installed at
`spawner.ts:1436-1446` — *after* `sandbox.open`, `stageSkills` and `onPrepared` — and cleared in the
`finally` at `spawner.ts:1494-1498`, which runs **before** `active.retrieve()`, `retrieveArtifacts`,
the final transcript capture, and `hooks.onComplete`. For `release`, `onComplete` is where the
*entire* promotion gate runs (`release.ts:570+`) — `npm install` plus a `make test` budgeted at
`DEFAULT_E2E_TIMEOUT_MS = 230 * 60 * 1000` (`release.ts:292`). **The release gate executes wholly
outside the max-run watchdog**, so setting `liveness.maxRunMs` alone would not have bounded a
gate-phase hang either.

**4. `runAbort` cannot interrupt the call that hung.** The watchdog's only action is
`runAbort.abort()` (`spawner.ts:1441`). That signal reaches `session.runPrompt({ signal })` and
`runLaunchSpec(..., runAbort.signal)` — but **not** `HarnessSession.open(...)`
(`spawner.ts:1455-1464`; `harness/session.ts:215` takes no signal). The live evidence puts the hang
at exactly that uninterruptible await. Aborting is therefore not sufficient: the *slot* must be
released independently of whether the run can be cancelled.

**5. The failure is invisible.** `spawner.ts:641` returns `decide('at_capacity', false)` — a span
attribute only. The task-path rejections immediately below it call `this.log(...)`
(`spawner.ts:667`: *"claim … held back (fleet health)"*; `spawner.ts:679`: *"claim … → drop"*). The
cron rejection writes nothing to `agents.log` and emits no bus event. Hours of consecutive
rejections produced zero log lines; the state was discoverable only by hand-reading
`~/.orche/traces.log`.

## Fix plan (for the ORCHE ticket — do not apply here)

**Fix A — make the cron slot self-healing (the real defect).** `spawner.ts:640-648`. Race the tick
against a hard slot deadline that (a) calls `runAbort.abort()`, then (b) after a short grace period
calls `settle()` unconditionally, so no tick can own the slot forever regardless of whether its
promise ever resolves. Log loudly when it fires. Derive the deadline generously — `maxRunMs` when
set, else a multiple of the cron interval — so it can only trip on a genuine wedge, never on a
healthy long gate.

*Trade-off to record in that commit message:* force-settling can let a second tick start while a
wedged first tick's process still exists, weakening `release.ts:564`'s "one writer advances the
release branch". This is safe because promotion is CAS-guarded — a losing concurrent promotion
returns `cas-failed` and defers to the next tick (`release.ts:588-590`, verified) — and a
permanently dead release pipeline is strictly worse than a bounded overlap.

**Fix B — bound the release tick at all.** `release.ts:558-569`: add
`liveness: { maxRunMs: releaseE2eTimeoutMs() + 15 * 60_000 }` (e2e budget + install headroom) so a
watchdog exists for this agent. One line; necessary but **not sufficient** alone (see defects 3/4).

**Fix C — widen the watchdog's coverage.** `spawner.ts:1400-1498`: arm the watchdog **before**
`sandbox.open` (covering setup hangs) and clear it **after** `onComplete` rather than in the
pre-retrieve `finally` (so the release gate runs under it). Keep `killedByWatchdog` feeding
`classifyHarnessStop` unchanged.

**Fix D — make the rejection visible.** `spawner.ts:641`: `this.log(...)` on a cron `at_capacity`
rejection, and escalate after N consecutive rejections (bus event / raised log level) so a wedged
slot surfaces on the dashboard instead of only in `traces.log`.

**Fix E (optional, secondary — not the cause of this incident).** `release.ts:261-281` `runStep`
awaits `Promise.all([new Response(proc.stdout).text(), new Response(proc.stderr).text(),
proc.exited])` while its timeout fires only `proc.kill()` on the direct child. A grandchild
(`go test`, `node`) inheriting the stdout pipe keeps the stream from EOF-ing, so `runStep` can
outlive its own budget. Spawn detached and kill the process *group* on timeout; race the stream
reads against `proc.exited` plus a short grace timer. Include only if the router wants it in the
same change — the wedged worktree has no `node_modules`, so the gate demonstrably never reached
this code.

### Tests (in the orche repo)

- **New `packages/agent/test/spawner-tick-deadline.test.ts`** — the regression test for Fix A. Use
  the `spawner-pause.test.ts:44` subclass idiom (override `runTick` to return a never-settling
  promise, drive `accept()` directly). Assert that after the slot deadline elapses (injected via the
  test spec so it runs in ms) a second `accept()` returns `tick` rather than `at_capacity`,
  `activeCount()` drops to 0, and `runAbort.signal.aborted` is `true`. Without Fix A this test hangs
  at `at_capacity` forever — it is the test that would have caught this bug.
- **`packages/agent/test/spec.test.ts`** — keep the `expect(s.maxRunMs).toBe(0)` default assertion
  (line 68; Fix B is per-agent, not a default change). Add an assertion that `release.ts`'s resolved
  spec has `maxRunMs > 0`, so the release agent can never silently regress to an unbounded tick.
- **`packages/agent/test/release.test.ts`** — extend the existing `describe('release gate budget')`
  invariant to assert `release`'s resolved `maxRunMs` **strictly exceeds** `releaseE2eTimeoutMs()`,
  so the tick cap and the gate budget cannot drift into a watchdog that kills healthy gates.
- **`packages/agent/test/spawner.test.ts`** — for Fix C, assert the watchdog is armed before
  `sandbox.open` (a spec with a tiny `maxRunMs` and a hanging `sandbox.open` must abort) and is still
  live during `onComplete` (a hanging `onComplete` must be cut off).
- **`packages/agent/test/spawner-log-tag.test.ts`** — for Fix D, assert a cron `at_capacity`
  rejection emits a log line tagged with the spawner name.

## Operator actions (human only)

1. **Restart the HARNESS-WRAPPER supervisor (pid 65669)** to free the slot. **This kills every
   in-flight agent in the fleet** — schedule it accordingly. Optionally kick
   `POST http://127.0.0.1:53998/release/fire` afterwards. Until this happens the lag keeps growing;
   it went 27 → 32 commits during triage alone.
2. **Remove the stranded worktrees** `agent-release-8df3c501-aa2b-412a-87b7-6d1decccb39e` (the wedged
   15:40 tick) and `agent-release-3a870b7e-6de6-4a4b-87f5-94f1e1348d25` (the failed 13:40 tick).
3. **File a separate ticket for worktree accumulation** — 46 `agent-release-*` worktrees are
   currently retained, because `cleanup: 'on-success'` (`release.ts:569`) keeps every failed tick's
   worktree. That retention is by design, but its unbounded growth is not tracked anywhere.

   > **Correction (HARNESS-WRAPPER-118, 2026-07-22 23:55).** The `46` above counts glob *entries*,
   > not worktrees: **19 directories + 27 `.pid` sidecars**. The sidecars belong to four supervisors
   > — `93479`×12, `8348`×8, `7802`×5 (the `META-HARNESS` fleet, whose worktrees are `orche`-repo
   > checkouts) and only **2** to `65669`. This workspace's retention is just `agent-release-3a870b7e-…`
   > (failed 13:40 tick) and `agent-release-8df3c501-…` (wedged 15:40 tick), so operator action 2
   > above covers it; the accumulation ticket is a fleet-wide concern, not a HARNESS-WRAPPER one.
