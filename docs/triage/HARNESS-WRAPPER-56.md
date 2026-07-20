# HARNESS-WRAPPER-56 — Triage record: false positive, no code change

**Ticket:** `[observer] release branch behind base (dev..main) — not promoting (release-lag:dev..main)`
**Verdict:** Dismissed as a false positive. No file in harness-wrapper participates in the
reported behavior, so this repository receives no code change; this document is the only
deliverable and records the triage for future observers of the ticket history.

## What was reported

On 2026-07-20 the orche observer filed a critical `release-lag:dev..main` anomaly claiming
`main` was 10 commits behind `dev` with the oldest unpromoted commit waiting ~4367 minutes
(~3 days).

## What actually happened

The lagging commits carried committer dates of Jul 17–18 but only *arrived* on the hub's
`dev` ref on 2026-07-20 21:39 — one minute before the observer sweep — via a supervisor
checkout↔hub sync. The release cron promoted them minutes later.

Verified at triage time in the hub repo (`/Users/oleh/repos/harness-wrapper.git`):

- `git rev-list --count main..dev` → `0`; both `dev` and `main` at `7f90e06`
  (2026-07-20 21:45). Promotion was healthy.
- Re-verified at implementation time (this commit): `main` is still at `7f90e06`; `dev` is
  ahead only by freshly-merged HARNESS-WRAPPER-46 work awaiting the next routine promotion.
  There is no stalled release.

## Root cause (lives outside this repository)

The defect is a detector weakness in the **orche observer codebase**,
`packages/agent/src/release-watch.ts` (`readReleaseLag`, lines 128–150):

- Lag age is derived from the oldest unpromoted first-parent commit's `%ct`, clamped only
  to the *release tip's* committer time (`arrivalCt = max(ct, releaseCt)` at line 149).
- That clamp handles the inverted-merge case (stale-dated commits older than the release
  tip). It cannot catch **late-arriving commits whose committer times fall after the
  release tip but long before they were pushed to the hub** — commit time ≠ hub-arrival
  time. The oldest unpromoted commit here (`7e43800`, %ct Jul 17 20:54) postdated `main`'s
  tip committer time (Jul 17 20:18), so the clamp was a no-op and the raw three-day-old
  committer time was reported as lag age, instantly exceeding the `releaseLagMin` floor.

## Recommended follow-up (separate ticket, orche workspace — not here)

If detector hardening is wanted, `readReleaseLag` should additionally floor the lag age at
the time the *base* ref last advanced — e.g. `git reflog show --format=%ct -1 <base>` on
the hub repo (or a persisted last-seen base tip + timestamp in observer state), taking
`arrivalCt = max(ct, releaseCt, baseLastMovedCt)`. Genuine stalls still report (base
hasn't moved recently relative to the lag floor); late-synced batches reset to actual
arrival time. Tests there should cover: old-`%ct` commits pushed "now" (reflog entry newer
than commit dates) yielding `oldestUnpromotedMs` ≈ arrival time, alongside the existing
inverted-merge and genuine-lag cases. Fail-safe: an unreadable reflog must fall back to
current behavior and never invent lag.
