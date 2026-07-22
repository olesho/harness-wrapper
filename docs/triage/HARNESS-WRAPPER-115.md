# HARNESS-WRAPPER-115 — Triage record: the accused agent had *finished*, not died (route to `ORCHE`)

**Ticket:** `[observer] crashed/dead spawner bug-reviewer left HARNESS-WRAPPER-113 working
(dead-spawner:bug-reviewer:HARNESS-WRAPPER-113)`.

**Verdict:** a **false positive, disproven by the accused agent's own release span.** The
observer claimed `agent:bug-reviewer:4f756716-5a25-4eed-8675-b85e7a97026f` had been `working` on
HARNESS-WRAPPER-113 with no heartbeat or release for **545 s**. That agent's *entire* recorded
lifetime is **279.76 s**, and it ended with an accepted `POST /issues/{id}/release` → `204` plus
`agent.finalize outcome=success`. The claimed silence is **1.95× the agent's total existence**.

The defect lives entirely in the **`orche`** tooling repo (`/Users/oleh/Work/new/orche`), not in
`harness-wrapper`. This repository therefore receives no source change; this record, the
companion entry in
[`docs/md/internal/out-of-scope-tickets.md`](../md/internal/out-of-scope-tickets.md#harness-wrapper-115),
and **Patch C** added to
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md)
are the only correct deliverables.

