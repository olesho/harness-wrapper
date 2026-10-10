# ADR-025: claude's clarifying questions are read, and answered by a plan of confirmed steps

**Status:** Accepted (2026-10-10). It extends [ADR-002](adr-002-interactive-input.md): a dialog is
still answered semantically, and the chat layer still owns the keystrokes and confirms them on the
screen, but an adapter may now plan an answer that takes several writes.

**Intent:** principle 1, *the screen is a contract we don't own*, principle 2, *a wrong verdict is
worse than no verdict*, principle 3, *normalize, don't leak*, and principle 6, *evolve public
contracts deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 1, the
question dialog is pattern-matched against one release (2.1.283), and a restyle makes detection go
silent rather than wrong. Under principle 2, keys go only to the pane a plan was made for, a step is
never re-sent, and an answer that does not land fails bounded instead of guessing. Under principle 3,
a client answers by option, alias or text; the keys stay server-side. Under principle 6, every change
is additive: two request kinds, two server-side option fields, two option aliases, and one optional
adapter interface.

## Context

When the model calls AskUserQuestion, Claude Code paints a dialog where its composer was: a tab strip
(☐/☒ per question, and ✔ Submit on a multi-question or multi-select one), the question, numbered
options with descriptions, a "Type something" text field, "Chat about this", and a footer starting
"Enter to select ·". A multi-question or multi-select dialog ends on a review pane, "Ready to submit
your answers?", with Submit answers and Cancel. Nothing read it. With no busy marker, no end-of-turn
marker and no composer, the screen read as ready, and the chat layer completed the turn as a prompt
claude had swallowed — "prompt not accepted / no assistant output" — while the question sat
unanswered. [#16](https://github.com/olesho/harness-wrapper/pull/16) detected the dialog on 2.1.210,
when `pkg/chat` was not yet `internal/chatcore`, and was never merged.

Measured on 2.1.283 against a local Messages API that asks (`internal/chatcore/question_record_test.go`),
an answer is not one keystroke:

- A digit on an ordinary single-select row answers the question, or moves the dialog to its next
  question. On a multi-select row it toggles the checkbox and leaves the highlight where it was; Tab
  then opens the review pane.
- "Type something" is a text field. Its digit (on a multi-select question, the arrows) only moves the
  highlight onto it; typing replaces its placeholder; Enter answers with the text, and Enter with no
  text declines the whole question. On a multi-select question, Down moves from the field to the
  Submit row, whose Enter opens the review pane.
- "Chat about this" answers on its digit: it declines the question, and claude goes on to ask what
  the user wants to clarify.
- A burst of plain keys in one write reaches claude as a paste: "3Green\r" answered "Red", and "13\t"
  answered nothing.
- A key written as a pane first paints is dropped: 3 times in 3 at 0 ms after the footer painted,
  once in 3 at 50 ms, never from 100 ms.

And claude repaints a pane in place, a line at a time, so a frame read mid-paint can mix a pane with
the one it replaces ("Which size do you need?" over the last question's rows).

## Decision

1. **The claude adapter reads both panes** (`pkg/turns/harness/claudecode/question.go`) as
   `turns.InputRequest`s of kind `question` and `question_review`, after its startup dialogs. A
   question carries its tab label as `Header`, `MultiSelect`, and each option's description. The text
   field is the option with alias `other`, "Chat about this" the one with alias `chat`. Two
   server-side option fields join `Highlighted`: `Checked` (a ticked checkbox) and `Typed` (the text
   in the field, whose `Label` stays the placeholder). None of the three is in the request id, so
   toggling, typing and moving the highlight do not make a new request.
2. **An adapter may plan an answer** (`turns.AnswerPlanner`): `PlanAnswer` returns `AnswerStep`s,
   each a write and the evidence it landed (`EvidenceGone`, `Highlighted`, `Unhighlighted`, `Checked`,
   `Typed`), and the first may say how long the pane must have been up (`After`; claude's is 300 ms).
   An answer the dialog cannot take — text without the `other` option, `other` without text, two
   options on a single-select question — is an error wrapping `turns.ErrInvalidAnswer`, before
   anything is written.
3. **The chat layer runs a plan** (`internal/chatcore/answer_plan.go`), for a client's `Answer`, an
   `OnInputRequest` answer and an `InputPolicy` disposition alike. Before each write it waits for the
   step's `After` (from when the request was raised) and for the screen to parse as the request's own
   dialog; after it, for the step's evidence, read back through `DialogReader`. It never re-sends a
   step; a dialog or evidence that does not appear within the render budget ends the answer with a
   bounded `*InputUnresolvedError`. A dialog is gone when another parses in its place, or when nothing
   parses and no dialog anchor is painted — never by its prompt, which claude prints into the
   conversation once answered.

## Alternatives

- **Port #16 as it was.** Its keys were measured on 2.1.210 and written in one burst; on 2.1.283 a
  burst is a paste, the text field is not an escape hatch, and "Chat about this" needs no Enter.
- **Extend `answerAndConfirm`.** Its rules are for one key and a highlight; a question needs
  toggles, text and a commit, each with its own evidence. The plan keeps those rules — positive
  evidence, no re-send, a bounded failure — and moves the harness's key knowledge into the adapter.
- **Skip frames read mid-paint.** No frame-level signal marks a finished one: claude sends no
  synchronized-output markers, and parks the cursor on the focused row, which a repaint also visits.
  A read of the terminal mostly delivers a whole update, and live no session raised a request it
  should not have (`TestQuestionLive`, every request asserted). A frame read mid-paint may still raise
  a short-lived request; it is resolved at once, and an answer to it writes nothing, since its pane is
  never the one on screen.

## Boundary

- claude-code over the screen transport. The stream-json transport, and agentd's agents, do not offer
  the AskUserQuestion tool at all: the model asks in plain text and ends its turn.
- Re-verify on every claude bump: `HW_LIVE_QUESTION=1 go test ./internal/chatcore -run QuestionLive`
  with the pinned claude on PATH (a local API asks; no account), re-record with
  `HW_RECORD_QUESTION=1 … -run RecordQuestion`, and run the real-account check
  (`HW_LIVE_ACCOUNT=1 HW_LIVE_TOKEN_FILE=… -run QuestionAccountLive`). See
  [Versions & Drift](../versions-drift.md#shapes-the-canonical-scenarios-miss).

## History

- 2026-10-10: accepted with claude-code 2.1.283.
