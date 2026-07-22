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
`harness-wrapper`.

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
    packages/agent/src/observer.ts               Patch B — dead-spawner ownership grounding
    packages/agent/test/observer-verdict.unit.test.ts   new — Patch A regression matrix
    packages/agent/test/observer.unit.test.ts    Patch B cases + one fixture fix at :725-736

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

A strict-subset guard; it alone would have suppressed this signature. It keys on `assignee` —
**not** on `PROGRESSED_STATUSES`, **not** on progress labels. See the rationale at
`packages/agent/src/observer.ts:120-130` on why label-based convergence hid the ORCHE-67 stall.

**Required companion.** The fixture at `packages/agent/test/observer.unit.test.ts:725-736` stubs
`getTask` with **no `assignee`**, so it starts failing under this guard. Give it an `assignee`
matching the seeded agent id. (Verified: this is a real break, not a hypothetical.)

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
  (e.g. *"a DISMISS on `<sig>` would be wrong"`)`.
- A two-signature reply whose **second** verdict line is decorated: each investigation note is
  extracted to its own block, with no bleed across the boundary (`extractInvestigation`).

### `packages/agent/test/observer.unit.test.ts` (Patch B)

Beside the ORCHE-130 block at `:725-805`, reusing its `seed` / `acquired` helpers and the fake
fleet's `f.created`:

- a `dead-spawner` whose re-fetched task is `{status:'open', assignee: undefined}` is **dropped**;
- a `dead-spawner` reassigned to a different agent (`assignee: 'agent:worker:other'`) is
  **dropped**;
- a `dead-spawner` that is `in_progress` **and still assigned to the accused agent** **still
  files** — the over-suppression guard, and the reason the `:725` fixture must gain a matching
  `assignee`.

## Apply

There is no script: read each anchor, apply the change, run the suite.

    cd "$ORCHE_DIR"            # /Users/oleh/Work/new/orche
    # Patch A: packages/agent/examples/observer.ts  (add verdictFor; use at :181 and :237)
    # Patch B: packages/agent/src/observer.ts       (ownership drop after the :852 terminal drop)
    #          packages/agent/test/observer.unit.test.ts:725-736  (fixture gains `assignee`)
    pnpm vitest run packages/agent/test/observer-verdict.unit.test.ts \
                    packages/agent/test/observer.unit.test.ts

Acceptance: the ``**DISMISS `<sig>`**`` case files nothing, the no-verdict case still files, and
the `in_progress`-and-still-assigned case still files.

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
