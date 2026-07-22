# HARNESS-WRAPPER-113 — Triage record: the observer's own `DISMISS` was discarded again, and the block-bleed predicted by §-111 is now measured

**Ticket:** `[observer] crashed/dead spawner plan-critic left HARNESS-WRAPPER-102 working
(dead-spawner:plan-critic:HARNESS-WRAPPER-102)`.

**Verdict:** a **false positive**, produced by the same defect as
[HARNESS-WRAPPER-111](HARNESS-WRAPPER-111.md) on a sibling signature — and, unlike its
predecessors, one where **three independent suppressions each would have been sufficient** on
their own. That makes HARNESS-WRAPPER-113 the cleanest regression anchor in this class. It also
supplies the first *empirical* confirmation of the second defect site §-111 could only predict
from source inspection (`HARNESS-WRAPPER-111.md:74-77`).

The defect lives entirely in the **`orche`** tooling repo (`/Users/oleh/Work/new/orche`, HEAD
`737ea45` at triage time), not in `harness-wrapper`. This repository therefore receives no source
change; the deliverables are this record plus an in-place correction to the already-committed
bundle
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md).
**No second bundle was created** — the fix is the same fix.

## This ticket is its own reproduction

No fleet-state reconstruction is needed: the proof is three artifacts of a *single* `onComplete`
pass (`packages/agent/examples/observer.ts:174-200`, cron tick every 300 s at `:102`).

One observer reply at 2026-07-22T19:59:11Z carried three decorated verdict lines, in this order:

```
**DISMISS `dead-spawner:plan-critic:HARNESS-WRAPPER-102`**
…
**DISMISS `dead-spawner:plan-reviewer:HARNESS-WRAPPER-103`**
…
**DISMISS `dead-spawner:worker:HARNESS-WRAPPER-109`**
…
---
Two things worth a human's attention …
```

