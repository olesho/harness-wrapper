# HARNESS-WRAPPER-111 — Triage record: the observer dismissed this anomaly and the supervisor filed it anyway

**Ticket:** `[observer] crashed/dead spawner worker left HARNESS-WRAPPER-109 working
(dead-spawner:worker:HARNESS-WRAPPER-109)`.

**Verdict:** a **real, newly located defect**, and a sharper diagnosis than the three prior
sweeps in this class
([HARNESS-WRAPPER-23](../md/internal/out-of-scope-tickets.md#harness-wrapper-23--dead-spawner-re-alert-false-positive),
HARNESS-WRAPPER-26, [HARNESS-WRAPPER-99](HARNESS-WRAPPER-99.md)). Those three all argued the
detector is too noisy. This one finds something worse and much cheaper to fix: **the observer
correctly judged the anomaly a false positive, wrote an explicit `DISMISS` verdict, and the
verdict was silently discarded by a substring match that markdown decoration defeats.** The
ticket exists *because* the dismissal was unreadable.

The defect lives entirely in the **`orche`** tooling repo (`/Users/oleh/Work/new/orche`), not
in `harness-wrapper`. This repository therefore receives no source change; the deliverables are
this record plus the ready-to-apply bundle
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md).

## This ticket is its own reproduction

No fleet-state reconstruction is required to prove the defect, because both halves of the proof
are artifacts of a *single* `onComplete` pass:

1. The ticket's **description** is verbatim `renderBugBody` output — i.e. `fileAnomaly` ran.
2. The ticket's **sole comment** (`agent:observer:07f8a7c5-…`, 2026-07-22T19:59:11Z) opens with

       **DISMISS `dead-spawner:worker:HARNESS-WRAPPER-109`**
       Real stall, already self-healed — no bug to file.

   i.e. the same reply that produced the ticket says the ticket should not exist.

A filed bug whose attached investigation is a dismissal of that bug is not an ordering artifact
or a race — the filing branch and the comment branch read the *same* `reply` string in the same
pass, and disagree about it. That disagreement is the bug.

## Root cause

### Primary — the verdict is parsed as a strict substring of a markdown-authored reply

`packages/agent/examples/observer.ts:181` (agent defined at `:62 makeObserver`, cron tick every
300 s at `:102`, hook body `:174-206`):

```ts
if (reply.includes(`DISMISS ${sig}`)) { … continue }   // suppress
```

The observer wrote ``**DISMISS `dead-spawner:worker:HARNESS-WRAPPER-109`**``. A backtick sits
between the space and the signature, so `reply.includes('DISMISS dead-spawner:worker:HARNESS-WRAPPER-109')`
is `false`, the suppression branch at `:181` is skipped, and `fileAnomaly` runs at `:187`.

The contract the prompt states is **line-oriented** — `packages/agent/examples/prompts/observer.md:43`
asks for "a verdict line for each signature", "write `DISMISS <signature>`", and "quote each
`<signature>` **exactly**". The implementation is **character-exact**. Every form of ordinary
markdown a model reaches for when writing a verdict heading — `**bold**`, a code span around
the signature, a `-` bullet, a `>` blockquote, a `##` heading — satisfies the stated contract
and defeats the implementation. None of them is forbidden by the prompt.

`extractInvestigation` (`:229`) then locates the block by `l.includes(sig)`, which *does*
tolerate the decoration, and posts the dismissal text as the ticket's comment at `:196`. The two
readers of the same reply use different matching rules; that asymmetry is what makes the
artifact pair self-contradictory instead of merely wrong.

**Why "err toward filing" does not excuse this.** The comment at `:170-173` names the
conservative default deliberately: a parse miss files rather than drops. That conservatism is
designed for the case where the harness returns **silence** — no verdict, no judgement, so file
and let a human look. It is not designed for the case where the harness returned the strongest
possible signal that no ticket should exist. Applied here the default is not conservative, it is
**inverted**: a decisive, correct, prompt-compliant dismissal is the exact input that produces
the ticket. A reply with *no* verdict for the signature would have produced the same outcome,
which is precisely the problem — the observer's judgement has no effect on the outcome it was
asked to decide.

**Same defect, second site.** `extractInvestigation`'s block terminator at `:237`,
`/^(CONFIRM|DISMISS) /.test(l.trim())`, carries the identical line-shape assumption. In a
multi-signature reply a decorated verdict fails to terminate the preceding block, so one
signature's investigation notes bleed into the next signature's comment.

### Secondary — the anomaly should never have been raised

The underlying event is benign and had already self-healed. `agent:worker:d10a71e6` blew its run
deadline (`agent run timeout: context deadline exceeded`, 19:57:02Z) *after* preserving two
commits on `work/HARNESS-WRAPPER-109`; orche's documented timeout path released the lease,
applied `timeout-attempt-1` (`packages/agent/src/finalize.ts:60-61`,
`MAX_CONSECUTIVE_TIMEOUTS = 3`) and re-leased the task to `agent:worker:53ee7ce1`. Two gaps —
both already diagnosed, both still unlanded, both re-verified against current `orche` source at
triage time — let the detector fire anyway:

