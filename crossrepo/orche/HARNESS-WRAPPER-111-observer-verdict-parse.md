# HARNESS-WRAPPER-111 — cross-repo deliverable for `orche`: observer verdict parsing

> **This bundle ships a patch, not byte-exact files.** Unlike the sibling
> `crossrepo/meta-harness/` bundle (see [`../meta-harness/APPLY.md`](../meta-harness/APPLY.md)),
> `harness-wrapper` is **not** canonical for any of the source below and holds no copy of it.
> The changes are specified here as diffs-in-prose against pinned `orche` file:line anchors,
> because applying them requires reading the surrounding code — a byte-exact mirror would go
> stale the moment `orche` moves.
>
> **Paths in this file are `orche`-relative** (`/Users/oleh/Work/new/orche`, one checkout per
> reader; do not edit it from a `harness-wrapper` worktree). The work must be committed / PR'd
> **in `orche` under its own ticket**, as a follow-up to **ORCHE-130**.

Triage records and the full evidence chain, in `harness-wrapper`:
[`docs/triage/HARNESS-WRAPPER-111.md`](../../docs/triage/HARNESS-WRAPPER-111.md) (Patch A),
[`docs/triage/HARNESS-WRAPPER-99.md`](../../docs/triage/HARNESS-WRAPPER-99.md) and
[`docs/triage/HARNESS-WRAPPER-113.md`](../../docs/triage/HARNESS-WRAPPER-113.md) (Patch B — the
latter corrected the Patch B fixture list below from one test to three, and promoted the `:237`
block-bleed from predicted to **confirmed**),
[`docs/triage/HARNESS-WRAPPER-112.md`](../../docs/triage/HARNESS-WRAPPER-112.md) and
[`docs/triage/HARNESS-WRAPPER-115.md`](../../docs/triage/HARNESS-WRAPPER-115.md) (Patch C),
[`docs/triage/HARNESS-WRAPPER-114.md`](../../docs/triage/HARNESS-WRAPPER-114.md) (Patch D).

**Three of the filed signatures came from one reply.** HARNESS-WRAPPER-111 (`:109`),
HARNESS-WRAPPER-112 (`:103`) and HARNESS-WRAPPER-113 (`:102`) were all filed from a *single*
observer reply at 2026-07-22T19:59:11Z whose three verdict lines were all decorated `DISMISS`es.
One fix retires all three. The `:103` case is measured span-by-span against the orche build trace
in [`docs/triage/HARNESS-WRAPPER-112.md`](../../docs/triage/HARNESS-WRAPPER-112.md); note that
Patch B alone does **not** retire it (its assignee never changed), which is why Patch A is the
load-bearing half.

## Why this exists

The observer agent investigated `dead-spawner:worker:HARNESS-WRAPPER-109`, concluded it was a
false positive, and wrote an explicit `DISMISS` verdict. The supervisor filed the bug anyway and
then attached that dismissal as the ticket's "investigation" comment. Cause: the verdict matcher
at `packages/agent/examples/observer.ts:181` tests

```ts
reply.includes(`DISMISS ${sig}`)
```

— a strict substring match that any markdown decoration between the keyword and the signature
defeats. The observer wrote ``**DISMISS `dead-spawner:worker:HARNESS-WRAPPER-109`**``; the
backtick broke the match; the dismissal was silently discarded.

The prompt's contract is line-oriented (`packages/agent/examples/prompts/observer.md:43`: "write
a verdict line for each signature", "quote each `<signature>` **exactly**"); the implementation
is character-exact. Emphasis, code spans, bullets and headings all satisfy the stated contract
and break the implementation, and none of them is forbidden by the prompt.

The `:170-173` comment names the "a parse miss errs toward filing" default as deliberate. That
default is right for **silence**. It is wrong for a dismissal the parser could not read: there,
the strongest available signal that a ticket should not exist becomes the exact input that
produces the ticket.