All three signatures were filed as tickets anyway — HARNESS-WRAPPER-113, -112 and -111
respectively (`obs-sig:5667632197`, `obs-sig:1b1aa379a4`, and -111's). **That is defect (1)
below, demonstrated three times in one pass.**

### The three-comment nesting proof — defect (2), measured

Each ticket's `observer investigation:` comment is exactly the **suffix of the reply running from
its own verdict line to EOF**:

| Ticket | Comment ts | Comment content |
| --- | --- | --- |
| HARNESS-WRAPPER-111 (`:109`) | 19:59:11.682143Z | `:109` block + trailing prose |
| HARNESS-WRAPPER-112 (`:103`) | 19:59:11.693216Z | `:103` + `:109` blocks + trailing prose |
| **HARNESS-WRAPPER-113 (`:102`)** | 19:59:11.702469Z | `:102` + `:103` + `:109` blocks + trailing prose |

Three strictly nested suffixes is what a block scan whose **terminator never fires** produces,
and nothing else. If the terminator at `:237` had matched even once, the `:102` and `:103`
comments would have been truncated at the following verdict line. §-111 predicted this bleed from
source inspection alone (`docs/triage/HARNESS-WRAPPER-111.md:74-77`); these three timestamped
artifacts measure it.

### Falsification of the anomaly itself

From `orche resolve fleet-db://HARNESS-WRAPPER-102` (comment list):
`agent:plan-critic:024391fc-debd-4059-9525-f42ceb9296f9` posted its full two-chunk critique at
**19:52:15.741249Z / 19:52:15.743223Z** and released. The task was **`open` / `assignee: none`**
at file time, and is today `in_progress`/`implemented` under
`agent:integrator:9208258e-7f16-4aa5-85dd-68de60761368`. Work advanced normally; nothing was ever
wedged.

The digest (≈19:56:05Z) reported `ageMs` 287 s, which implies the observer's `lastSeen` for that
agent was ≈19:51:18Z — i.e. it had not yet drained events that already existed, the release among
them. A drained-to-empty observer would have seen the release and never raised the anomaly at
all.

## Root cause

All in `orche`, all re-verified at HEAD `737ea45` at triage time. **Four defects; the first
three are each independently sufficient to have suppressed this ticket.**

### 1. Primary — the verdict is read as a strict substring of a markdown-authored reply

`packages/agent/examples/observer.ts:181`:

```ts
if (reply.includes(`DISMISS ${sig}`)) { … continue }   // suppress
```

The observer wrote ``**DISMISS `dead-spawner:plan-critic:HARNESS-WRAPPER-102`**``. A backtick
sits between the space and the signature, so the substring test is `false`, the suppression
branch is skipped, and `fileAnomaly` runs at `:187`.

The prompt's contract is **line-oriented** — `packages/agent/examples/prompts/observer.md:30`
("write a verdict line for each signature"), `:43` (`DISMISS <signature>`), `:49` ("Quote each
`<signature>` **exactly**"). The implementation is **character-exact**. `**bold**`, a code span,
`- `, `> ` and `## ` all satisfy the stated contract and defeat the implementation; none of them
is forbidden by the prompt.

The "a parse miss errs toward filing" default is deliberate (`:170-173`) and is right for
**silence**. It is inverted here: the strongest possible signal that a ticket should not exist is
the exact input that produces it.

### 2. Same defect, second site — now confirmed, not predicted

`extractInvestigation`'s block terminator, `packages/agent/examples/observer.ts:237`:

```ts
if (i > start && /^(CONFIRM|DISMISS) /.test(l.trim()) && !l.includes(sig)) break;
```

The block **start** is found by `l.includes(sig)` (`:231`) — decoration-tolerant — but the
**terminator** anchors `CONFIRM|DISMISS` at the start of the trimmed line, which `**DISMISS …`
never matches. So one signature's investigation notes bleed into the next signature's comment,
bounded only by the 40-line cap at `:234`. That asymmetry between the two readers of the same
reply is precisely the nesting table above.

### 3. Secondary — `fileAnomaly` never tests ownership (HARNESS-WRAPPER-99 Fix #1, still unlanded)

`packages/agent/src/observer.ts:816` `fileAnomaly` re-fetches the live task at `:825`, but the
only `dead-spawner` drop is the ORCHE-130 terminal-status check at `:852`, scoped to
`DEAD_SPAWNER_TERMINAL_STATUSES = {closed, tombstone}` (`:111`). It never compares
`task.assignee` against `a.facts.agentId` (`observer.ts:622`). `Issue.assignee` already exists
(`packages/fleet-db/src/types.ts:39`), so the check needs no new I/O.

**Unlike [HARNESS-WRAPPER-112](../md/internal/out-of-scope-tickets.md), this guard *does* fire
here.** HARNESS-WRAPPER-102 was `open`/`assignee: none` at file time and is assigned to a
different agent today, so `task.assignee !== 'agent:plan-critic:024391fc-…'` either way.
HARNESS-WRAPPER-113 is therefore the direct regression anchor for the guard that §-23, §-26,
[§-99](HARNESS-WRAPPER-99.md) and [§-111](HARNESS-WRAPPER-111.md) have all now re-derived without
landing.

### 4. Tertiary — the un-looped single-page drain

`packages/agent/examples/observer.ts:117` (and the identical library shape at
`packages/agent/src/observer.ts:242`) does one `await queue.pull(topic, subscriber)` per 300 s
tick against a backend whose `pull` is single-page by construction
(`packages/queue/src/fleet.ts:256-261`, `MAX_PAGE_LIMIT = 1000`). `dead-spawner` is an *absence*
detector, so a lagging cursor cannot distinguish "no heartbeat exists" from "I have not read the
heartbeats yet". The ≈60 s gap between this agent's real last event (~19:52:2x) and the
observer's `lastSeen` (~19:51:18) is that lag. Full analysis in
[HARNESS-WRAPPER-99](HARNESS-WRAPPER-99.md).

## Three independently sufficient suppressions — how this differs from `:103` and `:109`

This is what makes the ticket worth its own record rather than a footnote on §-111:

| Suppression | HARNESS-WRAPPER-111 (`:109`) | HARNESS-WRAPPER-112 (`:103`) | **HARNESS-WRAPPER-113 (`:102`)** |
| --- | --- | --- | --- |
| Patch A — read the decorated `DISMISS` | ✅ suppresses | ✅ suppresses | ✅ suppresses |
| Patch B — assignee grounding | ✅ (task re-leased) | ❌ assignee never changed | ✅ `open`/`assignee: none` at file time |
| Drain-to-empty | ✅ release already emitted | ✅ | ✅ release emitted 19:52:15Z |

`:103` needs Patch A or the drain fix; `:102` is stopped by any one of the three. A fix that
lands only Patch B still suppresses this signature, and a fix that lands only Patch A still
suppresses all three — which is why Patch A remains the higher-value half.

## Correction carried into the bundle: the Patch B fixture list is three tests, not one

The committed bundle's "Required companion" for Patch B named a single fixture
(`packages/agent/test/observer.unit.test.ts:725-736`). Verified against that file, **three**
`dead-spawner` tests assert the anomaly *still files* and all three stub `getTask` with **no
`assignee`**, so all three fail the moment Patch B lands:

| Test | Line | Stub |
| --- | --- | --- |
| `…whose task is in_progress is still filed` | `:730` | `{ id, status: 'in_progress', labels: [], title: id }` |
| `ORCHE-130: …in \`review\` still files` | `:777` | `{ id, status: 'review', … }` |
| `ORCHE-130: …\`deferred\` still files` | `:790` | `{ id, status: 'deferred', … }` |

Each needs `assignee: 'a1'`, matching the `acquired` helper's default `agentId = 'a1'`
(`observer.unit.test.ts:51`). The `:569` fixture is a *reopen-loop* test and is unaffected — the
guard is scoped to `a.kind === 'dead-spawner'`. The default `fakeClient.getTask` at `:100-106`
also carries no `assignee`, but no `dead-spawner` filing test relies on it: all five stub
explicitly.

## Confirmed: nothing in this repository participates

This is a Go module (`module github.com/olesho/harness-wrapper`); there is no `packages/` or
`apps/` tree here.

    $ grep -rniE 'dead-spawner|fileAnomaly|obs-sig|deadSpawnerMs|verdictFor|extractInvestigation' --include='*.go' .
    (no output)

The only tree-wide matches are the triage records under `docs/` and the cross-repo bundle. No
bus, queue, anomaly detector or verdict parser exists here. Adding fleet/lease/observer logic to
a Go PTY-supervision library would be a mis-port.

## Why it landed here anyway

Unchanged from
[§HARNESS-WRAPPER-23](../md/internal/out-of-scope-tickets.md#harness-wrapper-23--dead-spawner-re-alert-false-positive),
§HARNESS-WRAPPER-26, §HARNESS-WRAPPER-98, [§HARNESS-WRAPPER-99](HARNESS-WRAPPER-99.md) and
[§HARNESS-WRAPPER-111](HARNESS-WRAPPER-111.md): the observer files a `dead-spawner` against the
*task* it is watching (`HARNESS-WRAPPER-102`, in this repo's fleet-db workspace), so the ticket
lands in this workspace even though every line of the defect lives in `orche`. This is the
**fifth** recurrence of the class — and the third signature drawn from one reply.

## Fix (summary; the applicable form is in the bundle)

No new bundle. The existing
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md)
already specifies both patches; this triage amended it in place with the two corrections above:

- the Patch B companion fixture list corrected from one test to **three** (`:730`, `:777`,
  `:790`), each needing `assignee: 'a1'`;
- the `:237` bleed promoted from **predicted to confirmed**, with the nesting table as evidence,
  and the observed shape added to Patch A's test matrix: a reply with **three** consecutive
  decorated `DISMISS` lines must yield three *disjoint* investigation blocks, none containing a
  later signature's text or the trailing prose;
- HARNESS-WRAPPER-113 recorded in the bundle's Patch B section as the concrete signature the
  ownership guard suppresses.

**Do not land HARNESS-WRAPPER-99's Fix #2 verbatim** — its event-age freshness guard blinds the
detector in exactly the fleet-wide outage it exists to catch. Full argument in
[HARNESS-WRAPPER-99](HARNESS-WRAPPER-99.md#fix-2-is-unsound-as-specified--must-not-land-verbatim).

## Resolution

No source change in this repository — there is nothing here to change. No human gate is
requested: the in-repo change is documentation only and the root cause is unambiguous.

1. Hand the amended bundle to `orche`; it must be committed / PR'd there under its own ticket, as
   a follow-up to ORCHE-130.
2. Once handed off, the correct disposition for this ticket in this workspace is
   **close-as-invalid**, per the standing human ruling on HARNESS-WRAPPER-24 (2026-07-16) that
   this class not be re-filed here.
3. **Carried forward for a human — not a code defect, unchanged from §-111.**
   HARNESS-WRAPPER-79's decomposition children keep exceeding run deadlines (`-79` twice, `-109`
   once). Two more and the task parks at `blocked`/`stuck` per `MAX_CONSECUTIVE_TIMEOUTS`
   (`packages/agent/src/finalize.ts:60`). That is a task-decomposition question, out of scope
   here.
