# Ready-to-file `ORCHE` ticket — wedged cron slot in `@orche/agent`

This file **is** the ticket body. It exists because the outstanding blocker on
`obs-sig:1bf9fcd2c6` is not missing analysis — the analysis has been complete since
[`HARNESS-WRAPPER-118-release-slot-wedge.md`](./HARNESS-WRAPPER-118-release-slot-wedge.md) — it is
that **no ticket exists in any workspace to pick the bundle up**, and no agent running in a
`harness-wrapper` worktree may issue `orche` write commands. Sixteen filings have re-derived the
same analysis; none could file it. This turns that human action into one paste.

## How to file it

From anywhere, as a human (not from an agent worktree):

```sh
orche ship \
  --file crossrepo/orche/ORCHE-release-slot-wedge.ticket.md \
  --workspace ORCHE \
  --title "cron slot wedges permanently when a tick never settles (release promoter stopped 19h)" \
  --type bug \
  --priority 1 \
  --label spawner --label cron --label reliability
```

Everything below the `---` is the description to ship. It is written to stand alone in the `orche`
repo: it names its own file/line anchors and needs no `harness-wrapper` context to act on. The code
sketches, apply order and caveats live in the companion bundle, which should be copied into the
`orche` repo (or linked) when the ticket is picked up:
[`HARNESS-WRAPPER-118-release-slot-wedge.md`](./HARNESS-WRAPPER-118-release-slot-wedge.md).

**Anchors below are pinned to `orche` HEAD `737ea45`.** Re-verify line numbers before patching if
HEAD has moved; the bundle carries the same anchors and the same caveat.

---

## Summary

A single `runTick` that never settles owns the sole cron concurrency slot for the lifetime of the
supervisor process. Every subsequent fire is rejected `at_capacity`, **silently**, forever. There is
no timer that can reclaim the slot and no log line that reports the rejection.

Observed in production: the `release` promoter for workspace `HARNESS-WRAPPER` (supervisor pid
65669, `agent-cli.ts up`) last ran a tick at **15:10:03 on 2026-07-22**. Its 15:40:04 tick hung at
or inside `HarnessSession.open()` — it created its worktree and wrote **no transcript at all** — and
has held the slot ever since. ~19 hours later the release branch was 72 first-parent commits (138
total) behind its base, with zero promotions, and the only visible symptom was the *absence* of
`[release@…]` lines in `.orche/run/<ws>/agents.log`. The supervisor process itself is healthy and
its other cron agents (`observer`, 5-minute cadence) never missed a tick.

This is a **stopped promoter, not a diverged branch**: `dev..main` = 0 throughout, a clean
fast-forward. The gate would simply promote if it ran.

**Only a full supervisor restart clears it today** — which kills every in-flight agent in the
workspace. Patch A below makes it self-healing instead.

## Root cause — three defects compose

1. **The slot has exactly one release path.** `packages/agent/src/spawner.ts:640-648`:

   ```ts
   if (this.tracked.size >= this.spec.maxConcurrent) return decide('at_capacity', false);
   const { settle, runAbort } = this.track(taskId);
   void this.runTick(...).catch(…).finally(() => settle());
   ```

   `track()`'s idempotent `settle` (`spawner.ts:700-715`) is invoked *only* by that `.finally()`. A
   `runTick` whose promise never settles therefore owns the sole slot (`maxConcurrent: 1`) for the
   process lifetime.

2. **No timer bounds a tick.** The max-run watchdog is gated on `this.spec.maxRunMs > 0`
   (`spawner.ts:1436-1445`) and `maxRunMs` defaults to **0** (`packages/agent/test/spec.test.ts:68`).
   `packages/agent/examples/.orche/agents/release.ts:558-569` declares no `liveness` block, so it is
   never armed. Even when armed it is installed *after* `sandbox.open()` (`spawner.ts:1404`) and
   cleared at `spawner.ts:1495-1497` — i.e. before the `onComplete` dispatch at `spawner.ts:1214`
   where the promotion gate runs. Both the sandbox setup and the gate sit outside any timer.

3. **The hang point is unabortable.** `HarnessSession.open()`
   (`packages/agent/src/harness/session.ts:215-224`) takes no `AbortSignal`, so `track()`'s
   `runAbort` cannot interrupt the exact call that hung.

Compounding all three: `spawner.ts:641` returns `at_capacity` with no logging, so 30+ dead fires
left no trace any detector reads.

## Reproduction — deterministic, unit level

No sandbox, fleet or wall clock needed. Subclass `Spawner` per the
`packages/agent/test/spawner-pause.test.ts:33-50` idiom, override `runTick` to return a promise that
never resolves, and call `accept()` twice on a `cron` trigger. The second call returns
`decide('at_capacity', false)` — and does so forever.

## The change

Apply in this order. **A is the fix**; the rest are defence in depth and observability.

- **Patch A — hard slot deadline (load-bearing).** `spawner.ts:640-648`. Race `runTick` against an
  injectable deadline. On expiry: `runAbort.abort()` plus a loud `stderr` line; after a grace period
  call the **same** idempotent `settle()` returned by `track()` — do **not** add a separate
  `forceSettle()`, `settle` already does `this.tracked.delete(taskId)` before `resolveDone()`.
  Default the deadline *above* `releaseE2eTimeoutMs()` (the gate alone budgets ~20 min) and pin the
  two against each other by test. Safe for `release` specifically because promotion is CAS-guarded
  (`examples/.orche/agents/release.ts:662` `advanceBaseCAS`, `'cas-failed'` branch at `:673`), so an
  overlapping tick cannot double-promote. With A, the next cron fire recovers promotion **with no
  process restart** — and therefore without killing the fleet.
