# HARNESS-WRAPPER-114 — Triage record: the deployed observer is a second, untested tick (route to `ORCHE`)

**Ticket:** `[observer] crashed/dead spawner bug-reviewer left HARNESS-WRAPPER-112 working
(dead-spawner:bug-reviewer:HARNESS-WRAPPER-112)`.

**Verdict:** a **false positive — the sixth in this class, and the third in a single evening.**
The accused agent `agent:bug-reviewer:9231a24f-9fad-4481-9895-c4b7a6a39194` had already
finished its only job on HARNESS-WRAPPER-112 and released; by the time the ticket was filed a
successor worker had committed twice and the integrator had merged. In the *same* `onComplete`
pass the observer wrote an explicit **`DISMISS`** for this exact signature — and the ticket was
filed anyway, with that dismissal attached as its own comment 4 ms later.

Every line of the defect lives in **`orche`** (`/Users/oleh/Work/new/orche`), re-verified at
HEAD `737ea45`. This repository receives no source change; this record and the companion entry
in [`docs/md/internal/out-of-scope-tickets.md`](../md/internal/out-of-scope-tickets.md#harness-wrapper-114),
plus an additive amendment to the already-committed cross-repo bundle
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md),
are the only correct deliverables.

## What is new (why this is a record, not a duplicate close)

Two findings are new. Neither appears in
[§-99](HARNESS-WRAPPER-99.md), [§-111](HARNESS-WRAPPER-111.md), [§-112](HARNESS-WRAPPER-112.md)
or §-113.

1. **The deployed observer is not the tested observer.** The fleet runs `makeObserver` from
   `packages/agent/examples/observer.ts` — a *second*, hand-rolled copy of the tick loop that is
   missing the library's persistence gate, probe filter, `ignoreSpawners` escape hatch and
   incident correlation, and that has **zero test coverage**. That is the meta-reason this class
   survives five prior triages: the hardening keeps landing in a path the fleet does not run.
2. **The class now feeds itself.** -114 is the first instance where the accused agent was a
   **bug-reviewer triaging an observer-filed ticket**. Each observer bug consumes a bug-reviewer
   for a long, quiet turn — precisely the shape the lag-contaminated liveness predicate
   misreads — so each filed ticket manufactures the next false positive.

## This ticket is its own reproduction

No fleet-state reconstruction is required: the proof is two artifacts of one `onComplete` pass,
written by the same actor (`agent:observer:2d1bda46`) from the same `reply` string.

| Artifact | Timestamp | Content |
| --- | --- | --- |
| ticket created (`fileAnomaly`, `examples/observer.ts:187`) | 2026-07-22T20:18:03.445769Z | verbatim `renderBugBody` output, `obs-sig:12fbfb2b9d` |
| its only comment (`client.comment`, `:195`) | 2026-07-22T20:18:03.449130Z | opens ``**DISMISS `dead-spawner:bug-reviewer:HARNESS-WRAPPER-112`**`` |

Four milliseconds apart. The investigation that refuted the anomaly is the ticket's own body of
evidence — the §-111 shape, unchanged and still unfixed.

## Falsification of the anomaly itself

From `git log` in this worktree and the fleet-db record. Timestamps normalised to UTC.

| Fact | Value | Source |
| --- | --- | --- |
| accused agent's branch | `agent/HARNESS-WRAPPER-112-9231a24f` sits at base `b67b247` (19:59:59Z), **zero commits of its own** | `git log` |
| accused agent's actual work | triage description + `triaged` label, then release | `-112.updated_at` = 20:11:32Z |
| successor worker `7402b596` | `0f03795` @ 20:13:09Z, `547f460` @ 20:13:43Z on `work/HARNESS-WRAPPER-112` | `git log work/HARNESS-WRAPPER-112` |
| worker comment | *"worker: implemented … @ 547f460d"* @ 20:15:40Z | `-112` comment 1 |
| integrator `362767ce` merged | `117c0b1` @ **20:15:54Z** | `git log agent/HARNESS-WRAPPER-112-362767ce` |
| `-112.assignee` at file time | **`agent:integrator:362767ce`**, status `in_progress` | observer's own live `orche resolve`, same pass |
| claimed silence | 575 s | ticket description |