## Layout

    packages/agent/examples/observer.ts          Patch A — verdictFor(), used at :181 and :237
                                                 Patch C — drain to empty at :117 (onPrepared)
                                                 Patch D — gateByPersistence(), used at :163-166
    packages/agent/src/observer.ts               Patch B — dead-spawner ownership grounding
                                                 Patch C — drain to empty at :242 (tick)
    packages/agent/test/observer-verdict.unit.test.ts   new — Patch A regression matrix
    packages/agent/test/observer-example.unit.test.ts   new — Patch D; FIRST coverage of the
                                                 deployed makeObserver path (today: zero tests)
    packages/agent/test/observer.unit.test.ts    Patch B + Patch C cases
                                                 + three fixture fixes (:730, :777, :790)

## Patch A — read the verdict by line, not by substring (the actual fix)

In `packages/agent/examples/observer.ts`. Both current call sites are module-private, so extract
a small **exported pure helper** to make it testable:

```ts
/** Leading markdown a verdict line may carry: blockquote, heading, bullet, emphasis. */
const VERDICT_DECORATION = /^[\s>#*_+\-•`]*/;

/** The verdict a reply records for `sig`, or undefined if it recorded none.
 *  Line-oriented and decoration-tolerant: the prompt asks for a verdict LINE, and
 *  `**DISMISS \`sig\`**` is one. Reading it as "no verdict" files the ticket the
 *  investigation just refuted (HARNESS-WRAPPER-111). Absence still errs toward
 *  filing — only an explicit, parseable DISMISS suppresses. */
export function verdictFor(reply: string, sig: string): 'CONFIRM' | 'DISMISS' | undefined {
  for (const raw of reply.split('\n')) {
    const line = raw.replace(VERDICT_DECORATION, '');
    const m = /^(CONFIRM|DISMISS)\b/.exec(line);
    if (m && line.includes(sig)) return m[1] as 'CONFIRM' | 'DISMISS';
  }
  return undefined;
}
```

Then:

- **`:181`** — replace ``if (reply.includes(`DISMISS ${sig}`))`` with
  `if (verdictFor(reply, sig) === 'DISMISS')`.
- **`:237`** (`extractInvestigation`'s block terminator) — replace
  `/^(CONFIRM|DISMISS) /.test(l.trim())` with the same decoration-stripped test, so block
  extraction terminates on decorated verdicts too. Without this, a multi-signature reply whose
  second verdict line is decorated bleeds one signature's investigation notes into the next
  signature's comment. Same defect, same line-shape assumption.

  **This bleed is confirmed, not hypothetical** (HARNESS-WRAPPER-113). The block *start* is found
  by `l.includes(sig)` at `:231` — decoration-tolerant — while the terminator is not, so the scan
  runs to the 40-line cap at `:234`. The single 19:59:11Z reply produced three comments that are
  exactly the reply's suffixes from each verdict line to EOF:

  | Ticket | Comment ts | Comment content |
  | --- | --- | --- |
  | HARNESS-WRAPPER-111 (`:109`) | 19:59:11.682143Z | `:109` block + trailing prose |
  | HARNESS-WRAPPER-112 (`:103`) | 19:59:11.693216Z | `:103` + `:109` blocks + trailing prose |
  | HARNESS-WRAPPER-113 (`:102`) | 19:59:11.702469Z | `:102` + `:103` + `:109` blocks + trailing prose |

  Three strictly nested suffixes is what a terminator that never fires produces, and nothing
  else.

**Why anchoring matters.** The match anchors at line start *after decoration only*. That is what
keeps prose such as *"a DISMISS on `sig` would be wrong"* from matching — the one direction that
could suppress a **genuine** anomaly, which is strictly worse than the bug being fixed. Do not
relax the anchor to a bare `includes('DISMISS')`.

Everything else is unchanged. No verdict for a signature still files, exactly as `:170-173`
intends; only an explicit, parseable `DISMISS` suppresses.

## Patch B — dead-spawner ownership grounding (HARNESS-WRAPPER-99 Fix #1; land it alongside)

Still unlanded after three prior sweeps. In `packages/agent/src/observer.ts`, immediately after
the ORCHE-130 terminal-status drop at `:852`, reusing the already-fetched `task` from `:822`
(**no new I/O**):

```ts
if (a.kind === 'dead-spawner' && task.assignee !== a.facts.agentId) {
  process.stderr.write(`[observer] dropping released dead-spawner ${sig} — task assignee is now ${task.assignee ?? 'none'}\n`);
  return;
}
```

A strict-subset guard; it alone would have suppressed this signature — and, on the same
`assignee` comparison, `dead-spawner:bug-reviewer:HARNESS-WRAPPER-112`
(**HARNESS-WRAPPER-114**, `in_progress` · reassigned to `agent:integrator:362767ce`) beside the
HARNESS-WRAPPER-113 signature. It keys on `assignee` —
**not** on `PROGRESSED_STATUSES`, **not** on progress labels. See the rationale at
`packages/agent/src/observer.ts:120-130` on why label-based convergence hid the ORCHE-67 stall.

**The concrete anchor is HARNESS-WRAPPER-113** (`dead-spawner:plan-critic:HARNESS-WRAPPER-102`).
That agent posted its two-chunk critique at 19:52:15.74Z and released; the task was
`open`/`assignee: none` at file time and is assigned to a different agent today, so
`task.assignee !== a.facts.agentId` either way and this guard drops it. Contrast
HARNESS-WRAPPER-112 (`:103`), where the assignee never changed — that one is retired by Patch A
or by the drain fix, not by this guard. Land both patches.

**Required companion — three fixtures, not one.** Verified against
`packages/agent/test/observer.unit.test.ts`: **three** `dead-spawner` tests assert the anomaly
*still files* and all three stub `getTask` with **no `assignee`**, so all three fail the moment
this guard lands:

| Test | Line | Stub |
| --- | --- | --- |
| `…whose task is in_progress is still filed` | `:730` | `{ id, status: 'in_progress', labels: [], title: id }` |
| `ORCHE-130: …in \`review\` still files` | `:777` | `{ id, status: 'review', … }` |
| `ORCHE-130: …\`deferred\` still files` | `:790` | `{ id, status: 'deferred', … }` |

