# ADR-008: Events are delivered in order, bounded, and end with the exit

**Status:** Accepted (2026-09-23)

**Intent:** principle 2, *a wrong verdict is worse than no verdict*
([INTENT](../../../../INTENT.md#design-principles)) — a consumer that never hears a turn end, or the
harness exit, acts on a conversation that is not there; principle 7, *say what is enforced, not what
is intended* — the promise is written down with its limits: in order and once while the process lives,
not durably. It also applies 6: `OnEvent` is added beside `Events()`, whose contract stays as it was.

## Context

A consumer that journals what a conversation does — agentd commits every event to its execution
journal — could not rely on the events arriving:

- chat's `emit` dropped an event when the `Events()` channel was full, and during `Close` it dropped
  the final turn event about half the time; an emit racing the channel's close could panic.
- The wrapper dropped its own events when its 16-slot channel was full, the final `Terminated` event
  included. chat's status pump was their only reader, so a stalled consumer lost the harness's exit
  silently.
- A harness that exited with no turn in flight produced no chat event at all, and `Events()` never
  closed before `Close`: the screen pump ends only when unsubscribed.
- Send marked the turn in flight before submitting but emitted `pending` after, so the watcher's
  terminal event could arrive first, and a failed submit emitted a second terminal event over the one
  already stored.
- The Store is no substitute: input events never touch it, error events follow a failed write, and the
  session-id capture writes it without an event.
- There was no `State` getter, and `Snapshot().Status` had no age.

## Decision

**Each event a conversation generates is delivered in order, once, to one callback, while the process
lives. The delivery is bounded, and it is not durable.**

1. **`wrapper.Config.OnEvent` and `chat.Options.OnEvent` (and `ReopenOptions.OnEvent`) are serial
   callbacks.** One worker per queue calls them, outside every lock of the wrapper and of the
   conversation — its state, its submit lock, its close. A callback must not call back into hw or wait
   for anything waiting on the method that produced the event. `Events()` stays, as the best-effort view
   of the same stream: it drops what its reader does not take.
2. **A bounded queue feeds each callback**: `EventQueue`, by default 1024 events and 16 MiB of
   payload. A producer that finds it full waits for room, holding no lock; a slow consumer can delay the
   harness's reports, never an `Interrupt`, a `Stop`, the exit or `Close`. The wrapper's final
   `Terminated` event and chat's `EventExited` never wait, and producers still waiting when they are
   queued are turned away rather than delivered after them. An event larger than the byte bound is
   delivered without its turn's text, carrying `ErrEventTooLarge`. The pressure — queued events and
   bytes, how long a producer has waited, how long the running callback has taken, counts delivered,
   dropped and oversized — is in `State().Delivery`.
3. **chat takes the wrapper's events through `wrapper.Config.OnEvent`**, maps them with
   `turns.StatusEvents`, and watches the screen with `turns.WatchScreen`; one event loop handles both.
4. **`EventExited` ends the stream.** It carries `ExitInfo` (status, exit code, signal, reason, error
   class, end time) and follows the terminal event of the turn that was in flight: a turn the harness's
   last event did not end is ended errored "harness exited", and every turn ending already claimed
   queues its event first. `Done()` closes when the process has ended; `Events()` closes once
   `EventExited` has been delivered — the two differ when delivery stalls. `Close(ctx)` waits for the
   drain until `ctx` ends, then drops the rest and returns `ErrUndelivered`. The delivery worker is the
   only writer of the `Events()` channel, so nothing closes it under a send. After the exit `Send`
   returns `ErrExited`.
5. **Send queues the user turn and the pending assistant turn before it types the prompt**, outside
   every lock: nothing can end a turn that was never submitted, so no terminal event precedes its
   pending one. A submit that fails ends the turn once — unless the exit ended it first. A turn can end,
   and its terminal event be delivered, before `Send` returns.
6. **`Conversation.State()`** reads the conversation in one call: alive and pid, the turn in flight,
   the pending input request, `Busy`, the last output, the wrapper's classification with
   `Snapshot().ClassifiedAt`, the harness session id, the exit, and the delivery pressure.
7. **chatd feeds its SSE fan-out from `OnEvent`**, sends `EventExited` as an `exited` frame with an
   `exit` object — the last frame of the stream — answers a message after it with 410 `exited`, and
   sends no frame for an event type it does not know, rather than dressing it as a turn.

## Alternatives

- **A lossless, blocking `Events()`.** A caller that never reads the channel — many only watch for a
  turn to end — would wedge its conversation, with no way to say it does not want the stream.
- **An unbounded queue.** A stalled consumer grows it without limit.
- **Dropping the oldest events at capacity.** It loses exactly the events a journal must commit, a
  terminal turn among them.
- **Durable delivery inside hw.** The wrapper owns no persistence; agentd journals what it is handed
  and replays the harness's transcript and spool for what a crash lost. A lost in-memory observation
  may stay unknown, and the promise says so.
- **A `turns.Kind` for the exit.** ADR-002 keeps the adapter vocabulary closed, and the exit is a fact
  about the process, not a signal an adapter reads off the screen.
- **Emitting `pending` after the submit, as before.** A fast reply's terminal event can overtake it.

## Boundary

Guaranteed while the process lives: each event reaches `OnEvent` once, in the order it was generated;
the callback runs outside hw's locks; memory is bounded by the queue's limits plus the event the
callback holds and the final event; `EventExited` is the last event and follows the in-flight turn's
terminal event; `Interrupt`, `Stop`, the exit and `Close` never wait for delivery; `Close` returns by
its context.

Not guaranteed: delivery across a crash of the process embedding hw; delivery of what is still queued
when `Close`'s context ends (dropped, counted, `ErrUndelivered`); `Events()` completeness. A callback
that never returns stops delivery — and, once the queue is full, the producers behind it — until
`Close` abandons the queue.

## Evidence

- `internal/delivery`: order over 500 events; producers wait at the event bound and at the byte bound;
  an event over the byte bound is refused; a waiting push gives up on its abort; the last push never
  waits and turns waiting producers away; abandon drops and counts.
- `pkg/wrapper`: a slow `OnEvent` sees every verdict and then `Terminated`, in order; a callback that
  never returns does not hold `Stop`; `ClassifiedAt`; negative bounds refused.
- `pkg/chat`, over the fake harness: a turn the harness died in delivers user, pending, errored, then
  `EventExited`, and `Events()` closes; an exit with no turn delivers `EventExited`, and `Send` then
  returns `ErrExited`; `Close` drains; a stalled consumer holds the queue at its bound while `Interrupt`
  answers and `Close` returns `ErrUndelivered` in time; an oversized event arrives without its text;
  `State`; a callback reading `State` does not deadlock; `Close` racing emits under `-race`; a harness
  dying mid-submit ends the turn once, before the exit; `Done` without the drain.
- `cmd/harness-chatd`: the `exited` frame is last, after the errored turn, and the stream ends.

## Consequences

- A consumer sees a turn `pending` before its prompt reaches the harness; a submit that fails turns it
  `errored` ("submit: …").
- `Close` can now return an error. `RunTurn` closes with a background context, so it waits for the
  drain; `Events()` is never the one to block it.
- `turns.Watch` is unchanged for its other callers; chat no longer uses it.
- The gateway's SSE stream ends when the harness exits.

## Follow-ups

- meta-harness mirrors `OnEvent`, `EventExited`, `State` and the `exited` frame; loom and agentd take
  events through `OnEvent`.
- chatd exposes no route for `State`; one can come when a remote consumer needs it.