- **Patch B — arm the watchdog for `release`.** `examples/.orche/agents/release.ts:558-569`: add
  `liveness: { maxRunMs: releaseE2eTimeoutMs() + 15 * 60_000 }`, mirroring `worker.ts:60`. Opt-in
  defence in depth; A must not depend on it.
- **Patch C — watchdog scope.** `spawner.ts`: arm before `sandbox.open()` (`:1404`), clear after the
  `onComplete` dispatch (`:1214`) rather than at `:1495-1497`. Follow-on: thread the run's
  `AbortSignal` into `HarnessSession.open()` (`harness/session.ts:215-224`) — that follow-on is what
  makes *abort* (as opposed to force-settle) work at all. Not a substitute for A's force path, nor
  vice versa.
- **Patch D — make `at_capacity` observable.** `spawner.ts:641`: rate-limited logging plus
  escalation after N consecutive rejections with no intervening successful tick, tagged with the
  spawner name (`packages/agent/test/spawner-log-tag.test.ts` idiom). For a `maxConcurrent: 1` cron
  agent, "rejected on every fire for 3+ hours" is categorically a wedge, not load. Cheapest patch,
  best ratio — it does not fix the wedge, it makes the next one visible in minutes rather than after
  sixteen filings. Land it alongside A.

## Tests

- **`packages/agent/test/spawner-tick-deadline.test.ts`** (new — Patch A). Subclass `Spawner`
  (`spawner-pause.test.ts:33-50` idiom), override `runTick` to never settle, inject a ms-scale
  deadline on a fake clock:
  - *a tick that never settles does not hold the slot past the deadline* — `accept()` ⇒ `tick`;
    advance past deadline + grace; `accept()` ⇒ **`tick`**, not `at_capacity`. Without Patch A this
    assertion never passes. **It is the test that would have caught this bug.**
  - *the run is aborted strictly before the slot is force-released* (`runAbort.signal.aborted`).
  - *`activeCount()` drops to 0 after the force-settle.*
- **`packages/agent/test/spawner-drain.test.ts`** (extend, do not break) — `drain()` must still
  resolve with a force-settled tick outstanding, so a wedged cron slot is not traded for a wedged
  shutdown.
- **`packages/agent/test/spec.test.ts`** (beside `:68`) — pin `release`'s non-zero `maxRunMs` and its
  relationship to `DEFAULT_SLOT_DEADLINE_MS`.
- **`packages/agent/test/spawner-log-tag.test.ts`** (Patch D) — assert the rate-limited line and the
  consecutive-rejection escalation, tagged with the spawner name.

## Acceptance criteria

1. A cron spawner with `maxConcurrent: 1` whose in-flight tick never settles accepts a new tick
   after the deadline + grace elapses, with no process restart.
2. The wedged run's `AbortSignal` is aborted strictly before its slot is released.
3. `drain()` still resolves when a force-settled tick is outstanding.
4. Repeated `at_capacity` rejections with no intervening success are logged and escalate.
5. `release`'s configured `maxRunMs` is non-zero and pinned by test against the slot deadline.

## What this ticket is *not*

`ORCHE-31` and `ORCHE-40` share the `release-lag` signature label but are **closed** and both fixed
only the *detector* (first-parent counting; clamped `oldestUnpromotedMs`). Neither touches the cron
slot. The production numbers above are post-fix numbers — that is what separates this from a
detector artifact.

## Differential control (rules out the alternatives)

Supervisor pid **7802** (workspace `META-HARNESS`) is *older* (started 09:46:47), runs the same
`orche` build `737ea45` and a byte-identical `release.ts` (392 B, md5
`f3f1421393818760af0449c3d9f2133b` — both one-line re-exports of the same on-disk file), and has
never wedged: `main..dev` = 0, release ticks still advancing on cadence. That excludes a `737ea45`
regression, a machine-wide cron/clock/resource fault, a defect in the shared agent definition, and
supervisor ageing. It confirms the premise Patch A is built on: **one** hung tick owning a
process-lifetime slot, not a per-fire crash loop.

## Operator actions this ticket does *not* cover

1. **Restart supervisor pid 65669** (`agent-cli.ts up --workspace HARNESS-WRAPPER`). It is the only
   thing that restores promotion *today*, and it kills every in-flight agent in that workspace — so
   it must be scheduled by a human. Measured five times: the agent dispatched at this signature is
   itself a child of 65669, so it can never issue this. **Do not restart pid 7802** — that is the
   healthy control.
2. **Prune the two dead tick worktrees** — `agent-release-8df3c501-aa2b-412a-87b7-6d1decccb39e`
   (wedged 15:40:04) and `agent-release-3a870b7e-6de6-4a4b-87f5-94f1e1348d25` (failed 13:40:04),
   both retained by `cleanup: 'on-success'` (`release.ts:569`).
3. **Suppress or auto-close `obs-sig:1bf9fcd2c6` at the detector** until Patch A lands, so each
   re-fire stops consuming a triage agent — every such dispatch commits to `dev` and thereby
   increments the very `main..dev` count the signature reports.
