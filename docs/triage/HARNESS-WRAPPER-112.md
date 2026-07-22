# HARNESS-WRAPPER-112 — Triage record: the heartbeats existed, the observer had not drained them (route to `ORCHE`)

**Ticket:** `[observer] crashed/dead spawner plan-reviewer left HARNESS-WRAPPER-103 working
(dead-spawner:plan-reviewer:HARNESS-WRAPPER-103)`.

**Verdict:** a **false positive, and for the first time in this class a *measured* one.**
The observer claimed `agent:plan-reviewer:20f348b0-62c0-4101-a66b-2a3461207375` had been
`working` with no heartbeat for 287 s. Local trace data shows the agent was mid-run with its
heartbeat timers alive across essentially that entire interval. The observer was roughly five
minutes behind the bus.

The defect lives entirely in the **`orche`** tooling repo (`/Users/oleh/Work/new/orche`), not
in `harness-wrapper`. This repository therefore receives no source change; this record and the
companion entry in
[`docs/md/internal/out-of-scope-tickets.md`](../md/internal/out-of-scope-tickets.md#harness-wrapper-112)
are the only correct deliverables.

**What is new.** [HARNESS-WRAPPER-23](../md/internal/out-of-scope-tickets.md#harness-wrapper-23--dead-spawner-re-alert-false-positive),
HARNESS-WRAPPER-26, [HARNESS-WRAPPER-99](HARNESS-WRAPPER-99.md) and
[HARNESS-WRAPPER-111](HARNESS-WRAPPER-111.md) all *asserted* the false positive from
source-level reasoning about the consumer cursor. This ticket **measures** it: 69 trace spans
for the accused agent, from which the run window, the heartbeat count, and the beat interval
are all directly observable, and the observer's own `lastSeen` is recoverable by arithmetic
from the digest. The mechanism previously argued from the code is now confirmed from data.

## Falsifying trace evidence

From `~/.orche/traces.log`, filtered on
`agent:plan-reviewer:20f348b0-62c0-4101-a66b-2a3461207375` — 69 spans, orche build
`0.1.0+737ea45*`:

| Fact | Value | Source |
| --- | --- | --- |
| `agent.run_task` duration | **706,435 ms** (11 m 46 s); `orche.accept_outcome: claimed`; outcome `success` | trace span |
| `onComplete` comment POST | `plan-reviewer: no revised plan produced.` at **19:57:35.781Z** | HARNESS-WRAPPER-103 comment 1 / trace span 58 |
| ⇒ run window | ≈ **19:45:49Z → 19:57:36Z** | derived (`19:57:35.781Z − 706.435 s`) |
| Lock heartbeats actually issued | **17** × `POST /agent-leases/{id}/heartbeat` (+17 session heartbeats, +18 claim renewals) | trace spans |
| Lock heartbeat interval | **40 s** — `heartbeatMs = ttlSeconds * 1000 / 3` (`spec.ts:628`), `ttlSeconds ?? 120` (`spec.ts:589`) | source |
| ⇒ heartbeat timers alive for | 17 × 40 s ≈ **680 s of the 706 s run** — from acquire to ≈6 s before run end | derived |
| Anomaly's claimed silence | 287 s, digest at ≈ **19:56:05Z** ⇒ observer's `lastSeen` ≈ **19:51:18Z** | ticket + observer comment |

### Why the 17 lock beats settle it

The heartbeat that feeds the observer's `lastSeen` is the **screen** heartbeat, a fixed 30 s
`setInterval` at `spawner.ts:824-831` (`SCREEN_HEARTBEAT_MS = 30_000`, `spawner.ts:109`). It is
set up beside the **lock** heartbeat (`spawner.ts:814-816`) and — this is the load-bearing
detail — both are cleared in the **same** `finally` block (`spawner.ts:1105-1108`).

So the two timers live and die together. The 17 lock beats prove that `finally` had not run
by 19:57:30. Therefore `screenBeat` was still firing every 30 s at 19:51, 19:52, 19:53, 19:54,
19:55 and 19:56 — roughly **ten screen heartbeats** were emitted in the interval the observer
scored as silence — while the observer's `lastSeen` for that agent sat frozen at ≈19:51:18.

The trace shows the timers fired. It does not show every publish landed (see the tertiary cause
below), but on the consumer side the conclusion is unambiguous: the digest's 287 s was not
agent silence.

Corroborating: **two** `dead-spawner` signatures fired in the same digest — this one and
`dead-spawner:worker:HARNESS-WRAPPER-109` (triaged as
[HARNESS-WRAPPER-111](HARNESS-WRAPPER-111.md)). Multiple signatures in one digest against
unrelated agents is the signature of systemic view staleness, not coincident crashes — the same
pattern §-99 recorded with three.

## Root cause

### Primary — the liveness predicate is contaminated by the observer's own consumer lag

Re-verified at `orche` HEAD `737ea45`:

1. **`observer.ts:242`** — `const batch = await queue.pull(topic, subscriber);` — **one call,
   no loop, no `limit`**, once per 300 s tick (`intervalMs = 300_000`, `observer.ts:213`; timer
   at `observer.ts:328`). The cursor is committed at `observer.ts:264`.
2. **`packages/queue/src/fleet.ts:256-261`** — `pull()` issues exactly **one** `pullRaw` and
   returns `raw.messages`. It is single-page by construction; contrast `read()`
   (`fleet.ts:203-215`), which *does* page in a `for(;;)` loop. With `limit` undefined no
   `limit` query param is sent at all (`fleet.ts:853`), so the page is the server default,
   capped by `MAX_PAGE_LIMIT = 1000` (`fleet.ts:63`).
3. **`apps/screen/src/state.ts:346`** — `agent.lastSeen = Math.max(agent.lastSeen, evTs)`, a max
   over **drained** events only.
4. **`observer.ts:537`** — `agent.status === 'working' && agent.currentTaskId && now - agent.lastSeen > deadSpawnerMs`,
   with `now = Date.now()` (wall clock) and `deadSpawnerMs = 270_000` (`observer.ts:223`).

Wall-clock `now` minus a cursor-derived `lastSeen` is not an agent-liveness measure. It is
**`agent silence + observer lag`**. Once per-tick arrivals exceed one page, the lag grows
monotonically and every actively-working agent progressively reads as dead. `dead-spawner` is
an *absence* detector, and absence is unfalsifiable from a lagging cursor: "I have not seen a
heartbeat" is indistinguishable from "I have not yet read the heartbeats that exist."

This is exactly the primary root cause recorded in
[`docs/triage/HARNESS-WRAPPER-99.md`](HARNESS-WRAPPER-99.md) — **still unlanded and still
unfiled in `ORCHE`** (`orche list --workspace ORCHE --limit 500` carries no open observer/drain
ticket; ORCHE-130 is closed/merged). HARNESS-WRAPPER-112 is its **fourth** consecutive
re-derivation.

### Secondary — the pre-file grounding check cannot catch this shape

`fileAnomaly` (`observer.ts:816-857`) re-fetches the task but drops a `dead-spawner` only when
`DEAD_SPAWNER_TERMINAL_STATUSES.has(task.status)` — `{closed, tombstone}` (`observer.ts:111`,
`:852`). It never compares `task.assignee` against `a.facts.agentId`. That ownership guard
(§-99 Fix #1) remains sound and unlanded — but see the correction below: it is **not** the fix
for this ticket.

### Tertiary — heartbeat publishes fail silently (worth filing; not proven causal here)

`emitScreen` (`spawner.ts:1828-1830`) is `void fn(this.screen).catch(() => {})`. A failed bus
publish drops a heartbeat with no log, no retry, and no counter. The trace evidence proves the
*timers* fired; it cannot prove every publish landed. Producer-side loss and consumer-side lag
yield identical symptoms, and distinguishing them requires bus history this worktree cannot
query. The drain fix addresses the far more likely one; the catch-swallow should be instrumented
regardless, so this ambiguity stops recurring.

## Correction to the observer's own comment on this ticket

The observer's comment claims Fix #1 (the assignee guard) "would have suppressed 4 of these 5
signatures". **That does not hold for HARNESS-WRAPPER-112.**

HARNESS-WRAPPER-103 was `in_progress` and assigned to
`agent:plan-reviewer:20f348b0-…` at file time, and *is still*
`status: blocked · assignee: agent:plan-reviewer:20f348b0-…` today. The assignee never changed,
so `task.assignee !== a.facts.agentId` is `false` and the guard is a **no-op** here. Only the
drain fix suppresses this signature.

Fix #1 remains sound and worth landing on its own merits — it is a cheap strict-subset guard
that provably suppresses the §-23/§-26 shapes while still filing the genuinely wedged §-98
shape. It just is not the fix for *this* ticket, and the record should not credit it as one.

## Confirmed: nothing here participates

`go.mod` declares `module github.com/olesho/harness-wrapper`. There is no `packages/` or `apps/`
tree in this checkout, so `observer.ts`, `spawner.ts`, `fleet.ts` and `state.ts` do not exist
here.

- `git grep -niE "dead-spawner|fileAnomaly|task_released|agent_stopped|obs-sig|deadSpawnerMs|lastSeen"`
  over tracked source (excluding the triage/out-of-scope docs) returns exactly **four** hits,
  all in `pkg/wrapper/session.go:521-557` — a `classifierState.lastSeen` **byte counter** used
  for PTY output-change detection. Unrelated to fleet liveness.
- `git grep -nE "spawner|observer|heartbeat" -- '*.go'` returns only: an `exec` process spawner
  (`internal/env/openshell/openshell.go:266`), an activity-observer callback
  (`pkg/harness/run.go:78`), screen-change observers (`pkg/screen/screen.go:5`), a subagent
  comment (`pkg/harness/claude/subagent_test.go:55`), and a mock harness's
  `--api-error-heartbeat` flag (`test/fakeharness/mock/main.go`).

No bus, queue, lease, anomaly detector, or `fileAnomaly` exists in this repository.

## Why it landed here

Unchanged from §-23, §-26, §-98, §-99 and §-111: the observer files a `dead-spawner` against
the *task* it is watching (`HARNESS-WRAPPER-103`, which lives in this repo's fleet-db
workspace), so the ticket lands in this workspace even though every line of the defect lives in
`orche`.

## The `ORCHE` follow-up (cross-repo — a human/router action)

File one follow-up to ORCHE-130 in the `ORCHE` workspace. In priority order:

1. **Drain to empty in `tick`** (`observer.ts:242`) — loop `pull` → fold → `ackThrough` until a
   batch returns empty, with a bounded iteration cap and a `process.stderr.write` warning when
   the cap is hit. A silent cap reproduces the bug in a new shape. **This is the fix for
   HARNESS-WRAPPER-112.**
2. **Gate the `dead-spawner` scan on drain state, not event age** — record whether the loop
   reached empty; skip the scan only when it did not. Do **not** land the event-age freshness
   guard proposed on HARNESS-WRAPPER-99; §-99's analysis shows it blinds the detector in exactly
   the outage it exists to catch. Leave the count-based detectors (`reopen-loop`, `error-burst`,
   `backlog`, `role-coverage`, `release-lag`) untouched — they are not absence-based and stay
   valid on a lagging window.
3. **Ownership grounding (§-99 Fix #1)** — in `fileAnomaly`, after the terminal drop at
   `observer.ts:852`, drop a `dead-spawner` when `task.assignee !== a.facts.agentId`, using the
   already-fetched `task` (no new I/O; `Issue.assignee` exists at
   `packages/fleet-db/src/types.ts:39`). Sound, but **not sufficient for this ticket**.
4. **Instrument `emitScreen`** (`spawner.ts:1828`) — replace the bare `.catch(() => {})` with a
   rate-limited `stderr` warn plus a dropped-publish counter, so producer-side heartbeat loss
   stops being invisible.

**Deterministic reproduction for the implementer:** drive `observe()` with a fake queue whose
`pull` returns one full page while more events remain, and assert the resulting digest reports a
`dead-spawner` for an agent whose *undrained* heartbeats are current.

### Tests to add/update in `packages/agent/test/observer.unit.test.ts`

Fake-queue helpers already exist around lines 512-535.

- **the tick drains a multi-page backlog to empty in one tick** — fake queue returning two full
  pages then empty; assert `pull` is called until drained and `windowEvents` holds every event.
  **The regression anchor for HARNESS-WRAPPER-112**; fails today.
- **the tick warns and does not skip silently when the drain cap is hit** — fake queue that
  never empties; assert the `stderr` warn fires.
- **a digest whose drain never caught up reports no dead spawners.**
- **a caught-up digest still reports a genuinely stale working agent even when it is the ONLY
  agent on the bus** — no other traffic, a single `working` agent whose last heartbeat is
  `now - 10 min`, drain reached empty ⇒ that agent **is** reported. This is the over-suppression
  guard that must pass.
- **`dead-spawner` whose task is no longer assigned to the accused agent is dropped**, and
  **reassigned to a different agent is dropped** — for the ownership guard.
- **Companion change, required:** the existing fixture at `observer.unit.test.ts:725-730` stubs
  `getTask` returning `{id, status:'in_progress', labels:[], title:id}` with **no `assignee`**;
  it starts failing the moment the ownership guard lands. Add a matching `assignee` so the
  "`in_progress` is the live pathology" case keeps filing.

## Resolution

No source change made in this repository — there is nothing here to change, and adding
fleet/lease/observer logic to a Go PTY-supervision library would be a mis-port.

**No escalation requested.** The root cause is fully resolved and now *measured*, the in-repo
change is a safe docs record, and the class's disposition is already governed by a standing
human ruling on HARNESS-WRAPPER-24 (2026-07-16): *do not re-file this class in
`HARNESS-WRAPPER`*. This ticket should be closed once the record lands. The code fix belongs to
the `ORCHE` follow-up above, which no worker in this worktree can file.