- **No ownership grounding.** `fileAnomaly` re-fetches the live task
  (`packages/agent/src/observer.ts:822`), but the only `dead-spawner` drop is the ORCHE-130
  terminal-status check at `:852`, scoped to
  `DEAD_SPAWNER_TERMINAL_STATUSES = {closed, tombstone}` (`:111`). The defining claim of a
  `dead-spawner` is *"left agent X working on task T"*; once `task.assignee` is no longer
  `facts.agentId` that claim is false on its face, whatever the status.
  `Issue.assignee` already exists (`packages/fleet-db/src/types.ts:39`), so the check needs no
  new I/O. This is **Fix #1 of [HARNESS-WRAPPER-99](HARNESS-WRAPPER-99.md), still unlanded** —
  the fourth consecutive sweep to re-derive it.
- **Un-looped drain.** The deployed observer's `onPrepared` hook does a single
  `await queue.pull(topic, subscriber)` per 300 s tick
  (`packages/agent/examples/observer.ts:117`; the library path has the identical shape at
  `packages/agent/src/observer.ts:242`) against a backend whose `pull` is single-page by
  construction (`packages/queue/src/fleet.ts:256-261`, `MAX_PAGE_LIMIT = 1000`). `dead-spawner`
  is an *absence* detector, so a lagging cursor cannot distinguish "no heartbeat exists" from
  "I have not read the heartbeats yet". The released/stopped events for `d10a71e6` were emitted
  (`apps/screen/src/state.ts:361-413` clears `working` on `task_released` / `agent_stopped`) but
  not yet drained. `ageMs = 286 s` against a 270 s threshold (`observer.ts:51`) is exactly the
  marginal exceedance that structural drain lag produces.

**The primary defect is what turns the secondary ones into tickets.** With the verdict read
correctly, the observer's own judgement suppresses this whole class at the source even while the
detector stays noisy. That ordering matters for the fix: Patch A is the one that stops the
bleeding.

## Confirmed: nothing in this repository participates

This is a Go module (`module github.com/olesho/harness-wrapper`).

    $ grep -rniE 'dead-spawner|fileAnomaly|obs-sig|deadSpawnerMs' --include='*.go' .
    (no output)

`packages/agent/examples/observer.ts`, `packages/agent/src/observer.ts`,
`packages/agent/examples/prompts/observer.md`, `packages/queue/src/fleet.ts` and
`packages/fleet-db/src/types.ts` do not exist here. The only tree-wide hits for those terms are
this record and its predecessors under `docs/`. There is no bus, no fleet queue, no anomaly
detector, no verdict parser.

## Why it landed here anyway

Unchanged from
[§HARNESS-WRAPPER-23](../md/internal/out-of-scope-tickets.md#harness-wrapper-23--dead-spawner-re-alert-false-positive),
§HARNESS-WRAPPER-26 and §HARNESS-WRAPPER-99: the observer files a `dead-spawner` against the
*task* it is watching (`HARNESS-WRAPPER-109`, in this repo's fleet-db workspace), so the ticket
lands in this workspace even though every line of the defect lives in `orche`.

## Fix (summary; the applicable form is in the bundle)

Full patches, rationale and the test matrix are in
[`crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md`](../../crossrepo/orche/HARNESS-WRAPPER-111-observer-verdict-parse.md).
In brief:

- **Patch A — read the verdict by line, not by substring.** Extract an exported pure
  `verdictFor(reply, sig)` helper that strips leading markdown decoration
  (`/^[\s>#*_+\-•\`]*/`), anchors `^(CONFIRM|DISMISS)\b` at what remains, and requires the line
  to contain `sig`. Use it at both `:181` and `:237`. Anchoring at line start *after decoration
  only* is what keeps prose such as "a DISMISS on \`sig\` would be wrong" from matching — the
  one direction that could suppress a genuine anomaly. Absence of a verdict still files, exactly
  as `:170-173` intends.
- **Patch B — ownership grounding**, i.e. HARNESS-WRAPPER-99 Fix #1, landed alongside: drop a
  `dead-spawner` whose re-fetched `task.assignee !== a.facts.agentId`. Strict subset, no new
  I/O, and it alone would have suppressed this signature. Requires giving the
  `observer.unit.test.ts:725-736` fixture a matching `assignee` or it starts failing.
- **Do not carry over HARNESS-WRAPPER-99's Fix #2 verbatim** — its event-age freshness guard
  blinds the detector in exactly the fleet-wide outage it exists to catch. If drain lag is
  addressed in the same pass, take only the drain-to-empty loop (bounded iteration cap, `stderr`
  warn on cap; a silent cap reproduces the bug in a new shape) and derive freshness from
  *whether the drain reached empty*, not from event age. Full argument in
  [HARNESS-WRAPPER-99](HARNESS-WRAPPER-99.md#fix-2-is-unsound-as-specified--must-not-land-verbatim).

## Resolution

No source change in this repository — there is nothing here to change. No human gate is
requested: the in-repo change is documentation only and the root cause is unambiguous.

1. Hand the bundle to `orche`; it must be committed / PR'd there under its own ticket, as a
   follow-up to ORCHE-130.
2. Once handed off, the correct disposition for this ticket in this workspace is
   **close-as-invalid**. HARNESS-WRAPPER-24 (2026-07-16) already directed that this class not be
   re-filed here; this is the fourth recurrence.
3. **Carried forward for a human — not a code defect.** HARNESS-WRAPPER-79's decomposition
   children keep exceeding run deadlines (`-79` timed out twice; `-109` has now burned
   `timeout-attempt-1`). Two more and the task parks at `blocked`/`stuck` per
   `MAX_CONSECUTIVE_TIMEOUTS` (`packages/agent/src/finalize.ts:60`). That is a
   task-decomposition question, out of scope for this fix, and it is the one item this triage
   could not settle.
</content>
</invoke>