Give each `assignee: 'a1'`, matching the `acquired` helper's default `agentId = 'a1'`
(`observer.unit.test.ts:51`). (Verified: a real break, not a hypothetical. An earlier revision of
this bundle named only `:725-736`; that was incomplete.) The `:569` fixture is a *reopen-loop*
test and is unaffected — the guard is scoped to `a.kind === 'dead-spawner'`. The default
`fakeClient.getTask` at `:100-106` also carries no `assignee`, but no `dead-spawner` filing test
relies on it: all five stub explicitly.

## Patch C — drain the bus to empty before scoring liveness (the class fix)

**This is no longer optional, and it is no longer an aside.** Through HARNESS-WRAPPER-112 this
fix lived as a conditional paragraph inside "Explicitly NOT in this bundle" — so no implementer
ever picked it up, and the class re-derived itself seven times (§-23, §-26, §-99, §-111,
§-112, §-113, §-114, §-115 — §-113 and §-114 restate the same un-looped drain as their primary
or secondary cause). It is specified here as a first-class patch for exactly that reason.

**What it fixes.** The observer's `working` status is *derived from drained events*. A
`task_released` sitting on an undrained page leaves the agent's folded state at
`status: 'working'` with `currentTaskId` still set (`apps/screen/src/state.ts:371-384` — the fold
sets `idle` at `:378` and deletes `currentTaskId`), so the dead-spawner predicate at
`packages/agent/src/observer.ts:537` fires against an agent that already finished.

