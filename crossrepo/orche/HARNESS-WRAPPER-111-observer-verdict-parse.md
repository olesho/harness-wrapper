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

Triage record and the full evidence chain:
[`docs/triage/HARNESS-WRAPPER-111.md`](../../docs/triage/HARNESS-WRAPPER-111.md) in
`harness-wrapper`, amended by
[`docs/triage/HARNESS-WRAPPER-113.md`](../../docs/triage/HARNESS-WRAPPER-113.md) (which corrected
the Patch B fixture list below from one test to three, and promoted the `:237` block-bleed from
predicted to **confirmed**).

**This bundle covers three filed signatures, not one.** HARNESS-WRAPPER-111 (`:109`),
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
                                                 Patch C — gateByPersistence(), used at :163-166
    packages/agent/src/observer.ts               Patch B — dead-spawner ownership grounding
    packages/agent/test/observer-verdict.unit.test.ts   new — Patch A regression matrix
    packages/agent/test/observer-example.unit.test.ts   new — Patch C; FIRST coverage of the
                                                 deployed makeObserver path (today: zero tests)
    packages/agent/test/observer.unit.test.ts    Patch B cases + three fixture fixes (:730, :777, :790)

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

## Patch C — the deployed example tick has no persistence gate (HARNESS-WRAPPER-114)

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

**Out of scope for this patch, and the reason this class has survived six triages.** The durable
fix is to make `makeObserver` consume `observe()`'s tick so there is **one** implementation. The
example path has drifted in three further ways, each of which the merge would also repair —
named here so the follow-up is scoped rather than rediscovered a seventh time:

- **probe filter / `ignoreSpawners`** — `isObserverProbe(ev.spawner)` and the `ignoreSpawners`
  ingestion filter exist only in the library (`src/observer.ts:212`, `:259`). Synthetic probe
  traffic can therefore enter the deployed `windowEvents`, and the operator-facing mute knob is
  **unreachable on the path that actually runs**.
- **`incidentId` correlation** (`src/observer.ts:311`) is absent — `fileAnomaly(client, a)` is
  called two-arg at `examples/observer.ts:187`, so co-firing signatures in one digest cannot be
  grouped into a single incident.
- **single-page drain** at `examples/observer.ts:117` mirrors `src/observer.ts:242`; both need
  the drain-to-empty loop described below.

Merging the two implementations is an architectural change requiring a human decision on the
hook/`observe()` boundary. Patch C is the minimal port of the single gate this ticket proves is
load-bearing.

**And note what this implies for Patch A.** Patch B lands in `src/` and reaches production
through the shared `fileAnomaly`. Patches A and C do not: they live in `examples/`, which **no
test loads** — `grep -rn 'makeObserver' packages/agent/test/` and
`grep -rln 'examples/observer' packages/agent/test/` both return nothing, and all 35 `observe(`
call sites in `observer.unit.test.ts` drive the library. That is why both patches are specified
as exported pure helpers.

## Explicitly NOT in this bundle

**Do not carry over HARNESS-WRAPPER-99's Fix #2 verbatim.** Its event-age freshness guard
(skip the `dead-spawner` scan when `now - max(ts over windowEvents) > deadSpawnerMs`) blinds the
detector in exactly the fleet-wide outage it exists to catch: with `windowMs` 30 min and
`deadSpawnerMs` 4.5 min, a fleet whose only `working` agents are the dead ones has its newest
in-window event *be* the dead agent's last heartbeat, so the guard suppresses the scan across the
entire genuine detection band. Full argument in
[`docs/triage/HARNESS-WRAPPER-99.md`](../../docs/triage/HARNESS-WRAPPER-99.md#fix-2-is-unsound-as-specified--must-not-land-verbatim).

If drain lag is addressed in the same pass, take **only** the drain-to-empty loop at
`packages/agent/src/observer.ts:242` — `pull` → fold → `ackThrough` until a batch returns empty,
with a bounded iteration cap and a `stderr` warn when the cap is hit (a silent cap reproduces
this bug in a new shape) — and derive the freshness signal from *whether the drain reached
empty*, not from event age. The deployed example path
(`packages/agent/examples/observer.ts:117`) has the same single-`pull` shape and needs the same
treatment.

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

### `packages/agent/test/observer-example.unit.test.ts` (new — Patch C)

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

## Apply

There is no script: read each anchor, apply the change, run the suite.

    cd "$ORCHE_DIR"            # /Users/oleh/Work/new/orche
    # Patch A: packages/agent/examples/observer.ts  (add verdictFor; use at :181 and :237)
    # Patch B: packages/agent/src/observer.ts       (ownership drop after the :852 terminal drop)
    #          packages/agent/test/observer.unit.test.ts:730,:777,:790  (each fixture gains `assignee: 'a1'`)
    # Patch C: packages/agent/examples/observer.ts  (add gateByPersistence + the closure Map;
    #                                                gate at :163-166, before the digest write)
    pnpm vitest run packages/agent/test/observer-verdict.unit.test.ts \
                    packages/agent/test/observer-example.unit.test.ts \
                    packages/agent/test/observer.unit.test.ts

Acceptance: the ``**DISMISS `<sig>`**`` case files nothing, the no-verdict case still files, and
the three-signature decorated reply produces three disjoint comments (A); an `in_progress` task
assigned to a *different* agent is dropped while the `in_progress` / `review` /
`deferred`-and-still-assigned cases still file (B); a first-tick anomaly files nothing and a
second consecutive tick does (C).

## Paired ticket

Raise in `orche` as a follow-up to **ORCHE-130** (whose terminal-status drop Patch B extends).
Patch A is independently landable and is the higher-value half: it stops the observer's own
judgement from being discarded, which suppresses this whole class at the source even while the
detector stays noisy. Patch B is the fourth re-derivation of the same unlanded guard — land it
this time.

## Out of scope, carried forward for a human

HARNESS-WRAPPER-79's decomposition children keep exceeding run deadlines (`-79` timed out twice;
`-109` has now burned `timeout-attempt-1`). Two more and the task parks at `blocked`/`stuck` per
`MAX_CONSECUTIVE_TIMEOUTS` (`packages/agent/src/finalize.ts:60`). That is a task-decomposition
question, not a code defect.
</content>
