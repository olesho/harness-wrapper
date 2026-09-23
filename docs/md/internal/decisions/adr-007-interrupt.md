# ADR-007: An interrupt is a conversation operation, not a keystroke

**Status:** Accepted (2026-09-23)

**Intent:** principle 2, *a wrong verdict is worse than no verdict*
([INTENT](../../../../INTENT.md#design-principles)) — a turn ends interrupted only when the harness says
it stopped, and an interrupt that could not be confirmed says so; principle 1, *the screen is a
contract we don't own* — every shape below was captured on the pinned claude before code keyed on it.
It also applies 3 (the keys and the screen reading stay in the adapter; callers get one turn state),
4 (the partial reply comes from the transcript once it records the interrupt) and 6 (a new route and
wire state, added with their fixtures).

## Context

harness-wrapper had no interrupt. A caller wrote Esc through `Wrapper().WriteStdin`, and then:

- **An Esc before Claude's first token** cancels the turn and puts the prompt back in the composer
  with nothing painted. chat's idle fallback then ended the turn by guesswork — errored "prompt not
  accepted" on a fresh conversation, complete with stale text when an older reply was on screen — and
  the restored prompt stayed in the composer, where the next Send's text was appended to it.
- **An Esc between Send's text and its submit key** lands in the composer, not on the turn.
- **claudecode read an interrupt by the marker's presence** (`lastInterruptSeen`). A second interrupt
  while the first marker was still on screen went unseen, the idle fallback then completed the turn
  with its partial text, and the flag flipped with no turn in flight.
- **An interrupt that takes the control token deadlocks**: `RunTurn` holds the token for the whole
  turn.
- `TurnCode` is wall-only by contract, and ADR-002 keeps `turns.Kind` closed (`Errored` plus a
  `Reason` at the adapter layer), so neither could carry "interrupted" without a new contract.

## Decision

**An interrupt needs no control token, never lands inside a submit, and ends the turn only on the
harness's acknowledgement. An interrupted turn has its own terminal state, `TurnStateInterrupted` —
neither a success nor a failure.**

1. **`turns.Interrupter`**, an adapter capability:
   - `InterruptSequence()` — the keys, written as one write;
   - `InterruptOutcome(prompt, snap)` — `Pending`; `Stopped` (the interrupt marker below this turn's
     prompt echo, the partial reply being the text above it, returned with it); `Cancelled` (the
     composer holds this prompt: the harness put it back); `Finished` (this turn's own end-of-turn
     marker);
   - `ComposerText(snap)` — what the composer holds, or not readable;
   - `ClearComposerSequence(composer)` — keys that empty it from wherever its cursor is.

   The reading is per turn: everything is read below this turn's prompt echo — the last echo on
   screen, when it is this prompt's — so an earlier turn's marker, which stays painted above, never
   speaks for it, and a second interrupt is seen. With no echo on screen the turn's has scrolled off
   with every earlier one, and the whole conversation is this turn's. claudecode's `OnScreen` emits
   nothing for the marker.
2. **`Conversation.Interrupt(ctx) (InterruptResult, error)`.** With no turn in flight it returns
   `InterruptNoTurn` and writes nothing. Otherwise it takes the submit lock Send holds from its
   composer check to its submit key, waits until the harness has taken the prompt — the screen showed
   it working after the submit began; before that the composer holds the prompt as typed, which is
   also what a cancel leaves — and, while the turn still runs, writes the keys once. A concurrent
   Interrupt for the same turn joins it. Then:
   - `Stopped`: the turn ends `TurnStateInterrupted` with its partial reply — the transcript's, once it
     has recorded the interrupt (a user entry after the turn's replies), else the screen's; a tool call
     is not reply text — and Interrupt returns `InterruptStopped`;
   - `Cancelled`: the turn ends `TurnStateInterrupted` with no text, chat empties the composer with
     `ClearComposerSequence`, re-reading it until it reads empty, and Interrupt returns
     `InterruptCancelled` — with `ErrComposerNotCleared` if it would not clear;
   - `Finished`: no keys are written, the turn keeps its own outcome, and Interrupt returns
     `InterruptTooLate`;
   - ctx ends first: `ErrInterruptUnconfirmed`, and the turn stays in flight.

   `ErrInterruptUnsupported` for an adapter without the capability — codex until its captures exist.
3. **An interrupt at the terminal ends the turn the same way.** chat reads every repaint of a taken
   turn, so a person pressing Esc at the TUI ends it `TurnStateInterrupted`, its reason saying
   "(at the terminal)". While an Interrupt's keys await the harness's answer, the idle fallback does
   not complete the turn — the settled screen shows a reply cut short; the harness's own end-of-turn
   marker still does.
4. **Send types into an empty composer only.** Under the same lock it reads the composer and empties
   whatever it holds — a prompt a cancel put back, a draft typed at the terminal — with the same keys.
   A composer that will not clear gets `ErrComposerNotCleared`, nothing typed and no turn recorded; a
   composer the adapter cannot read is typed into as before.
5. **The state crosses every layer.** `TurnStateInterrupted` carries the partial reply in `Text` and,
   in `Reason`, whether the harness stopped or cancelled the turn and who interrupted it; `Code` stays
   empty. `RunTurn` ends with `ErrTurnInterrupted`, which wraps `ErrTurnErrored`. chatd serves
   `POST /v1/conversations/{id}/interrupt` without a token, like `DELETE`, answering
   `{"result": "stopped" | "cancelled" | "no_turn" | "too_late"}` (plus `error` beside a composer that
   would not clear), 501 `interrupt_unsupported` and 504 `interrupt_unconfirmed`; a Send refused over a
   composer that would not clear is 409 `composer_not_cleared`; `interrupted` is a wire turn state.
6. **claude's keys.** The interrupt is Esc in the kitty keyboard protocol, CSI 27 u, which claude's TUI
   runs (the reason its Enter is CSI 13 u). The composer is emptied with Ctrl-E, then Ctrl-K twice per
   line, then Ctrl-U twice per line and once more.

## Alternatives

- **`CodeInterrupted`.** `TurnCode` is wall-only by contract; an interrupt is not a wall.
- **`Errored` with a canonical reason.** Consumers would substring-match the reason, which `TurnCode`
  exists to prevent, and an interrupt is not a failure.
- **An Interrupt that takes the control token.** It deadlocks against `RunTurn`, which holds the token
  for the whole turn.
- **Reading the marker by presence, or counting markers.** Presence misses a second interrupt while the
  first marker is painted; a count stays the same when an old marker scrolls off in the frame the new
  one lands.
- **Comparing against a snapshot taken when the keys went out.** It cannot say which marker belongs to
  which turn once the screen scrolls, and the prompt identifies the turn's echo on any frame. Taken
  from the moment the harness showed the turn working, the prompt is all the reading needs.
- **A lone 0x1b.** It interrupts too (captured), but a lone ESC is also the first byte of every escape
  sequence: claude holds it for its escape timeout before calling it a key (~90 ms against ~30 ms),
  and a write landing inside that window can fuse with it into an Alt chord.
- **Clearing with Esc Esc, or Ctrl-C.** Two Escs on a composer holding text clear it, but on an empty
  one they open claude's Rewind picker, whose `❯ (current)` row reads as a ready prompt. Ctrl-C clears
  a composer, but pressed twice it quits claude: no key that can end the session belongs in a clear
  that runs on a reading which may be a frame old.
- **Sending the interrupt again when it goes unanswered.** A second Esc within about a second of the
  first on an idle, empty composer opens the Rewind picker.

## Evidence

Captured on claude 2.1.280 driven through chat against a local Messages API (`ANTHROPIC_BASE_URL`,
a placeholder token): a stream of words 150 ms apart, a first token six seconds late, a Bash tool call.
Recorded as `test/corpus/claude-code/interrupt-mid-reply`, `interrupt-mid-tool`,
`interrupt-before-first-token`, `interrupt-second-turn` and `rewind-picker` by
`HW_RECORD_INTERRUPT=1 go test ./pkg/chat -run RecordInterrupt`; `HW_LIVE_INTERRUPT=1 go test ./pkg/chat
-run InterruptLive` runs the same scenarios and passes.

| Shape | Screen | Transcript |
|---|---|---|
| Esc mid-reply | `⎿  Interrupted · What should Claude do instead?` below the partial reply 30–90 ms later; composer empty | the partial reply, then a user entry `[Request interrupted by user]` |
| Esc mid-tool | the marker in place of the tool's `⎿  Running…` | the tool call, a rejected `tool_result`, `[Request interrupted by user for tool use]` |
| Esc before the first token | no marker; the prompt's echo goes, the prompt is back in the composer (a >1 KB paste comes back expanded and can overflow the screen) | only the prompt's user entry |
| a second Esc right after an interrupt | nothing | — |
| two Escs < 1 s apart on an idle, empty composer | the Rewind picker ("Enter to continue · Esc to cancel"); one more Esc closes it | — |
| one Esc after a finished turn | nothing | — |
| Esc on a composer holding text | "Esc again to clear"; a second within 0.8 s (not 1.0 s) clears it | — |
| CSI 27 u instead of 0x1b | the same, mid-reply, mid-tool and before the first token | — |

Composer keys: Ctrl-U kills to the start of the line, and on an empty line joins it to the one above,
so n lines take 2n−1 presses from the end; Ctrl-E moves to the end of the line; Ctrl-K kills to the end
and joins the next line; Ctrl-End and Meta-> do not move the cursor. Ctrl-U does nothing on an empty
composer, and Ctrl-E, Ctrl-K×6, Ctrl-U×7 emptied a three-line composer from its middle line — spare
presses past the text do nothing either, which the before-first-token recording's clear shows.

The recordings replay in 64-byte chunks, which cut claude's `❯` across writes; `pkg/screen` dropped a
character cut that way (harness-wrapper #85, which this record relies on).

## Consequences

- A consumer that switches on turn states must handle `interrupted`: meta-harness's closed union,
  chatd clients, loom. `RunTurn` callers get it through `ErrTurnErrored` unless they ask for
  `ErrTurnInterrupted`.
- History read from claude's transcript includes its `[Request interrupted by user]` user entries.
- An Interrupt for a turn the harness never showed working — a swallowed prompt, a turn that finished
  within one frame — writes nothing, and ends `InterruptTooLate` or `ErrInterruptUnconfirmed`.
- Send reads the composer before every prompt, and a draft someone typed at the terminal is cleared
  rather than sent with it.

## Follow-ups

- codex implements `Interrupter` once its interrupt is captured; the only recorded codex marker is in a
  meta-harness probe.
- `readyForInput` calls the Rewind picker ready (its `❯ (current)` row); it should refuse it.
- meta-harness mirrors `Interrupter`, `TurnStateInterrupted`, the route and the conformance fixtures.