HARNESS-WRAPPER-115 is the proof: `agent:bug-reviewer:4f756716-…` was accused of 545 s of silence
on HARNESS-WRAPPER-113, but its whole recorded lifetime was **279.76 s**, ending in
`POST /issues/{id}/release` → **`204`** and `agent.finalize outcome=success`. The `204` proves
the terminating event reached fleet-db; it was simply still undrained. The claimed silence is
1.95× the agent's total existence. Full arithmetic:
[`docs/triage/HARNESS-WRAPPER-115.md`](../../docs/triage/HARNESS-WRAPPER-115.md).

**Why one `pull` is not enough.** `packages/queue/src/fleet.ts:256-261` — `pull()` issues exactly
**one** `pullRaw` and returns `raw.messages`; it is single-page by construction (contrast
`read()` at `:203-215`, which *does* page). With `limit` undefined no `limit` param is sent, so
the page is the server default, capped by `MAX_PAGE_LIMIT = 1000` (`fleet.ts:63`). One page per
`intervalMs = 300_000` tick (`observer.ts:213`) cannot keep up with a busy fleet.

### C1 — loop the drain at `packages/agent/src/observer.ts:242`

Replace the single `const batch = await queue.pull(topic, subscriber);` with `pull` → fold →
`ackThrough`, repeating **until a batch returns empty**. The cursor commit currently at `:264`
moves inside the loop so progress is durable per page, not only at the end.

The loop **must** carry a bounded iteration cap, and hitting the cap **must** be loud:

```ts
const MAX_DRAIN_PAGES = 50;   // bounded: a runaway producer must not stall the tick forever
let drainedToEmpty = false;
for (let page = 0; page < MAX_DRAIN_PAGES; page++) {
  const batch = await queue.pull(topic, subscriber);
  if (batch.length === 0) { drainedToEmpty = true; break; }
  // …fold batch into windowEvents / screen state, exactly as today…
  await queue.ackThrough(topic, subscriber, /* last cursor in batch */);
}
if (!drainedToEmpty) {
  process.stderr.write(
    `[observer] drain cap ${MAX_DRAIN_PAGES} hit — bus still backlogged; skipping absence-based detectors this tick\n`,
  );
}
```

**A silent cap reproduces this bug in a new shape** — a capped-out drain looks exactly like a
drained one to every downstream detector. The warn is not decoration; it is the difference
between a bounded loop and a re-run of the same defect.

### C2 — the same treatment at `packages/agent/examples/observer.ts:117`

`onPrepared` (`:113`) has the identical single-`pull` shape and is the **deployed** path. Patch
C1 alone leaves the actually-running observer broken. Both sites, or the fix does not ship.

### C3 — gate the `dead-spawner` scan on drain state, never on event age

Absence-based detection is only valid on a caught-up view. Gate the scan on `drainedToEmpty` —
the boolean the loop already computes — and on nothing else:

- **Never** gate on event age. That is §-99's Fix #2, and it is rejected below for a reason that
  is unchanged: it blinds the detector in exactly the outage it exists to catch.
- **Never** gate on "the window looks quiet". Quiet is the signal, not the disqualifier.

`dead-spawner` is the only absence-based detector. Leave `reopen-loop`, `error-burst`, `backlog`,
`role-coverage` and `release-lag` **untouched** — they are count-based and stay valid on a
lagging window; suppressing them on a capped drain would trade one false-negative class for
another.

### C4 — Patch C is the class fix; Patch B is not a substitute

Patch B (ownership grounding) and Patch C cover overlapping but **different** signatures:

| | HARNESS-WRAPPER-112 | HARNESS-WRAPPER-115 |
| --- | --- | --- |
| Task assignee at file time | still the accused agent | a **different** agent |
| Patch B suppresses? | **no** — the guard is a no-op | **yes** |
| Patch C suppresses? | **yes** | **yes** |

So Patch B alone suppresses HARNESS-WRAPPER-115 and gives it a welcome second line of defence,
but it does nothing for HARNESS-WRAPPER-112. **Only Patch C covers both.** Land B for
defence-in-depth; land C to close the class.