The merge landed **~2 minutes before the ticket was filed**. The accused agent's branch carries
no commits because it was never supposed to produce any: its job was the triage description and
the `triaged` label, which it delivered before releasing.

### Deriving the observer's `lastSeen`

`575 s` ⇒ the observer's `lastSeen` for the accused agent was `digest_now − 575 s`. `digest_now`
is the `onPrepared` timestamp — the file time minus the Claude investigation run, which the
-111/-112/-113 pass precedent puts at 3–8 min. So

    lastSeen ∈ [20:00Z, 20:05Z]

i.e. at or just after the acquire. Everything that followed was **undrained** at file time:
every 30 s screen heartbeat (`SCREEN_HEARTBEAT_MS = 30_000`, `packages/agent/src/spawner.ts:109`,
`:824-831`, cleared only in the `finally` at `:1106`), the release, the successor's acquire, and
both of its commits.

The observer's **bus** view was ≥ 6 min stale while its **fleet** view — `getTask` at
`packages/agent/src/observer.ts:825`, fetched in the same call that filed the ticket — was
current and already contradicted the claim. That is the whole defect in one sentence.

**Deterministic reproduction in `orche`:** drive `makeObserver`'s hooks (or `observe()`) with a
fake queue whose `pull` returns one full page while more events remain, and assert a
`dead-spawner` is reported — and filed — for an agent whose *undrained* release event already
exists.

## Root cause

Four independent defects. The first three are already on the record; the fourth is new.

### 1. The verdict is read as a strict substring of a markdown-authored reply (recorded: §-111, §-113)

`packages/agent/examples/observer.ts:181`:

```ts
if (reply.includes(`DISMISS ${sig}`)) { … continue }   // suppress
```

The observer wrote ``**DISMISS `dead-spawner:bug-reviewer:HARNESS-WRAPPER-112`**``. A backtick
sits between the space and the signature, the test is `false`, and `fileAnomaly` runs at `:187`.
The prompt's contract is line-oriented (`prompts/observer.md:30`, `:43`, `:49`); the
implementation is character-exact. Fixed by **Patch A** of the committed bundle.

### 2. `extractInvestigation`'s terminator shares the line-shape assumption (recorded: §-113)

`:237` — `/^(CONFIRM|DISMISS) /.test(l.trim())` never matches `**DISMISS …`, while the block
*start* at `:231` uses decoration-tolerant `l.includes(sig)`. -114's reply carried a single
verdict block, so it adds no new evidence here; §-113's three-comment nesting table remains the
proof.

### 3. `fileAnomaly` never tests ownership — and -114 is the discriminating anchor (recorded: §-99 Fix #1, §-111 Patch B, §-113; **still unlanded**)

`packages/agent/src/observer.ts:816` re-fetches the live task at `:825`, but the only
`dead-spawner` drop is the ORCHE-130 terminal-status check at `:852`, scoped to
`DEAD_SPAWNER_TERMINAL_STATUSES = {closed, tombstone}` (`:111`). It never compares
`task.assignee` against `a.facts.agentId` (`:618-622`). `Issue.assignee` already exists
(`packages/fleet-db/src/types.ts:39`) and the task is already in hand — **the guard costs no new
I/O, and the datum that falsifies the claim is fetched three lines above the filing.**

**What -114 adds that -113 does not.** §-113's anchor is `{status:'open', assignee: undefined}`.
-114 is `{status:'in_progress', assignee: 'agent:integrator:362767ce'}` — a *different, live*
owner. That is the row proving the guard must key on `assignee` and **cannot** be approximated
by any status check: the existing test at `packages/agent/test/observer.unit.test.ts:730`
asserts an `in_progress` dead-spawner *still files*, and the comment at
`packages/agent/src/observer.ts:836-841` deliberately forbids applying `PROGRESSED_STATUSES`
here. Same status, opposite correct outcome; only the assignee comparison separates them.

