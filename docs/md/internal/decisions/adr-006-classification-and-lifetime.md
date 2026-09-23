# ADR-006: A classification ends the harness only when the caller leaves its lifetime to the wrapper

**Status:** Accepted (2026-09-23)

**Intent:** principle 2, *a wrong verdict is worse than no verdict*
([INTENT](../../../../INTENT.md#design-principles)) — a caller that owns the harness's lifetime is
told what the output shows and decides; the wrapper does not act on a verdict it cannot be sure of.
Also the [Boundaries](../../../../INTENT.md#boundaries): the wrapper reports what happened "precisely
enough for them to decide", and here it had been deciding.

## Context

`wrapper.Session` ended the run on every terminal classification: `awaitTermination` SIGTERMed the
harness the moment the classifier returned `blocked_by_cost` or `retry_later`. After `IdleClassify`
(60 s) without output, the per-harness classifier lower-cases the last 64 KB and substring-matches its
phrase arms — `rate limit`, `usage limit`, `resets at`, `please try again`, `network error`,
`fetch failed`, … — and every hit is terminal.

That welds diagnosis to lifetime: a verdict was also the decision to end the process. It fits
run-to-completion supervision — `wrapper.Run`, `harness.RunTurn`, oneshot and structured-run, where a
stuck run must still end in a verdict. It does not fit a caller that keeps the harness alive between
messages. There, silence is the resting state: Claude Code writes little while it sits at its
composer, and nothing while a turn waits on a background command. `chat.Open` built its `wrapper.Config` with
no way to say so, so a conversation idle for a minute was killed whenever its recent output happened
to contain a phrase — and with no turn in flight chat dropped the event, so the caller was never told.

## Decision

`wrapper.Config.KeepAliveOnClassification` says the caller owns the harness's lifetime. The wrapper
then reports and never enforces:

1. **No verdict signals the harness.** A terminal classification is recorded like a non-terminal one —
   in the Snapshot, as a `SessionEvent` with `Terminated: false`, and in the trace (`enforced: false`).
   Only Stop, context cancellation or the harness's own exit end the process.
2. **Silence is not evidence.** `ClassifierInput.Idle` is never set, mid-run or in the exit pass, so the
   idle-gated cost, retry and transport phrase arms never run and `IdleClassify` has no effect. The
   anchored API-error and session-limit matchers and the quiet-gated prompt matcher remain.
3. **A verdict consumes its evidence.** A pass runs only when new output arrived or the output went
   quiet since the previous one, and reads only what was written after the last verdict, widened back
   to the start of the line it began in, so a line split across writes is read whole. A whole-window
   scan keeps returning the oldest match: an old `API Error` line, which is checked first, masked a
   later session-limit banner for as long as it stayed in the window.
4. **The status tracks the evidence.** A pass that finds a verdict records it; a pass over new output
   that finds nothing clears `Snapshot().Status` to empty, which the Snapshot contract already defines
   as "producing output, unclassified". No event marks the clearing. A caller that reads the status
   never sees a verdict the harness has moved past.
5. **The Result describes how the process ended.** `Result.Status` and `Reason` are the exit's —
   idle, failed or interrupted, never a classification. A failed exit's `Class` is the exit pass's
   verdict over what came after the last verdict, or else that verdict's class if the harness wrote
   nothing after it.

The zero value keeps run-to-completion behaviour for every existing caller.

In every mode, a negative `IdleQuiet` or `IdleClassify` is refused with `ErrInvalidConfig`. Both used to
pass validation and be kept (defaults replace only zero). A negative `IdleClassify` makes every tick
idle, so any phrase kills at once — and it is the value a caller reaches for, by analogy with
`StaleThreshold: -1`, to switch the kill off.

## Alternatives

- **Tightening or anchoring the phrase list.** A real wall would still kill a conversation that should
  wait for it to reset, and any prose list stays open to the adversarial cases principle 2 names — the
  assistant quoting a marker.
- **A threshold and classifier passthrough on `chat.Options`.** A caller could only lengthen the fuse,
  and a passed-through `IdleClassify` exposes the negative-value trap above.
- **Keep-alive as the default.** `harness.RunTurn`, oneshot, structured-run and the `run` CLI are
  run-to-completion callers, and they depend on a stuck run ending in a verdict.
- **A caller-side workaround** — treat the kill as a park and relaunch with `--resume`. It gives up the
  warm process a long-lived conversation exists for, and leaves every other caller exposed.

## Evidence

- Reproduced on v0.12.0 with a scripted harness whose last line is "I added rate limiting to the login
  endpoint.": through `chat.Open` with its defaults the harness was SIGTERMed as
  `blocked_by_cost: rate limit` at 65 s, no chat event arrived, and `Events()` did not close.
- The claude classifier returned a terminal verdict for both of chat's not-a-wall rows
  (`pkg/chat/usage_limit_test.go`) and for four of five ordinary coding-agent lines: a reply about rate
  limiting, a diff adding `"Something went wrong, please try again"`, a test log with
  `TypeError: fetch failed`, and "the counter resets at midnight". `TestKeepAlive_AdversarialRowsNeverClassify`
  runs all six through both modes.
- It fired on a working agent in loom's fleet: on 2026-09-03 a worker implementing a rate limiter went
  quiet while a build ran in the background, the arm matched `rate-limit` in its own output, and the
  resulting account wall parked seven agents for fifteen minutes.
- claude 2.1.280 paints a usage notice a few seconds after a turn when the account is near its limit
  ("You've used 92% of your weekly limit · resets 6pm"). It names a limit and a reset time but is no
  wall; `TestUsageWarningIsNotAWall` pins that none of the matchers reads it as one.

## Consequences

- A keep-alive caller must end the harness itself; nothing the output says will.
- A keep-alive caller no longer learns of a transport failure from the phrase arms. It learns of it
  from the harness's own record (the transcript) or from the harness exiting.
- Classification in keep-alive mode reads only new output, so its cost scales with what the harness
  writes, not with the size of the window.

## Follow-ups

- meta-harness, the TypeScript twin, carries the same phrase arms (`src/wrapper/internal/harness/claude.ts`).
  Principle 6 wants the option and this record mirrored there.
- The default mode keeps its phrase arms, and the 2026-09-03 wall shows they misfire there too. Either
  run-to-completion callers opt in (loom's leads and workers, with a stuck run ending at the caller's
  turn deadline instead), or the arms are retired in favour of the harness's own transcript tags, as
  loom retired its screen-scrape wall detector.
- `Result.ExitCode` is -1 for a signalled harness, although its documentation promises 128+signum.
