# HARNESS-WRAPPER-99 — Triage record: correct diagnosis, wrong repository (route to `ORCHE`)

**Ticket:** `[observer] crashed/dead spawner plan-critic left HARNESS-WRAPPER-78 working
(dead-spawner:plan-critic:HARNESS-WRAPPER-78)`, escalated to `review`.

**Verdict:** the ticket's root-cause analysis is **correct and independently re-verified**
against real `orche` source — a materially better diagnosis than its predecessors in this
class. But the defect lives entirely in the **orche** tooling repo
(`/Users/oleh/Work/new/orche`), not in harness-wrapper, so this repository receives no code
change and this document is the only deliverable.

**The one substantive correction:** the ticket's own **Fix approach #2** (the freshness
guard) is unsound as specified. It would suppress *genuine* dead-spawner detection in any
fleet whose only working agents are the dead ones — the precise over-suppression failure the
ticket was written to avoid. See [Fix #2 is unsound as
specified](#fix-2-is-unsound-as-specified-must-not-land-verbatim). Fix #1 is sound and should
land; Fix #2 needs redesign before it is implemented.

Two things a worker in this worktree cannot do, and which this record exists to hand off:

1. **Re-file in the `ORCHE` workspace.** A `HARNESS-WRAPPER` worker is spawned in a checkout
   of the Go repo (`module github.com/olesho/harness-wrapper`);
   `packages/agent/src/observer.ts`, `packages/queue/src/fleet.ts`,
   `packages/agent/src/spawner.ts` and `packages/fleet-db/src/types.ts` do not exist here.
   `ORCHE` already carries ORCHE-130, whose guard this extends.
2. **Decide the disposition.** A human directed at HARNESS-WRAPPER-24 (2026-07-16) that this
   class not be re-filed here. Close-as-invalid vs. re-file-against-`ORCHE` is a human call.

## Why it landed here

The observer files a `dead-spawner` anomaly against the *task* it is watching
(`HARNESS-WRAPPER-78`, which lives in this repo's fleet-db workspace), so the ticket lands in
this repo's workspace even though the detector that produced it lives in `orche`. This is the
same misrouting mechanism as
[HARNESS-WRAPPER-23](../md/internal/out-of-scope-tickets.md#harness-wrapper-23--dead-spawner-re-alert-false-positive)
and HARNESS-WRAPPER-26, and it is unchanged by this ticket.

## Confirmed: nothing here participates

This is a Go module. A search of the tree (excluding `node_modules`) for
`spawner|observer|heartbeat` in `*.go` returns only unrelated matches, none of them fleet
machinery:

| Hit | What it actually is |
| --- | --- |
| `internal/env/openshell/openshell.go:266` | "nil defaults to a real **process** spawner" — `exec` wiring |
| `pkg/harness/claude/subagent_test.go:55` | "parented to the spawner" — a Claude subagent session comment |
| `pkg/harness/run.go:78` | an activity-**observer** callback (e.g. a loom daemon IPC heartbeat) |
| `pkg/chat/conversation.go:461`, `cmd/harness-chatd/server.go:418` | "any number of **observers** can inspect a live harness" |
| `pkg/screen/screen.go:5` | screen-change **observers** (turn detectors, gateways) |
| `test/fakeharness/mock/main.go` | a mock harness's `--api-error-heartbeat` flag |

No bus, no fleet queue, no anomaly detector, no `fileAnomaly`, no `obs-sig` labels.

## Re-verification of the ticket's claims against real `orche` source

Every load-bearing claim was independently re-checked at implementation time. All hold.

| Claim | Status |
| --- | --- |
| `tick` drains with a single un-looped, un-limited `pull()` | **Confirmed** — `observer.ts:242`, `await queue.pull(topic, subscriber)`, one call, no loop, no `limit` |
| `FleetQueue.pull` is single-page by construction | **Confirmed** — `fleet.ts:258-260` issues one `pullRaw` and returns `raw.messages`. Contrast `read()` at `fleet.ts:203-215`, which *does* page internally in a `for(;;)` loop |
| Page cap is 1000 | **Confirmed** — `MAX_PAGE_LIMIT = 1000`, `fleet.ts:63` |
| Liveness test compares wall-clock `now` to drained-event `lastSeen` | **Confirmed** — `observer.ts:537`, `now - agent.lastSeen > deadSpawnerMs`; `now = Date.now()` at `observer.ts:266` |
| `lastSeen` is a max over *drained* event timestamps | **Confirmed** — `apps/screen/src/state.ts:345`, `agent.lastSeen = Math.max(agent.lastSeen, evTs)` |
| `deadSpawnerMs` default 270s | **Confirmed** — `observer.ts:223` |
| Heartbeats are timer-driven, not output-driven | **Confirmed** — `spawner.ts:823` fires one immediately after acquire, then `setInterval(…, SCREEN_HEARTBEAT_MS)` at `spawner.ts:824-831`; `SCREEN_HEARTBEAT_MS = 30_000` (`spawner.ts:109`), `TRANSCRIPT_REFRESH_MS = 10_000` (`spawner.ts:114`) |
| `fileAnomaly` re-fetches the live task but never tests ownership | **Confirmed** — `observer.ts:822` fetches; the only `dead-spawner` drop is the ORCHE-130 terminal-status check at `observer.ts:855`, scoped to `DEAD_SPAWNER_TERMINAL_STATUSES = {closed, tombstone}` (`observer.ts:111`) |
| `Issue.assignee` exists, so Fix #1 needs no new I/O | **Confirmed** — `packages/fleet-db/src/types.ts:39`, `assignee?: string` |
| The existing `in_progress` test would break under Fix #1 | **Confirmed** — the fixture at `observer.unit.test.ts:725-736` stubs `getTask` returning `{id, status:'in_progress', labels:[], title:id}` with **no `assignee`**, so the proposed guard drops it and the test fails. The ticket flags this; it is real |

**The ticket's refutation of the filing agent's hypothesis is also correct.** The filing agent
claimed "heartbeats only tick on bus events." They do not — `spawner.ts:824` is a fixed 30s
`setInterval`, independent of model output, cleared only in the run's `finally`
(`spawner.ts:1106`). A long quiet LLM turn is exactly what that timer exists to cover. The gap
is on the consumer side, as the ticket says.

**Live-fleet state was not re-queried.** This task's instructions prohibit running `orche`
commands, so the claim that the accused `agent:plan-critic:625d36c7-…` delivered its review
~3 min before the digest is carried forward from the ticket unverified. It is consistent with
the source-level mechanism and is not load-bearing for the routing decision — but a human
should confirm it before closing HARNESS-WRAPPER-78's sibling as a false positive.

## Root cause (as re-verified)

**Primary — the liveness measure is contaminated by the observer's own bus lag.**
`dead-spawner` is an *absence*-based detector, and absence is unfalsifiable from a lagging
cursor: "I have not seen a heartbeat" is indistinguishable from "I have not yet read the
heartbeats that exist." The lag is structural, not incidental — one un-looped page per 300s
tick (`observer.ts:242`) against a backend whose `pull` is single-page (`fleet.ts:258`). Once
arrivals-per-tick exceed one page, lag grows monotonically and every actively-working agent
progressively reads as dead. The ticket's arithmetic is plausible: ~50 events per agent per
300s tick (30s heartbeat + 30s screen push + 10s transcript refresh) saturates the 1000-message
page at roughly 20 concurrent agents.

Simultaneous firing of three `dead-spawner` signatures in one digest (`:-78`, `:-79`, `:-89`)
against unrelated agents is the signature of a systemic view-staleness fault, not three
coincident crashes.

**Secondary — the pre-file grounding check never tests ownership.** The defining claim of a
`dead-spawner` is *"left agent X working on task T"*. If the live task is no longer assigned to
`facts.agentId`, that claim is false on its face, whatever the status. HARNESS-WRAPPER-78 was
`assignee: none` at file time and the anomaly was filed anyway.

## Fix #1 — ownership grounding (sound; land this)

In `fileAnomaly`, immediately after the ORCHE-130 terminal drop at `observer.ts:855`, using the
already-fetched `task` (no new I/O):

```ts
if (a.kind === 'dead-spawner' && task.assignee !== a.facts.agentId) {
  process.stderr.write(`[observer] dropping released dead-spawner ${sig} — task assignee is now ${task.assignee ?? 'none'}\n`);
  return;
}
```

This is a strict-subset guard, cheap, and would alone have suppressed this ticket. It keys on
`assignee` — **not** `PROGRESSED_STATUSES`, **not** handoff labels. That matters: the one
genuinely wedged sibling (`:-79`) is `in_progress` *with the dead agent still recorded as
assignee*, so it correctly still files. Do not widen to `implemented`-style labels;
`observer.ts:120-130` documents why label-based convergence hid the ORCHE-67 stall.

**Required companion change:** extend the `observer.unit.test.ts:725` fixture with a matching
`assignee`, or it starts failing (verified above).

## Fix #2 is unsound as specified — must not land verbatim

The ticket proposes computing digest freshness as `now - max(ts over windowEvents)` and
**skipping the `dead-spawner` scan entirely** when that exceeds `deadSpawnerMs`. Two defaults
make this actively dangerous:

- `windowMs = 1_800_000` (30 min, `observer.ts:214`)
- `deadSpawnerMs = 270_000` (4.5 min, `observer.ts:223`)

So the genuine detection band for a dead agent is **[4.5 min, 30 min] after its last
heartbeat** — a 25.5-minute window during which the anomaly should fire.

Now consider a fleet where the only `working` agents are the dead ones — a small fleet, a
quiet period, or the very outage the detector exists to catch. `pruneWindow`
(`observer.ts:347-353`) has already dropped everything older than 30 min, so the newest event
in `windowEvents` **is** the dead agent's last heartbeat. Its age is, by definition of the
detection band, greater than `deadSpawnerMs`. The proposed guard therefore skips the scan for
the entire band — and once the band expires the acquire event ages out of the window, the
agent leaves the replayed state, and the anomaly can never fire at all.

**Net effect: the guard blinds dead-spawner detection precisely when every working agent is
dead.** The ticket's own proposed test — "a fresh digest still reports a genuinely stale
working agent" — passes only because it seeds heartbeats *from other agents* at `now - 5s`. It
silently assumes a live cohort keeps the view fresh. Strip that assumption and the guard
suppresses the true positive. This is the HARNESS-WRAPPER-79 masking risk, generalized from one
ticket to the whole detector.

**Redesign direction.** Measure freshness of *the drain*, not of the events. A drain that
returns an empty or short page has provably caught up — the view is current even when the
newest event is old, which is exactly the "everything died" case. Concretely:

- Keep the ticket's drain-to-empty loop in `tick` (`observer.ts:242`): loop `pull` → fold →
  `ackThrough` until a batch returns empty, with a bounded iteration cap and a `stderr` warn
  when the cap is hit. A silent cap reproduces the bug in a new shape. This half is sound and
  is the real fix for the primary root cause.
- Derive the freshness signal from that loop: record whether it reached empty (caught up) or
  bailed on the cap (still lagging), and expose *that* boolean on `ObserverDigest` — not an
  event-timestamp age.
- Skip the `dead-spawner` scan only when the drain did **not** reach empty. An observer that
  drained to empty has a current view by construction, so a stale `lastSeen` then means a
  genuinely stale agent.

Leave count-based detectors (`reopen-loop`, `error-burst`, `backlog`, `role-coverage`,
`release-lag`) untouched — they are not absence-based and stay valid on a lagging window. The
ticket is right about that.

## Test plan (for the `ORCHE` implementer)

In `packages/agent/test/observer.unit.test.ts`, alongside the ORCHE-130 block at lines
725-805, reusing its `seed` / `acquired` helpers and the fake fleet's `f.created`:

- **`dead-spawner` whose task is no longer assigned to the accused agent is dropped** — stale
  `working` agent; re-fetch returns `{status:'open', assignee: undefined}` ⇒ `f.created` empty.
  The direct regression anchor for HARNESS-WRAPPER-99.
- **`dead-spawner` that is `in_progress` AND still assigned to the accused agent still files** —
  the HARNESS-WRAPPER-79 over-suppression guard. Sits next to the existing `in_progress` test,
  whose fixture must gain a matching `assignee`.
- **`dead-spawner` reassigned to a different agent is dropped** —
  `assignee: 'agent:plan-critic:other'` ⇒ not filed.
- **the tick drains a multi-page backlog to empty in one tick** — fake queue returning 2 full
  pages then empty; assert `pull` was called until drained and `windowEvents` holds every
  event. (`packages/queue/test/`, or the `observe` tick tests.)
- **the tick warns and does not skip silently when the drain cap is hit** — fake queue that
  never empties; assert the `stderr` warn fires.
- **a digest whose drain never caught up reports no dead spawners** — replaces the ticket's
  event-age test with the drain-state version.
- **a caught-up digest reports a genuinely stale working agent even when it is the ONLY agent
  on the bus** — no other traffic, single `working` agent whose last heartbeat is `now - 10min`,
  drain reached empty ⇒ that agent **is** reported. This is the test that fails under the
  ticket's Fix #2 as written, and it is the one that must pass.

## Resolution

No source change made in this repository — there is nothing here to change. For the human at
the `review` gate:

1. **Decide the disposition** — close-as-invalid here vs. re-file against `ORCHE` as a
   follow-up to ORCHE-130. HARNESS-WRAPPER-24 (2026-07-16) already directed that this class
   not be re-filed in this workspace.
2. **If re-filed: land Fix #1 alone first.** It is a strict-subset guard, needs no new I/O,
   suppresses this ticket, and provably still files the genuinely wedged `:-79`.
3. **Do not land Fix #2 verbatim.** Land the drain-to-empty loop (sound, and the actual fix for
   the primary cause), but replace the event-age freshness guard with the drain-state signal
   above, or the detector goes blind in exactly the outage it exists to catch.
4. **Confirm the `:-78` / `:-89` false-positive claim against live fleet history** before
   closing them — that part of the ticket could not be re-verified here.
