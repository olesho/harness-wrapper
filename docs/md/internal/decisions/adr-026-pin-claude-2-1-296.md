# ADR-026: the claude pin moves to 2.1.296; its auto-mode nudge is a dialog of its own

**Status:** Accepted (2026-10-10)

**Intent:** principle 1, *the screen is a contract we don't own*, and principle 6, *evolve public
contracts deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 1, a
startup dialog a new release paints is read as a dialog before any prompt is typed into it. Under
principle 6, the pin moves once every live test speaks for it, and every Session the old pin saved
stays loadable.

## Context

The claude pin stood at 2.1.283, held there by [ADR-019](adr-019-pin-codex-0-160.md) because
`TestRunTurn_RealClaudeLargePromptIntact` failed on 2.1.289. Those failures were the test's own:
in about one run in four, on 2.1.283 as on later releases, claude reports that the paste arrived
whole instead of answering it, and the screen wraps a long reply across the expected words. The
test was fixed on 2026-10-10; it passes on 2.1.283, 2.1.289 and 2.1.296, and no run left the paste's
first line in the composer. npm's latest was 2.1.296.

2.1.296 passes the stream-json and screen profiles' conformance suites, the load fixtures, the
real-claude RunTurn tests and the chat live tests, but not the interrupt and question live tests.
At startup, when the user settings name a `defaultMode`, it offers once to make auto mode the
default permission mode: *Make auto mode your default permission mode?*, with *Yes, set auto mode as
my default permission mode* highlighted and *No, keep <that mode>* below it. The offer paints about
100 ms after the composer, so the prompt the screen driver types lands in it. Its Enter confirms
*Yes*, which writes `defaultMode: "auto"` into the user's settings and loses the prompt. Answering
either way records `hasSeenAutoDefaultNudge: true` in `.claude.json`, and claude does not offer it
again.

## Decision

1. **The pin moves** to 2.1.296, in `pkg/versions/versions.json` and the vendored meta-harness
   snapshot, and CI's real-claude job installs it.
2. **2.1.283 stays a load source.** The Session 2.1.283 saved is kept, and 2.1.296 loads it;
   2.1.296's own saved Session is recorded beside it.
3. **The nudge is a dialog of its own kind**, `auto_mode_nudge` (`claudecode.KindAutoModeNudge`).
   The screen driver never types a prompt into it, and a policy can answer it apart from folder trust
   and bypass acceptance. Its options alias `proceed` (*Yes*) and `deny` (*No, keep …*).
4. **The unattended policy declines it.** `oneshot.UnattendedInputPolicy` answers `deny`;
   `AutoAcceptAnswer`, which answers what the policy does not name, would accept it.
5. **The configurations hw writes mark the nudge seen.** The Claude Code profile's `.claude.json`
   and the live tests' seeded configurations set `hasSeenAutoDefaultNudge`, so claude never offers
   it there.

## Alternatives

- **Accept the nudge unattended, as the other startup dialogs are accepted.** Turned down: it changes
  the user's settings, which no launch should do as a side effect.
- **Hold the pin at 2.1.283.** Turned down: the hold rested on a test fault, and the nudge has a
  fix.

## Consequences

- A launch with a user's own configuration that names a `defaultMode` and has not seen the nudge
  loses its first prompt to it: the turn fails rather than going through. With a policy or a client
  that answers the request, that happens once per configuration; without one, `Send` returns
  `ErrInputPending`. The user's settings are never changed unless the answer is `proceed`.
- Verified on 2.1.296 (macOS arm64): the stream-json and screen profiles' conformance suites,
  including `MaxSessions` side by side and the version policy against a non-pin claude; the load
  fixtures; `TestRunTurn_RealClaude*` (`LargePromptIntact` 8 runs of 8);
  `TestTrustDialogLive`, `TestSessionAssignedLive`, `TestKeepAliveLive`, `TestStreamLive`,
  `TestInterruptLive`, `TestQuestionLive`; and with a real account, `TestStreamAccountLive`,
  `TestQuestionAccountLive`, `TestToolHooksLive` and `TestSubagentHooksLive`.
  `TestRunTurn_RealClaudeUntrustedDirSurfacesTrustDialog` needs a config dir with stored
  credentials and was not run; `TestTrustDialogLive` covers the folder-trust dialog.