**What is new.** This is the sixth consecutive re-derivation of the same defect
([§-23](../md/internal/out-of-scope-tickets.md#harness-wrapper-23--dead-spawner-re-alert-false-positive),
§-26, [§-99](HARNESS-WRAPPER-99.md), [§-111](HARNESS-WRAPPER-111.md),
[§-112](HARNESS-WRAPPER-112.md), and now §-115). §-112 was the first to *measure* the false
positive; §-115 goes one step further and **isolates the causal event**. It is not a
heartbeat-freshness bug at all — it is a *released* agent read as `working`. That distinction
demotes a previously-suspected cause (§-112's tertiary `emitScreen` publish-loss) and is why the
recommended in-repo edit differs from §-112's: the drain fix is promoted from a conditional aside
into a first-class specified patch, because the aside is the mechanical reason six re-derivations
have never landed.

## Falsifying trace evidence

This ticket is its own reproduction — the accused run is fully recorded. From
`~/.orche/traces.log`, filtered on `agent:bug-reviewer:4f756716-5a25-4eed-8675-b85e7a97026f` —
**50 spans**, one `traceId` (`51525075f89eb525f512e0b9f641cff7`), orche build `0.1.0+737ea45*`:

| Fact | Value | Source |
| --- | --- | --- |
| `agent.accept` | `orche.task_id: HARNESS-WRAPPER-113`, `orche.spawner: bug-reviewer`, `accept_outcome: claimed` | trace span |
| `agent.run_task` duration | **279,759.066 ms** (4 m 39.8 s); `executor: headless`; `outcome: success`; `final_status: open` | trace span |
| Lease heartbeats issued | **6** × `POST /agent-leases/{id}/heartbeat` | trace spans |
| Session heartbeats issued | **6** × `POST /agent-sessions/{id}/heartbeat` | trace spans |
| Claim renewals | **7** × `POST /issues/{id}/claim` | trace spans |
| Work products written | 1 × `PATCH /issues/{id}` (the triage brief), 2 × `POST /issues/{id}/labels` (incl. `triaged`) | trace spans |
| **Lease released** | `POST /agent-leases/{id}/release` → **200** | trace span |
| **Task released** | `POST /issues/{id}/release` → **204** | trace span |
| `agent.finalize` | `outcome: success`, `final_status: open` | trace span |
| Anomaly's claimed silence | **545 s** | ticket body |

### The arithmetic that settles it

Let `T0` be run start. The run ended at `T0 + 279.76 s` with an **accepted** release. The
observer computed `now - lastSeen = 545 s`, and `lastSeen` cannot exceed the run's last event, so

    lastSeen ≤ T0 + 279.76

Therefore the digest ran at `≥ T0 + 545` — **at least 265 s after the agent had already released
the task and exited.** The accused agent was not silent for 545 s; it did not exist for 545 s.

The `204` is what makes this stronger than every prior record in the class. It proves the
terminating event was produced *and accepted by fleet-db*. The event existed on the bus; it was
simply still undrained when the observer scored the agent.

Corroborating:

- `persistTicks = 2` (`packages/agent/src/observer.ts:215`) means the signature had to survive
  **two consecutive 300 s ticks**, so the release stayed undrained for ≥ 300 s.
- HARNESS-WRAPPER-113 today reads `status: open`, labels `triaged, implemented` — the
  bug-reviewer lane completed and handed off, exactly as the observer's own comment on this
  ticket states before dismissing it.

## Root cause

### Primary — the `working` status itself is derived from undrained events

1. **`packages/agent/src/observer.ts:242`** — `const batch = await queue.pull(topic, subscriber);`
   — **one call, no loop, no `limit`**, once per 300 s tick (`intervalMs = 300_000`, `:213`;
   `tick()` at `:230`). Cursor committed at `:264`. The deployed example path repeats the
   identical shape at **`packages/agent/examples/observer.ts:117`** (`onPrepared`, `:113`).
2. **`packages/queue/src/fleet.ts:256-261`** — `pull()` issues exactly **one** `pullRaw` and
   returns `raw.messages`; single-page by construction (contrast `read()` at `:203-215`, which
   *does* page). With `limit` undefined no `limit` param is sent, so the page is the server
   default, capped by `MAX_PAGE_LIMIT = 1000` (`fleet.ts:63`).
3. **`apps/screen/src/state.ts:371-384`** — folding `task_released` sets `agent.status = 'idle'`
   (`:378`) **and `delete agent.currentTaskId`**.
4. **`packages/agent/src/observer.ts:537`** —
   `agent.status === 'working' && agent.currentTaskId && now - agent.lastSeen > deadSpawnerMs`,
   with `deadSpawnerMs = 270_000` (`:223`).

Had the `task_released` been drained, **conjuncts 1 and 2 are both false and the age comparison
at `:537` is never reached.** The `545 s` in the ticket title is therefore a red herring: no
heartbeat-freshness tuning can fix this, because the predicate short-circuits before `lastSeen`
is consulted. The detector reported a *released* agent as `working` because the release was still
queued behind an unfinished single-page drain.

### What this instance rules out — correction to §-112's tertiary cause

[HARNESS-WRAPPER-112](HARNESS-WRAPPER-112.md) could not exclude producer-side heartbeat loss via
`emitScreen`'s bare `.catch(() => {})` (`packages/agent/src/spawner.ts:1828-1830`), because
heartbeat-timer evidence cannot prove every publish landed.

**This ticket excludes it.** The causal event here is the *release*, and its `204` proves it
reached fleet-db. Every heartbeat publish in this run could have been silently dropped and the
false positive still would not have occurred, had the release been drained.

`emitScreen` remains worth instrumenting for its own sake — a swallowed publish is a real hole —
but it is **not causal for this shape**, and the record should stop listing it as a candidate
cause for released-agent false positives.

### Secondary — the pre-file grounding check, and why it *does* apply here

`fileAnomaly` (`packages/agent/src/observer.ts:816-857`) re-fetches the task but drops a
`dead-spawner` only when `DEAD_SPAWNER_TERMINAL_STATUSES.has(task.status)` — `{closed,
tombstone}` (`:111`, `:852`). It never compares `task.assignee` against `a.facts.agentId`.

At file time HARNESS-WRAPPER-113 was `in_progress` assigned to `agent:worker:90e906dd-…` —
**not** the accused `agent:bug-reviewer:4f756716-…`. So the
[§-99 Fix #1](HARNESS-WRAPPER-99.md) ownership guard (Patch B in the cross-repo bundle) **would
have suppressed this signature**, giving §-115 a second independent line of defence.

This does **not** contradict §-112, which recorded the guard as a *no-op* for that ticket because
HARNESS-WRAPPER-103's assignee never changed. Both statements hold:

| | §-112 | §-115 |
| --- | --- | --- |
| Assignee at file time | still the accused agent | a **different** agent (`agent:worker:90e906dd-…`) |
| Patch B (ownership guard) suppresses? | **no** — no-op | **yes** |
| Patch C (drain to empty) suppresses? | **yes** | **yes** |

The guard is sufficient for §-115, not for §-112. **Only the drain fix covers both**, which is
why Patch C is the class fix and not an optional extra.

### Escalating: the class now feeds itself

The accused agent is a **bug-reviewer**, accused of dying while triaging HARNESS-WRAPPER-113 —
which was itself a dead-spawner false positive of this same class. A bug-reviewer triage run
measures **279.76 s**, which *exceeds* `deadSpawnerMs = 270_000`.

The lane therefore has **zero slack**: it depends entirely on drain freshness, and any drain lag
converts a healthy triage run into a "dead spawner", whose ticket is then triaged by another
bug-reviewer that becomes the next candidate. Six re-derivations is not coincidence; it is a
closed loop, and it now consumes the very agents assigned to break it.

### Why it keeps being re-derived and never landed

The drain fix has been specified in prose six times in `docs/triage/*.md`. But the artifact
actually designed to be *applied* in `orche` —
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md)
— carried only **Patch A** (verdict parsing) and **Patch B** (ownership grounding). The drain fix
appeared there only as a conditional aside *inside the "Explicitly NOT in this bundle" section*:
"If drain lag is addressed in the same pass, take only the drain-to-empty loop…". It was not a
specified patch, so no implementer picked it up.

**That gap is the mechanical reason this loop has not closed**, and it is the one thing fixable
from this repository. This ticket's load-bearing deliverable is closing it: the bundle now
carries **Patch C — drain to empty** as a first-class patch, listed in its `## Layout`.

## Confirmed: nothing in this repository participates

`go.mod` declares `module github.com/olesho/harness-wrapper`; there is no `packages/` or `apps/`
tree, so `observer.ts`, `spawner.ts`, `fleet.ts` and `state.ts` do not exist here.

`grep -rniE 'dead-spawner|deadSpawnerMs|obs-sig|fileAnomaly|heartbeat' --include='*.go' .`
returns only:

- a mock harness's `--api-error-heartbeat` flag and its `runAPIError` heartbeat loop
  (`test/fakeharness/mock/main.go:12`, `:40-41`, `:80`, `:150`, `:168-171`);
- an activity-observer callback comment (`pkg/harness/run.go:78`).

The four `lastSeen` hits in `pkg/wrapper/session.go:521-557` are a `classifierState.lastSeen`
**byte counter** for PTY output-change detection, unrelated to fleet liveness. No bus, queue,
lease, anomaly detector or verdict parser exists here.

## Fix approach

No Go source change — adding fleet/lease/observer logic to a Go PTY-supervision library would be
a mis-port. The deliverables are three documentation changes:

1. **Amend the cross-repo bundle with "Patch C — drain to empty"** (load-bearing) — promote the
   drain fix out of the conditional aside into a specified patch, listed in `## Layout`, with
   both call sites (`src/observer.ts:242`, `examples/observer.ts:117`), a bounded iteration cap
   plus `stderr` warn, the drain-state gate (never event age), the retained exclusion of §-99's
   Fix #2, the untouched count-based detectors, the Patch B/Patch C coverage table, and the
   `emitScreen` de-listing.
2. **This record** (`docs/triage/HARNESS-WRAPPER-115.md`).
3. **A `## HARNESS-WRAPPER-115` entry** in
   [`docs/md/internal/out-of-scope-tickets.md`](../md/internal/out-of-scope-tickets.md#harness-wrapper-115).

### Tests

**In this repository: none.** There is no code change, and no Go test can exercise a TypeScript
observer that does not exist here. Verification is `harness lint` / `harness docs markdown`
passing and every cited link resolving.

**Specified in the bundle for the `ORCHE` implementer**, in
`packages/agent/test/observer.unit.test.ts` (fake-queue helpers already exist around `:512-535`):

- `the tick drains a multi-page backlog to empty in one tick` — fake queue returning two full
  pages then empty; assert `pull` is called until drained and `windowEvents` holds every event.
  **Fails today.**
- `a dead-spawner is not reported for an agent whose task_released is on a later page` — the
  direct regression anchor for HARNESS-WRAPPER-115: page 1 full of unrelated traffic, page 2
  carrying the accused agent's `task_released`; assert the digest reports **no** dead spawner.
  **Fails today.**
- `the tick warns and does not skip silently when the drain cap is hit` — fake queue that never
  empties; assert the `stderr` warn fires.
- `a digest whose drain never caught up reports no dead spawners.`
- `a caught-up digest still reports a genuinely stale working agent even when it is the ONLY
  agent on the bus` — the over-suppression guard that must pass: no other traffic, one `working`
  agent whose last heartbeat is `now - 10 min`, drain reached empty ⇒ that agent **is** reported.
- `a dead-spawner whose task is assigned to a different agent is dropped` (Patch B) — mirrors
  HARNESS-WRAPPER-115 exactly: accused `agent:bug-reviewer:…`, `task.assignee = agent:worker:…`.
- **Companion fixture fix, required:** the stub at `observer.unit.test.ts:725-730` returns
  `{id, status:'in_progress', labels:[], title:id}` with **no `assignee`**; it starts failing the
  moment Patch B lands. Add a matching `assignee` so the "`in_progress` is the live pathology"
  case keeps filing.

### The deterministic unit reproduction

Drive `observe()` with a fake queue whose `pull` returns one full page while a `task_released`
for the accused agent remains on the next page; assert the digest reports a `dead-spawner` for an
agent that has already released. Fails today; passes under Patch C.

## Outstanding action no worker in this worktree can perform

File the follow-up to **ORCHE-130** in the `ORCHE` workspace carrying **Patches A + B + C**.
Verified again at triage time: `orche list --workspace ORCHE --limit 500` carries **no open
observer/drain ticket**, and ORCHE-130 is `closed/merged/done`.

Until that ticket exists, this class will keep re-filing itself — and, as §-115 demonstrates, it
now does so by accusing the very agents that triage it.

## Disposition

**No escalation requested.** The root cause is unambiguous, measured against live `orche` source
(HEAD `737ea45`) and this fleet's own trace log. Once the `ORCHE` follow-up is filed, the correct
disposition for this ticket in this workspace is **close-as-invalid** — HARNESS-WRAPPER-24
(2026-07-16) already directed that this class not be re-filed in `HARNESS-WRAPPER`.