### 4. NEW — the deployed tick has no persistence gate, and no tests

`observe()` (`packages/agent/src/observer.ts:197`) gates filing on a consecutive-tick streak:
`persistTicks = Math.max(1, opts.persistTicks ?? 2)` (`:215`), streak map `:221`, gate
`:294-304`, whose own comment reads *"A one-tick blip never reaches the filing stage."*

`makeObserver` does **not** use `observe()`. It reimplements the tick in `onPrepared`
(`examples/observer.ts:113-167`) over the same detection helpers and files straight from the
first sighting:

```ts
// examples/observer.ts:163-167
const fresh = detectAnomalies(digest, { releaseLagMinMs: releaseLagFloorMs() }).filter(
  (a) => !handled.has(anomalySignature(a)),
);
pending.set(run.agentId, fresh);
writeFileSync(join(run.workdir, 'OBSERVER_DIGEST.md'), renderDigestMd(fresh));
```

`handled` is a *dedup* TTL, not a persistence streak: it suppresses re-investigation of an
**already-filed** signature. Nothing gates the first filing. `grep -n 'persistence\|persistTicks'`
over `examples/observer.ts` returns nothing.

Three sibling divergences confirm this is duplication drift, not a deliberate reduction:

- `isObserverProbe(ev.spawner)` / `ignoreSpawners` ingestion filters exist only in the library
  (`src/observer.ts:212`, `:259`) — so synthetic probe traffic can enter the deployed
  `windowEvents`, and the operator-facing mute knob is unreachable on the path that actually
  runs.
- `incidentId` correlation (`src/observer.ts:311`) is absent: `fileAnomaly(client, a)` is called
  two-arg at `examples/observer.ts:187`, so the two co-firing `dead-spawner` signatures in this
  very digest could not be grouped as one incident.
- The single-page drain at `examples/observer.ts:117` mirrors `src/observer.ts:242`; both are
  un-looped (`packages/queue/src/fleet.ts:256-261`; no `limit` ⇒ server default, `MAX_PAGE_LIMIT
  = 1000` at `fleet.ts:63`).

**And it is untested.** `grep -rln 'examples/observer' packages/agent/test/` → no files;
`grep -rn 'makeObserver' packages/agent/test/` → no hits. All **35** `observe(` invocations in
`observer.unit.test.ts` exercise the library. Patch B lands in `src/` and therefore *does* reach
production through the shared `fileAnomaly`; Patches A and C do not have that luxury — they live
in code no test touches, which is exactly why §-111's bundle already prescribes extracting
`verdictFor` as an exported pure helper.

**Honest scoping of Defect 4.** A persistence gate is a *mitigation*, not a cure. It suppresses
-114 only if the signature is absent on the following tick, which requires the drain to have
caught up. It is strictly complementary to the drain fix; it costs up to 300 s of extra latency
on a *genuine* dead-spawner (a trade the library already made deliberately); and it would have
suppressed every ticket in this class whose lag blip self-cleared within one tick.

## Deployment chain — why `examples/` is the live path, not a sample

Verified in this fleet:

```
$ORCHE_PROJECT_DIR/.orche/agents/observer.ts
  → file:///Users/oleh/Work/new/orche/packages/agent/examples/.orche/agents/observer.ts   (:16-18)
  → makeObserver  (packages/agent/examples/observer.ts:62)
```

with `ORCHE_QUEUE_BACKEND=fleet`, so `queue.pull` resolves to
`packages/queue/src/fleet.ts:256-261`. Cron tick every 300 s
(`examples/observer.ts:102`); hooks `onPrepared` `:113-167` and `onComplete` `:174-200`.

