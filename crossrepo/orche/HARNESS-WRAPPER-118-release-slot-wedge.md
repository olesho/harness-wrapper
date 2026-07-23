# HARNESS-WRAPPER-118 — cross-repo deliverable for `orche`: the wedged release cron slot

> **This bundle ships a patch, not byte-exact files.** Unlike the sibling
> `crossrepo/meta-harness/` bundle (see [`../meta-harness/APPLY.md`](../meta-harness/APPLY.md)),
> `harness-wrapper` is **not** canonical for any of the source below and holds no copy of it.
> The changes are specified here as diffs-in-prose against pinned `orche` file:line anchors,
> because applying them requires reading the surrounding code — a byte-exact mirror would go
> stale the moment `orche` moves.
>
> **Paths in this file are `orche`-relative** (`/Users/oleh/Work/new/orche`, one checkout per
> reader; do not edit it from a `harness-wrapper` worktree). The work must be committed / PR'd
> **in `orche` under its own ticket** — no such ticket exists today; filing it is an outstanding
> human action (see [Paired ticket](#paired-ticket)).

Triage record and the full evidence chain, in `harness-wrapper`:
[`docs/triage/HARNESS-WRAPPER-97.md`](../../docs/triage/HARNESS-WRAPPER-97.md) — the canonical
record for observer signature `obs-sig:1bf9fcd2c6`, plus the running amendment log in
[`docs/md/internal/out-of-scope-tickets.md`](../../docs/md/internal/out-of-scope-tickets.md)
(section `HARNESS-WRAPPER-97 / -100 / -110 / -116 / -117 / -118 / -119 / -120 / -121 / -122 / -123`).

**This bundle exists because five prior filings produced no patch.** HARNESS-WRAPPER-97, -100,
-110, -116 and -117 all re-derived the same root cause, all correctly concluded "not this repo",
and all stopped at an amendment paragraph. Nothing was ever written down in a form an `orche`
implementer could pick up — the exact failure mode that kept the observer-drain fix unlanded
through seven triages until HARNESS-WRAPPER-115 promoted it to a first-class patch in
[`HARNESS-WRAPPER-111-observer-verdict-parse.md`](./HARNESS-WRAPPER-111-observer-verdict-parse.md)
(§ Patch C: *"so no implementer ever picked it up, and the class re-derived itself seven times"*).
This file is that promotion for the release-slot wedge.

## Why this exists

The `release` cron agent for workspace `HARNESS-WRAPPER` has not run a single tick since **15:10
local on 2026-07-22**. Its 15:40 tick opened a sandbox and never settled. A cron spawner's sole
concurrency slot in `@orche/agent` is released *only* by that tick's own promise settling, and the
release agent has no max-run watchdog — so every later fire is rejected `at_capacity`, silently.
Promotion dev→main is dead for the workspace, and nothing in the fleet reports it.

### Evidence (measured live in the HARNESS-WRAPPER-118 worktree, 2026-07-22 23:49–23:55 local)

1. **The supervisor was never restarted.** `ps -p 65669` →
   `agent-cli.ts up --dir …/harness-wrapper/.orche --workspace HARNESS-WRAPPER`, start
   **Wed Jul 22 11:32:51**, elapsed **12:21**. Same process as at HARNESS-WRAPPER-97 filing time.
2. **Tick cadence stops dead at 15:10.** `…/.orche/run/HARNESS-WRAPPER/agents.log` records release
   ticks on cadence through `cron:release:1784725803650` at **15:10:03 → success**
   ("`release: main is up to date with dev — nothing to release`" — correct at the time:
   `git log --first-parent main..dev --until='2026-07-22 15:10'` is empty). **After that, not one
   `[release@…]` line exists**, while `[observer@…]` ticks continue every 5 minutes to the present.
   The absence is the whole signal: an `at_capacity` rejection logs nothing (Patch D).
3. **The 15:40 tick left a worktree and nothing else.**
   `/Users/oleh/.orche/worktrees/agent-release-8df3c501-aa2b-412a-87b7-6d1decccb39e`, dir mtime
   frozen at **15:40:04**, sidecar `.pid` = **65669**, branch `agent/tick-8df3c501`, HEAD
   `6281927`, `.claude/settings.json` + `hooks/send_last_response.py` staged, `node_modules`
   absent — and **no transcript file** under `…/run/queue/transcripts/HARNESS-WRAPPER/`
   (newest `agent_release_*` is `…5d7d11ee…` at **15:10**). No child process of 65669 survives.
   The tick therefore died **between `sandbox.open()` and the first transcript write**.
4. **The hub would fast-forward if the gate ever ran.** In `/Users/oleh/repos/harness-wrapper.git`:
   `rev-list --count --first-parent main..dev` = **56**, total = **122**, `dev..main` = **0**.
   `main` is still `6281927` (10:15:03 +0200) — **zero promotions in ~13.5 h**. Oldest unpromoted
   commit `40e7251` (18:16:09 +0200, ~**338 min**). The `dev..main` = 0 asymmetry is the proof
   that this is a *stopped promoter*, not a diverged branch: `main` is a clean ancestor.

Lag progression across the six filings: **27 → 32 → 52/57 → 82/41 → 112/51 → 122/56**.

### Re-confirmed at the eighth and ninth filings (HARNESS-WRAPPER-119, -120)

Nothing below revises the analysis — it re-stamps the evidence so a reader can tell the bundle is
still live rather than a historical record. Measured in the HARNESS-WRAPPER-120 worktree,
**2026-07-23 ~01:00 local**:

- Same supervisor, never restarted: **pid 65669**, start Wed Jul 22 11:32:51, elapsed **13:29**.
- `main` still **`6281927`**; `--first-parent main..dev` = **59**, `dev..main` = **0** — still a
  clean fast-forward, still a *stopped promoter*. Oldest unpromoted `40e7251` (2026-07-22 18:16:09).
  **Zero promotions in ~15 h.**
- `agents.log`: the last release tick is line **3780** (`cron:release:1784725803650: success`,
  15:10) and line **3781** its branch sweep — still the final `[release@…]` lines in the file. The
  log has since grown to **4506** and counting, every later line another agent still ticking.
- First-parent lag across all nine filings: **27 → 32 → 57 → 41 → 51 → 56 → 58 → 59**.
- Every `orche` anchor cited in this bundle was re-read against `orche` HEAD and is **unchanged**.
  The one substantive correction found is folded into [Patch A](#patch-a--hard-slot-deadline-load-bearing)
  (`forceSettle()` is redundant — `track()`'s `settle()` already deletes from `this.tracked`).

### Still live at the eleventh filing (HARNESS-WRAPPER-122)

Measured in the HARNESS-WRAPPER-122 worktree, **2026-07-23 02:12 local** — nothing below revises
the analysis; it exists so a reader can tell this bundle is still describing a live wedge:

- Same supervisor, still never restarted: **pid 65669**, start Wed Jul 22 11:32:51, elapsed
  **14:39**. Wedged worktree `agent-release-8df3c501-…` still frozen at mtime **Jul 22 15:40:04**,
  sidecar `.pid` = **65669**, still no transcript for that tick.
- `main` still **`6281927`** — **~16 h, zero promotions**. `--first-parent main..dev` = **61**
  (127 total), `dev..main` = **0**. Oldest unpromoted `40e7251` (2026-07-22 18:16:09), ~476 min.
- `agents.log` (under the supervisor's `--dir`,
  `/Users/oleh/Work/aether/harness-wrapper/.orche/run/HARNESS-WRAPPER/`) has grown to **4542**
  lines while the `[release@…]` count is **still 456**, last two release lines still **3780–3781**.
- First-parent lag across all eleven filings: **27 → 32 → 57 → 41 → 51 → 56 → 58 → 59 → 60 → 61**.
- `orche` is still at HEAD **`737ea45`** — the same commit the -120/-121 re-reads used — and the
  four anchors were spot-checked unchanged: `at_capacity` at `spawner.ts:641`, the sole slot
  release at `spawner.ts:647` (`.finally(() => settle())`), the `maxRunMs > 0` watchdog gate at
  `spawner.ts:1437`, and `release.ts:564` (`maxConcurrent: 1`, no `liveness` block).
- `HarnessSession.open()` is at **`packages/agent/src/harness/session.ts:215`** — there is no
  `packages/agent/src/session.ts`. The Layout section below already used the correct path; the
  abbreviated form in the `harness-wrapper` amendment log has been corrected to match.

### A live differential control — the same agent, unwedged, in a second workspace (new at the twelfth filing, HARNESS-WRAPPER-123)

Measured **2026-07-23 03:25–03:52 local**. This is the first filing to establish a *control*, and it
is the single most useful new fact for whoever implements these patches: a second supervisor is
running the **byte-identical** `release` agent out of the **same** `orche` process image and has
never wedged.

| | HARNESS-WRAPPER (wedged) | META-HARNESS (healthy control) |
| --- | --- | --- |
| Supervisor | pid **65669**, started Wed Jul 22 **11:32:51**, elapsed **16:18** | pid **7802**, started Wed Jul 22 **09:46:47**, elapsed **18:05** — *older* |
| `--dir` | `…/aether/harness-wrapper/.orche` | `…/aether/meta-harness/.orche` |
| Agent definition | `.orche/agents/release.ts`, 392 B, md5 **`f3f1421393818760af0449c3d9f2133b`** | **byte-identical** — same size, same md5 |
| Both re-export | `file:///Users/oleh/Work/new/orche/packages/agent/examples/.orche/agents/release.ts` | same file — one on-disk definition, two supervisors |
| Build tag in `agents.log` | `release@0.1.0+737ea45*` | `release@0.1.0+737ea45*` — same commit |
| Last successful release tick | `cron:release:1784725803650` → **2026-07-22 15:10:03** | `cron:release:1784769929485` → **2026-07-23 03:25:29** |
| Release ticks logged | **456**, frozen since 15:10 | **1396**, still advancing on the 30-min cadence, every recent tick `success` |
| `--first-parent main..dev` | **63** | **0** — fully promoted (`main` `266bf5c`, 2026-07-22 22:56:59) |

**What this rules out.** Not a regression in `737ea45` (the control runs the same commit); not a
machine-wide resource, clock, or cron-scheduler failure (the control fires on schedule through the
whole wedge window); not a defect in the `release` agent definition or its `maxConcurrent: 1`
declaration (the two workspaces share one file, byte for byte); and not supervisor ageing — the
healthy supervisor is the **older** of the two by 1 h 46 m.

**What this confirms.** Exactly the premise Patch A is built on: the failure is **one** tick that
hung and then owned the process-lifetime slot forever, *not* a per-fire crash loop. A repeating
per-tick fault would have taken the control down too. So the trigger was workspace-local state
inside that one `sandbox.open()` at 15:40:04, and the defect that turned a single local hang into
18 hours of dead promotion is the missing deadline at `spawner.ts:640-648` — nothing else.

**Consequences for implementers.**

- **Do not** rework the cron accept path broadly or make `at_capacity` itself the target; on the
  control that path is behaving correctly ~48 times a day. Patch A's per-tick deadline is the
  minimal correct edit, and it is a no-op for a workspace that never hangs.
- The deadline **must** force-`settle()` even when the hung call cannot be aborted. Whatever hung
  at 15:40 was below `HarnessSession.open()` (`harness/session.ts:215`, no `AbortSignal`), so
  Patch C's signal plumbing alone would not have recovered this exact wedge — as the bundle already
  states, C is not a substitute for A's force path.
- Patch D's escalation is what makes the two columns of the table above distinguishable **from the
  log alone**. Today the only difference visible in `agents.log` is the *absence* of `[release@…]`
  lines in one file and their presence in the other — a difference no detector reads, which is why
  this signature survived eleven filings with no operator ever seeing a cause.
- The control also bounds the blast radius of the operator restart: only pid **65669** needs
  restarting. **Do not restart pid 7802** — META-HARNESS is healthy, and restarting it would kill
  its in-flight agents for nothing.

### Deterministic reproduction (unit level, in `orche`)

Subclass `Spawner` per the `packages/agent/test/spawner-pause.test.ts:33-50` idiom, override
`runTick` to return a promise that never resolves, and drive `accept()` twice on a `cron` trigger:
the second call returns `decide('at_capacity', false)` **forever**. No clock advance, no sandbox,
no fleet needed.

### Not a detector artifact

`ORCHE-31` and `ORCHE-40` are both **closed** and both fixed only the *detector* (first-parent
commit counting; the clamped `oldestUnpromotedMs`). The counts above are post-fix values, which is
what separates this signature from [HARNESS-WRAPPER-56](../../docs/triage/HARNESS-WRAPPER-56.md),
where the same `release-lag:dev..main` signature *was* a mis-aging artifact.

## Layout

    packages/agent/src/spawner.ts                     Patch A — hard slot deadline at :640-648
                                                      Patch C — watchdog scope (:1404, :1214, :1495-1497)
                                                      Patch D — observable at_capacity at :641
    packages/agent/examples/.orche/agents/release.ts  Patch B — arm liveness.maxRunMs at :558-569
    packages/agent/src/harness/session.ts             Patch C follow-on — open() takes an AbortSignal (:215-224)
    packages/agent/test/spawner-tick-deadline.test.ts new — Patch A regression test
    packages/agent/test/spec.test.ts                  Patch B — pin release's non-zero maxRunMs (beside :68)

## Root cause — three defects compose

None of them exists in `harness-wrapper`: a tree-wide `grep` for `runTick` / `maxRunMs` /
`at_capacity` over `*.go` returns **zero** hits, and the only tree-wide matches at all are
`docs/triage/HARNESS-WRAPPER-97.md` and `docs/md/internal/out-of-scope-tickets.md`.

**1. The slot has exactly one release path — the tick promise.** `packages/agent/src/spawner.ts:640-648`:

```ts
if (this.tracked.size >= this.spec.maxConcurrent) return decide('at_capacity', false);
const { settle, runAbort } = this.track(taskId);
void this.runTick(taskId, agentId, runAbort).catch(…).finally(() => settle());
```

`track()` (`spawner.ts:700-715`) registers the entry and hands back an idempotent `settle()` that
*only* the `.finally()` above calls. A `runTick` that never settles owns the sole slot
(`maxConcurrent: 1`) for the process lifetime.

**2. No timer bounds a tick.** The max-run watchdog is conditional on `this.spec.maxRunMs > 0`
(`spawner.ts:1437-1445`), and `maxRunMs` defaults to **0** (asserted in
`packages/agent/test/spec.test.ts:68`). The release agent declares no `liveness` block
(`packages/agent/examples/.orche/agents/release.ts:558-569`; only
`examples/.orche/agents/worker.ts:60` sets one), so the watchdog is never armed. Worse, even when
armed it is installed **after** `sandbox.open()` (`spawner.ts:1404`) and cleared in the `finally`
at `spawner.ts:1495-1497` — i.e. *before* `onComplete` runs at `spawner.ts:1214`, and `onComplete`
is where the entire promotion gate lives (`release.ts:570-680`, including the ~20-minute
`runGate`). Both the sandbox setup and the gate sit outside any timer.

**3. The hang point is unabortable.** The tick died at or just inside `HarnessSession.open()` /
the first `runPrompt` (evidence item 3 above). `HarnessSession.open()`
(`packages/agent/src/harness/session.ts:215-224`) takes no `AbortSignal`, so the `runAbort`
controller from `track()` cannot interrupt it even if a watchdog fired.

Net effect: one hung `HarnessSession.open()` at 15:40 permanently disables dev→main promotion for
the workspace, silently, with no self-healing path.

## Patch A — hard slot deadline (load-bearing)

In `packages/agent/src/spawner.ts:640-648`. Race `runTick` against a deadline. On expiry:
`runAbort.abort()`, write a loud `stderr` line naming the tick id and elapsed time, then — after a
grace period — force-`settle()`, so a tick wedged in unabortable native code cannot hold the slot
forever.

```ts
// A tick that never settles owns the sole slot for the process lifetime
// (HARNESS-WRAPPER-118: one hung HarnessSession.open() froze dev→main promotion
// for ~13.5 h, silently). Abort first; if the tick is wedged somewhere abort
// cannot reach, reclaim the slot anyway after a grace period.
const { settle, runAbort } = this.track(taskId);
const started = this.now();
const deadline = this.spec.slotDeadlineMs ?? DEFAULT_SLOT_DEADLINE_MS;
const timer = setTimeout(() => {
  process.stderr.write(
    `[${this.spec.name}] tick ${taskId} exceeded slot deadline ${deadline}ms ` +
      `(elapsed ${this.now() - started}ms) — aborting\n`,
  );
  runAbort.abort();
  grace = setTimeout(() => {
    process.stderr.write(
      `[${this.spec.name}] tick ${taskId} did not settle ${SLOT_GRACE_MS}ms after abort — ` +
        `force-releasing the slot; the tick may still be running\n`,
    );
    settle();   // the SAME idempotent settle() from track() — see the note below
  }, SLOT_GRACE_MS);
}, deadline);
void this.runTick(taskId, agentId, runAbort)
  .catch(…)
  .finally(() => { clearTimeout(timer); clearTimeout(grace); settle(); });
```

Requirements on the implementation:

- **`settle()` must stay idempotent.** The force path and the natural path can both fire; the
  second is a no-op. `track()` already returns an idempotent `settle` — preserve that and assert it.
- **No separate `forceSettle()` is needed** *(correction folded in at HARNESS-WRAPPER-120, verified
  against `orche` HEAD)*. Earlier revisions of this bundle asked the force path to "settle() **and**
  remove the entry from `this.tracked`" as if those were two steps. They are one: `track()`'s
  `settle` (`spawner.ts:707-713`) already does `this.tracked.delete(taskId)` *before* `resolveDone()`,
  guarded by its `settled` flag. So the deadline path calls the **same closure** the `.finally()`
  calls — which both frees the slot and lets `drain()` resolve, since `drain()` (`spawner.ts:529-543`)
  snapshots `this.tracked` and awaits each entry's `done`. Adding a second private method would
  duplicate the delete and give the `settled` flag a second owner. **The requirement that survives is
  the test, not the method:** `drain()` must still resolve with a force-settled tick outstanding —
  otherwise a wedged cron slot is traded for a wedged shutdown. `spawner-drain.test.ts` covers the
  existing contract; extend it, do not break it.
- **The deadline must be injectable** (spec field or constructor option) so the regression test can
  run in milliseconds on a fake clock.
- **Both log lines are load-bearing**, not decoration. A silent force-settle reproduces this bug in
  a new shape: the slot recycles, the tick is still wedged, and nobody knows.

**Safe for `release` specifically.** Promotion is CAS-guarded: `advanceBaseCAS`
(`release.ts:662`) with an explicit `'cas-failed'` = *"another tick won the race"* branch at
`:673`. An overlapping tick therefore cannot double-promote — the loser observes the CAS failure
and exits. This is what makes reclaiming the slot under a possibly-still-live tick acceptable here.

**Choose the default deadline above the gate budget**, not below it, or the watchdog kills healthy
gates: `release`'s `runGate` alone budgets ~20 min. `DEFAULT_SLOT_DEADLINE_MS` should exceed
`releaseE2eTimeoutMs()` with margin (see Patch B), and the two must be pinned against each other by
a test so they cannot drift.

## Patch B — arm the watchdog for `release`

At `packages/agent/examples/.orche/agents/release.ts:558-569`, add a `liveness` block to
`defineAgent`, mirroring `examples/.orche/agents/worker.ts:60`:

```ts
defineAgent({
  name: 'release',
  maxConcurrent: 1,
  liveness: { maxRunMs: releaseE2eTimeoutMs() + 15 * 60_000 },  // gate budget + slack
  trigger: cron({ intervalMs: 30 * 60 * 1000, … }),
  …
})
```

**Defence in depth only.** Patch A must not depend on this: `maxRunMs` is opt-in per agent, so
every *other* unbounded cron agent stays exposed until A lands. And as Patch C explains, the
watchdog as currently scoped would not have fired for this wedge anyway — it is installed after
`sandbox.open()`, and the hang was at or before it.

## Patch C — watchdog scope

Arm the tick watchdog **before** `sandbox.open()` (`spawner.ts:1404`) and clear it **after** the
`onComplete` dispatch (`spawner.ts:1214`) — not in the harness-block `finally` at
`spawner.ts:1495-1497`. Two windows are currently unguarded and both matter:

| Window | Currently guarded? | Why it matters here |
| --- | --- | --- |
| `sandbox.open()` → harness block | **no** — armed after `:1404` | the 15:40 tick died in this window |
| `onComplete` (`:1214`) | **no** — cleared at `:1495-1497`, before dispatch | the entire promotion gate (`release.ts:570-680`) runs here |

**Follow-on, required for abort to actually work:** `HarnessSession.open()`
(`packages/agent/src/harness/session.ts:215-224`) should accept the run's `AbortSignal` and thread
it into the process spawn / connect it drives. Without it, the abort in Patch A cannot reach the
exact call that hung here — which is precisely why Patch A also needs its force-settle grace path.
Do not treat the signal plumbing as a substitute for the force path, or vice versa.

## Patch D — make `at_capacity` observable

`spawner.ts:641` returns `decide('at_capacity', false)` **silently**. That silence is why this
wedge survived six observer filings with no operator ever seeing a cause: `agents.log` contains no
trace of the 15:40+ rejections, only the *absence* of release lines, which no detector reads.

- Log cron `at_capacity` rejections, rate-limited (e.g. first occurrence, then once per N or per
  time window) so a busy `worker` spawner at legitimate capacity does not flood the log.
- Escalate after **N consecutive** rejections for the same spawner with no intervening successful
  tick — a `stderr` warn naming the spawner, the consecutive count, and the age of the tick that
  holds the slot. For a `maxConcurrent: 1` cron agent, "rejected every fire for 3+ hours" is
  categorically a wedge, not load.
- Tag the line with the spawner name so `spawner-log-tag.test.ts`'s existing idiom applies.

With Patch D, the next wedge surfaces in `agents.log` within minutes instead of after six observer
filings and ~13.5 h of dead promotion.

## Tests to add

### `packages/agent/test/spawner-tick-deadline.test.ts` (new — Patch A)

Use the `spawner-pause.test.ts:33-50` subclass idiom: subclass `Spawner`, override `runTick` to
return a never-settling promise, drive `accept()` directly. Inject a millisecond-scale deadline via
the test spec and drive a fake clock.

- **`a tick that never settles does not hold the slot past the deadline`** — `accept()` once
  (returns `tick`), advance past deadline + grace, `accept()` again ⇒ **`tick`**, not
  `at_capacity`. *Without Patch A this assertion never passes — the second `accept()` returns
  `at_capacity` forever.* This is the test that would have caught the bug.
- **`the run is aborted before the slot is force-released`** — assert `runAbort.signal.aborted` is
  `true` at the moment of force-settle, and that abort happens strictly before the grace expiry.
- **`activeCount() drops to 0 after the force-settle`**.
- **`drain() still resolves with a force-settled tick outstanding`** — the shutdown-hang guard.
  Pair it with the existing `spawner-drain.test.ts` contract.
- **`settle() is idempotent across the forced and natural paths`** — let the wedged promise resolve
  *after* the force-settle and assert the slot count does not go negative and no second release is
  observed.
- **`a tick that finishes inside the deadline is never aborted`** — the over-suppression guard:
  assert `runAbort.signal.aborted` stays `false` and no `stderr` line is written.

### `packages/agent/test/spec.test.ts` (Patch B)

- Keep the existing `expect(s.maxRunMs).toBe(0)` default assertion at `:68` — Patch B is per-agent,
  not a default change.
- Add: the resolved `release` spec has **`maxRunMs > 0`**, so the release agent cannot silently
  regress to an unbounded tick.
- Add (or extend `release.test.ts`'s `describe('release gate budget')`): `release`'s resolved
  `maxRunMs` **strictly exceeds** `releaseE2eTimeoutMs()`, so the tick cap and the gate budget
  cannot drift into a watchdog that kills healthy gates.

### `packages/agent/test/spawner.test.ts` (Patch C)

- A spec with a tiny `maxRunMs` and a **hanging `sandbox.open`** must abort — proves the watchdog is
  armed before `:1404`.
- A **hanging `onComplete`** must be cut off — proves the watchdog is still live at `:1214`.

### `packages/agent/test/spawner-log-tag.test.ts` (Patch D)

- A cron `at_capacity` rejection emits a log line tagged with the spawner name.
- N consecutive rejections escalate exactly once, not N times (rate limiting holds).

## Apply

There is no script: read each anchor, apply the change, run the suite.

    cd "$ORCHE_DIR"            # /Users/oleh/Work/new/orche
    # Patch A: packages/agent/src/spawner.ts:640-648      (deadline race + abort + force-settle via
    #                                                      track()'s own idempotent settle())
    # Patch B: packages/agent/examples/.orche/agents/release.ts:558-569  (liveness.maxRunMs)
    # Patch C: packages/agent/src/spawner.ts:1404         (arm before sandbox.open)
    #          packages/agent/src/spawner.ts:1214,1495-97 (clear after onComplete, not in the finally)
    #          packages/agent/src/harness/session.ts:215  (open() accepts the run AbortSignal)
    # Patch D: packages/agent/src/spawner.ts:641          (rate-limited log + consecutive escalation)
    pnpm vitest run packages/agent/test/spawner-tick-deadline.test.ts \
                    packages/agent/test/spawner-drain.test.ts \
                    packages/agent/test/spawner.test.ts \
                    packages/agent/test/spawner-log-tag.test.ts \
                    packages/agent/test/spec.test.ts

Acceptance: a never-settling `runTick` no longer blocks the next `accept()` past the deadline, the
run is aborted before the slot is reclaimed, `drain()` still resolves (A); the `release` spec
resolves a non-zero `maxRunMs` that exceeds the gate budget (B); a hang in `sandbox.open` and a
hang in `onComplete` are both cut off (C); a cron `at_capacity` rejection is visible in the log and
escalates after N consecutive fires (D).

## Paired ticket

**No `ORCHE` ticket covers Patches A–D today.** `ORCHE-31` and `ORCHE-40` — the sibling tickets
sharing this signature label — are both **closed** and both fixed only the *detector*; neither
touches the wedged cron slot. Filing the `ORCHE` ticket that carries this bundle is the outstanding
action, and **no worker in a `harness-wrapper` worktree can perform it** (an agent here must not
issue `orche` write commands).

Priority if only part ships:

- **Patch A is the fix.** It is the only patch that makes the wedge self-healing, and it is
  independent of every other one. If exactly one thing lands, land A.
- **Patch D is the cheapest and has the best ratio** — it does not fix the wedge, it makes the next
  one visible in minutes instead of six filings. Land it alongside A.
- **Patch C** closes the two unguarded windows and is what makes any watchdog meaningful for this
  agent; its `session.ts` follow-on is what makes *abort* (as opposed to force-settle) work at all.
- **Patch B** is per-agent defence in depth. It is the weakest of the four on its own: it is opt-in,
  and as scoped today the watchdog would not have fired for this wedge anyway.

## Out of scope for this bundle — human/operator actions

1. **Restart supervisor `pid 65669`.** It is the **only** thing that restores promotion now. It
   kills every in-flight fleet agent (including any agent implementing this ticket), so it must be
   scheduled by a human. Optionally follow with `POST http://127.0.0.1:53998/release/fire`.
2. **File the `ORCHE` ticket** carrying Patches A–D (see above).
3. **Prune the two dead HARNESS-WRAPPER tick worktrees** —
   `agent-release-8df3c501-aa2b-412a-87b7-6d1decccb39e` (wedged 15:40) and
   `agent-release-3a870b7e-6de6-4a4b-87f5-94f1e1348d25` (failed 13:40), retained by
   `cleanup: 'on-success'` (`release.ts:569`). Note the corrected accounting: the
   `/Users/oleh/.orche/worktrees/agent-release-*` glob matches **46 entries = 19 directories + 27
   `.pid` sidecars**, and those sidecars belong to **four** supervisors — `93479`×12, `8348`×8,
   `7802`×5 (the `META-HARNESS` fleet, whose worktrees are `orche`-repo checkouts, not this
   project's) and only **2** to `65669`. Earlier filings reported "46 retained worktrees" for this
   workspace; the HARNESS-WRAPPER retention is just the two named above.

**Until the supervisor is restarted or Patch A lands in `orche`, `obs-sig:1bf9fcd2c6` re-fires on
every observer sweep regardless of what merges in `harness-wrapper`.** No change in this repository
— of any kind — can affect it. This bundle is the deliverable; the amendment log in
[`docs/md/internal/out-of-scope-tickets.md`](../../docs/md/internal/out-of-scope-tickets.md)
is the record of how many times we rediscovered it before writing it down.