### C5 — `emitScreen` publish-loss is de-listed as a cause

HARNESS-WRAPPER-112 listed `emitScreen`'s bare `.catch(() => {})`
(`packages/agent/src/spawner.ts:1828-1830`) as a tertiary cause, because heartbeat-timer evidence
cannot prove every publish landed. **HARNESS-WRAPPER-115 excludes it for this shape.** The causal
event there is the *release*, and its `204` proves it reached fleet-db — every heartbeat publish
in that run could have been silently dropped and the false positive still would not have
occurred, had the release been drained.

Instrumenting `emitScreen` (log/count the swallowed failure) remains worthwhile on its own
merits, but it is **follow-up instrumentation, not a fix for released-agent false positives**,
and the record should stop listing it as a candidate cause for them.

## Patch D — the deployed example tick has no persistence gate (HARNESS-WRAPPER-114)

**The fleet does not run `observe()`.** The deployed chain is
`$ORCHE_PROJECT_DIR/.orche/agents/observer.ts` →
`packages/agent/examples/.orche/agents/observer.ts:16-18` → `makeObserver`
(`packages/agent/examples/observer.ts:62`). `makeObserver` reimplements the tick in `onPrepared`
(`:113-167`) over the same detection helpers rather than calling `observe()`, and that second
implementation silently lacks the library's consecutive-tick persistence gate
(`packages/agent/src/observer.ts:215`, `:221`, `:294-304` — *"A one-tick blip never reaches the
filing stage."*). `handled` (`examples/observer.ts:89`) is a **dedup TTL for already-filed
signatures**, not a streak counter; nothing gates the *first* filing. `grep -n
'persistence\|persistTicks' packages/agent/examples/observer.ts` returns nothing.

Smallest correct edit — port the one gate that matters, as an **exported pure helper** so the
deployed path becomes testable at all (symmetric with Patch A's `verdictFor`). In
`packages/agent/examples/observer.ts`:

```ts
/** Consecutive ticks a signature must persist before it may be filed. Mirrors
 *  observe()'s persistTicks (src/observer.ts:215): the deployed example path is a
 *  SECOND tick implementation and silently lacked this gate, so a one-tick window
 *  blip amplified straight into a ticket (HARNESS-WRAPPER-114). Exported and pure
 *  so it is testable without booting the hooks. Mutates `streaks` in place:
 *  advances every signature seen this tick, deletes any not seen (recovery ⇒ reset). */
export const PERSIST_TICKS = 2;

export function gateByPersistence(
  anomalies: Anomaly[],
  streaks: Map<string, number>,
  persistTicks = PERSIST_TICKS,
): Anomaly[] {
  const seen = new Set(anomalies.map(anomalySignature));
  for (const sig of [...streaks.keys()]) if (!seen.has(sig)) streaks.delete(sig);
  return anomalies.filter((a) => {
    const sig = anomalySignature(a);
    const streak = (streaks.get(sig) ?? 0) + 1;
    streaks.set(sig, streak);
    return streak >= persistTicks;
  });
}
```

Add `const persistence = new Map<string, number>();` to the `makeObserver` closure beside
`handled` (`:89`), and at `:163-166` gate after the `handled` filter:

```ts
const fresh = gateByPersistence(
  detectAnomalies(digest, { releaseLagMinMs: releaseLagFloorMs() })
    .filter((a) => !handled.has(anomalySignature(a))),
  persistence,
);
```

Gating **before** the digest write at `:167` is deliberate: a one-tick blip then costs no Claude
investigation either — the saving `handled`'s comment at `:83-88` already reasons about. The
considered alternative — show all anomalies in the digest and gate only the filing in
`onComplete` — buys an earlier human-readable verdict at the cost of a Claude run per blip and
split bookkeeping across the two hooks. Considered and rejected, not silently dropped.

**Honest scoping.** This is a *mitigation*, not a cure: it suppresses HARNESS-WRAPPER-114 only if
the signature is absent on the following tick, which requires the drain to have caught up. It is
strictly complementary to the drain fix, and it costs up to 300 s of extra latency on a *genuine*
dead-spawner — a trade `observe()` already made deliberately.

**Out of scope for this patch, and the reason this class has survived seven triages.** The durable
fix is to make `makeObserver` consume `observe()`'s tick so there is **one** implementation. The
example path has drifted in three further ways, each of which the merge would also repair —
named here so the follow-up is scoped rather than rediscovered an eighth time:

- **probe filter / `ignoreSpawners`** — `isObserverProbe(ev.spawner)` and the `ignoreSpawners`
  ingestion filter exist only in the library (`src/observer.ts:212`, `:259`). Synthetic probe
  traffic can therefore enter the deployed `windowEvents`, and the operator-facing mute knob is
  **unreachable on the path that actually runs**.
- **`incidentId` correlation** (`src/observer.ts:311`) is absent — `fileAnomaly(client, a)` is
  called two-arg at `examples/observer.ts:187`, so co-firing signatures in one digest cannot be
  grouped into a single incident.
- **single-page drain** at `examples/observer.ts:117` mirrors `src/observer.ts:242`; both need
  the drain-to-empty loop — that one is **Patch C above**, and is in scope here.

Merging the two implementations is an architectural change requiring a human decision on the
hook/`observe()` boundary. Patch D is the minimal port of the single gate this ticket proves is
load-bearing.

**And note what this implies for Patch A.** Patch B lands in `src/` and reaches production
through the shared `fileAnomaly`. Patches A, C2 and D do not: they live in `examples/`,
which **no test loads** — `grep -rn 'makeObserver' packages/agent/test/` and
`grep -rln 'examples/observer' packages/agent/test/` both return nothing, and all 35 `observe(`
call sites in `observer.unit.test.ts` drive the library. That is why the `examples/` patches are
specified as exported pure helpers.

## Explicitly NOT in this bundle

**Do not carry over HARNESS-WRAPPER-99's Fix #2 verbatim.** Its event-age freshness guard
(skip the `dead-spawner` scan when `now - max(ts over windowEvents) > deadSpawnerMs`) blinds the
detector in exactly the fleet-wide outage it exists to catch: with `windowMs` 30 min and
`deadSpawnerMs` 4.5 min, a fleet whose only `working` agents are the dead ones has its newest
in-window event *be* the dead agent's last heartbeat, so the guard suppresses the scan across the
entire genuine detection band. Full argument in
[`docs/triage/HARNESS-WRAPPER-99.md`](../../docs/triage/HARNESS-WRAPPER-99.md#fix-2-is-unsound-as-specified--must-not-land-verbatim).

This exclusion is **retained under Patch C** and is the reason C3 gates on drain state rather
than on event age. The two are easy to confuse: both are "is the view fresh enough to trust an
absence?", but only the drain-state signal distinguishes *"the bus is quiet"* from *"we have not
looked at the bus"*.

> **Superseded note.** Earlier revisions of this bundle mentioned the drain-to-empty loop only
> here, as a conditional aside ("if drain lag is addressed in the same pass…"). That phrasing is
> why seven triage passes re-derived the fix and none landed it. It is now **Patch C** above, and
> it is in scope.

## Tests to add

### `packages/agent/test/observer-verdict.unit.test.ts` (new, unless a home already exists)

Against the exported `verdictFor` and, where practical, the `onComplete` hook end-to-end:

- ``**DISMISS `<sig>`**`` **suppresses filing** — the verbatim HARNESS-WRAPPER-111 regression
  anchor; assert **no ticket created**.
- Bare `DISMISS <sig>`, `- DISMISS <sig>`, `> DISMISS <sig>` and `## DISMISS <sig>` all suppress.
- A reply with **no** verdict for `<sig>` **still files** — the conservative default is preserved.
- `CONFIRM <sig>` files, however decorated.
- Prose containing the word `DISMISS` mid-sentence alongside `<sig>` does **not** suppress
  (e.g. `a DISMISS on <sig> would be wrong`).
- A two-signature reply whose **second** verdict line is decorated: each investigation note is
  extracted to its own block, with no bleed across the boundary (`extractInvestigation`).
- **The HARNESS-WRAPPER-113 shape** — the observed production case: a **three**-signature reply
  with all three verdict lines decorated ⇒ **three disjoint** `extractInvestigation` blocks. No
  block may contain a later signature's text, and the trailing non-verdict prose after the final
  `---` attaches to the **last** block only. Pre-fix this yields three nested suffixes; that
  assertion is the regression anchor for `:237`.

### `packages/agent/test/observer.unit.test.ts` (Patch B)

Beside the ORCHE-130 block at `:725-805`, reusing its `seed` / `acquired` helpers and the fake
fleet's `f.created`:

- a `dead-spawner` whose re-fetched task is `{status:'open', assignee: undefined}` is **dropped**
  — the direct HARNESS-WRAPPER-113 anchor;
- a `dead-spawner` reassigned to a different agent (`assignee: 'agent:worker:other'`) is
  **dropped**;
- a `dead-spawner` whose re-fetched task is `{status:'in_progress', assignee:'agent:worker:other'}`
  — **`in_progress` *and* owned by someone else** — is **dropped**. This is the
  **HARNESS-WRAPPER-114 anchor** and the row that proves the guard **cannot be approximated by
  any status check**: it has the *same* status as the "still files" case below, and only the
  assignee comparison separates them. (Live instance: `-112` was `in_progress` ·
  `agent:integrator:362767ce` when the observer accused the already-released
  `agent:bug-reviewer:9231a24f`.) Note that `src/observer.ts:836-841` deliberately forbids
  applying `PROGRESSED_STATUSES` here — this row is why that comment and this guard coexist.
- a `dead-spawner` that is `in_progress` **and still assigned to the accused agent** **still
  files** — the over-suppression guard, and the reason the `:730` / `:777` / `:790` fixtures must
  each gain a matching `assignee`.

### `packages/agent/test/observer-example.unit.test.ts` (new — Patch D)

The **first** test coverage of the deployed path; today `grep -rn 'makeObserver'
packages/agent/test/` returns nothing. Against the exported `gateByPersistence` (and
`verdictFor`, if Patch A lands in the same pass):

- a signature seen on **one** tick is not eligible; seen on **two consecutive** ticks is eligible
  — *the direct HARNESS-WRAPPER-114 regression anchor; there is no test today that fails.*
- a signature absent on tick 2 has its streak **deleted**, so tick 3 alone does not file —
  recovery resets, no accumulation across a gap.
- one signature's presence never advances another's streak — independent counters.
- `persistTicks = 1` reproduces today's behaviour exactly — the escape hatch for anyone who wants
  first-tick filing back.

### `packages/agent/test/observer.unit.test.ts` (Patch C)

The fake-queue helpers already exist around `:512-535`; extend them to return a scripted sequence
of pages so `pull` can be driven page-by-page.

- **`the tick drains a multi-page backlog to empty in one tick`** — fake queue returning two full
  pages then empty; assert `pull` is called until drained and `windowEvents` holds every event.
  **Fails today.**
- **`a dead-spawner is not reported for an agent whose task_released is on a later page`** — the
  direct regression anchor for HARNESS-WRAPPER-115: page 1 full of unrelated traffic, page 2
  carrying the accused agent's `task_released`; assert the digest reports **no** dead spawner.
  **Fails today.**
- **`the tick warns and does not skip silently when the drain cap is hit`** — fake queue that
  never empties; assert the `stderr` warn fires. Guards C1's loudness requirement.
- **`a digest whose drain never caught up reports no dead spawners.`**
- **`a caught-up digest still reports a genuinely stale working agent even when it is the ONLY
  agent on the bus`** — the over-suppression guard that **must** pass: no other traffic, one
  `working` agent whose last heartbeat is `now - 10 min`, drain reached empty ⇒ that agent **is**
  reported. This is the test that would fail if C3 were ever weakened into an event-age guard.
- **`the count-based detectors still fire on a capped drain`** — assert `reopen-loop` /
  `error-burst` are unaffected by `drainedToEmpty === false`.

## Apply

There is no script: read each anchor, apply the change, run the suite.

    cd "$ORCHE_DIR"            # /Users/oleh/Work/new/orche
    # Patch A: packages/agent/examples/observer.ts  (add verdictFor; use at :181 and :237)
    # Patch B: packages/agent/src/observer.ts       (ownership drop after the :852 terminal drop)
    #          packages/agent/test/observer.unit.test.ts:730,:777,:790  (each fixture gains `assignee: 'a1'`)
    # Patch C: packages/agent/src/observer.ts:242        (drain loop + cap + warn; ack per page)
    #          packages/agent/examples/observer.ts:117   (same shape, deployed path)
    #          packages/agent/src/observer.ts:537        (gate the scan on drainedToEmpty)
    # Patch D: packages/agent/examples/observer.ts  (add gateByPersistence + the closure Map;
    #                                                gate at :163-166, before the digest write)
    pnpm vitest run packages/agent/test/observer-verdict.unit.test.ts \
                    packages/agent/test/observer-example.unit.test.ts \
                    packages/agent/test/observer.unit.test.ts

Acceptance: the ``**DISMISS `<sig>`**`` case files nothing, the no-verdict case still files, and
the three-signature decorated reply produces three disjoint comments (A); an `in_progress` task
assigned to a *different* agent is dropped while the `in_progress` / `review` /
`deferred`-and-still-assigned cases still file (B); the later-page `task_released` case files
nothing while the lone-stale-agent-on-an-empty-bus case **still files** (C); a first-tick anomaly
files nothing and a second consecutive tick does (D).

## Paired ticket

Raise in `orche` as a follow-up to **ORCHE-130** (whose terminal-status drop Patch B extends).

- **Patch A** is independently landable and is the higher-value half of the parse defect: it
  stops the observer's own judgement from being discarded, which suppresses this class at the
  source even while the detector stays noisy.
- **Patch B** is the fourth re-derivation of the same unlanded guard — land it this time.
- **Patch C** is the **class fix**. It is the only one of the four that covers both
  HARNESS-WRAPPER-112 and HARNESS-WRAPPER-115, and it has now been re-derived seven times without
  landing because it was never written down as a patch. If only one thing ships, ship this.
- **Patch D** is a complementary *mitigation*, not a substitute for C: it suppresses a one-tick
  blip on the deployed path (HARNESS-WRAPPER-114) and, as its own section states, only helps once
  the drain has caught up. Its standalone value is that it makes the deployed `makeObserver` path
  testable at all — today it has zero coverage.

As of HARNESS-WRAPPER-115's triage, `orche list --workspace ORCHE --limit 500` still carries
**no open observer/drain ticket** and ORCHE-130 is `closed/merged/done`. Filing that follow-up is
the outstanding action; no worker in a `harness-wrapper` worktree can perform it. Until it
exists, the class keeps re-filing itself — HARNESS-WRAPPER-115 is a `dead-spawner` false positive
against the **bug-reviewer that was triaging the previous one**, and a bug-reviewer triage run
(279.76 s) already exceeds `deadSpawnerMs = 270_000`, so that lane has zero slack against drain
lag.

## Out of scope, carried forward for a human

HARNESS-WRAPPER-79's decomposition children keep exceeding run deadlines (`-79` timed out twice;
`-109` has now burned `timeout-attempt-1`). Two more and the task parks at `blocked`/`stuck` per
`MAX_CONSECUTIVE_TIMEOUTS` (`packages/agent/src/finalize.ts:60`). That is a task-decomposition
question, not a code defect.
</content>