This is the fact that reframes the whole class: five triages have specified fixes against
`src/observer.ts`, and three of the four defects above live in a file the test suite never
loads.

## Confirmed: nothing here participates

`go.mod` declares `module github.com/olesho/harness-wrapper`. There is no `packages/` or `apps/`
tree in this checkout.

```
$ grep -rniE 'dead-spawner|fileAnomaly|obs-sig|deadSpawnerMs|task_released|agent_stopped' \
       --include='*.go' .
0
```

Zero hits across every Go file in the module. No bus, queue, lease, anomaly detector, observer
tick or `fileAnomaly` exists in this repository. Adding fleet/observer logic to a Go
PTY-supervision library would be a mis-port.

## Why it landed here

Unchanged from §-23, §-26, §-98, §-99, §-111, §-112 and §-113: the observer files a
`dead-spawner` against the *task* it is watching (`HARNESS-WRAPPER-112`, which lives in this
repo's fleet-db workspace), so the ticket lands in this workspace even though every line of the
defect lives in `orche`.

## The `ORCHE` deliverable

Specified additively in the existing bundle
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md)
— **Patch C** (persistence gate on the deployed tick, as an exported pure `gateByPersistence`
helper so that path becomes testable at all) plus one added row to Patch B's test matrix (the
-114 anchor: `in_progress` **and** assigned to a different agent ⇒ dropped). Do **not** create a
second bundle.

Out of scope for Patch C, and stated in the bundle: the durable fix is to make `makeObserver`
consume `observe()`'s tick so there is one implementation — which would also restore the probe
filter, `ignoreSpawners` and `incidentId` correlation on the deployed path. That is an
architectural change requiring a human decision on the hook/`observe()` boundary.

**Do not land HARNESS-WRAPPER-99's Fix #2 verbatim** — its event-age freshness guard blinds the
detector in exactly the fleet-wide outage it exists to catch
([§-99](HARNESS-WRAPPER-99.md#fix-2-is-unsound-as-specified--must-not-land-verbatim)).

## Carried forward for a human (not implementable from this worktree)

1. **The drain fix is now on its fifth consecutive re-derivation and is still unfiled in
   `ORCHE`.** Loop `pull` → fold → `ackThrough` until a batch returns empty, at **both**
   `packages/agent/src/observer.ts:242` and `packages/agent/examples/observer.ts:117`, with a
   bounded iteration cap that `stderr`-warns rather than silently truncating (a silent cap
   reproduces this bug in a new shape), and derive the freshness signal from *whether the drain
   reached empty*, not from event age. No agent in this worktree can file in `ORCHE`.
2. **The observer lane is now self-sustaining.** Every observer-filed bug occupies a bug-reviewer
   for a long quiet turn, which is the next `dead-spawner` candidate; this triage run
   (`agent:bug-reviewer:ce19a1c5`, quiet on -114) is the predicted source of
   `dead-spawner:bug-reviewer:HARNESS-WRAPPER-114`. The obvious stopgap — muting the
   `bug-reviewer` spawner — is **unavailable**, because `ignoreSpawners` exists only in
   `observe()` and the fleet runs `makeObserver`. Until Patch B or the drain fix lands, the only
   lever is `ORCHE_SCREEN_HEARTBEAT_MS` / `DEAD_SPAWNER_MS` tuning, which is an operator
   decision.

## Resolution

No source change made in this repository — documentation only, and **no escalation requested**:
the root cause is unambiguous and re-verified against live `orche` source. Disposition for this
ticket in this workspace is **close-as-invalid** once the bundle is handed to `orche`, per the
standing human ruling on HARNESS-WRAPPER-24 (2026-07-16, oleh): *"Do not reopen in
HARNESS-WRAPPER."*

Related records: [§-99](HARNESS-WRAPPER-99.md) · [§-111](HARNESS-WRAPPER-111.md) ·
[§-112](HARNESS-WRAPPER-112.md) · §-113 ·
[bundle](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md).
