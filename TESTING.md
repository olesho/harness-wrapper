# Testing strategy

The wrapper's job is to drive interactive coding-agent TUIs (claude-code, codex)
over a PTY and report **turn boundaries** and **reply content** faithfully. The
hard part isn't our own API — it's that we screen-scrape tools we don't control,
and they change. This document describes how the suite is structured to keep
that contract stable, and how to extend it.

## The three contracts

1. **Outward** — the HTTP routes, JSON DTOs, CLI flags, and exported `pkg/chat`
   API. We own it; it breaks only when we edit it carelessly.
2. **Inward** — our assumptions about how claude-code/codex *render*:
   `✻ <Verb> for Ns`, `esc to interrupt`, `❯`, `›`, `Token usage:`, the CSI-13u
   submit, the double-Ctrl-C quit. We don't own it; it breaks silently whenever
   those tools ship a new version. (See the git log — most fixes chase exactly
   this.)
3. **Correspondence** — the actual promise to callers: *report turn boundaries
   correctly and hand back the real final reply, not a mid-turn preamble.* This
   is what the timing-sensitive completion logic defends, and the layer with the
   least natural coverage.

The trap: pattern unit tests + corpus replay are strong on (1) and (2)-at-rest,
which gives **false confidence** — a corpus recorded from `claude-code 2.1.x`
stays green forever, including the day a new version breaks every real user.

## The test pyramid

| Layer | Tests | Hermetic? | Cadence | Where |
|---|---|---|---|---|
| 0. API-freeze | HTTP routes + wire DTOs + CLI flags + exported `pkg/chat` Go API as golden snapshots | yes | per-commit | `cmd/harness-chatd/contract_test.go`, `cmd/harness-wrapper/contract_test.go`, `pkg/chat/contract_test.go` |
| 1. Pattern units | the adapter regexes / markers | yes | per-commit | `pkg/turns/harness/**`, `pkg/wrapper/internal/harness/**` |
| 2. Corpus replay | recorded byte-streams → adapter → boundaries+text | yes | per-commit | `pkg/turns/harness/**/*_test.go`, `test/corpus/**` |
| 3. **Fake-harness integration** | full PTY→screen→turns→chat→HTTP against a scriptable fake | yes | per-commit | `internal/fakeharness`, `cmd/fakeharness`, `pkg/chat/integration_test.go` |
| 4. **Live conformance + drift** | real installed binaries: version drift + sentinel round-trip | no | nightly | `pkg/harness/conformance_test.go` |

Layers 1–2 verify the adapter in isolation. Layer 3 (below) is the one that
exercises the **timing** machinery — the idle-completion watcher, the
marker-confirm gap, `Busy()` flicker — which adapter-level replay cannot reach
because it bypasses the conversation loop and its wall-clock timers.

## Layer 3: the scriptable fake harness

`cmd/fakeharness` is a **real binary** that `chat.Open` (and `harness.RunTurn`,
and the chatd HTTP server) spawn over a real `creack/pty`. Replaying a script
therefore drives the genuine `screen` emulator, `turns.Watch` pump, and
`idleCompletionWatcher` goroutines on real time. It is not a mock.

A test builds a `fakeharness.Script` with the fluent builder, marshals it to
JSON, and points the binary at it via the `FAKEHARNESS_SCRIPT` env var. The
binary switches its PTY slave to raw mode (as a real TUI does), then replays the
timeline: paint frames on a delay, block until the wrapper types an expected
byte sequence, optionally echo the captured prompt back.

### Why a real PTY matters

This layer regression-locks the quiescence/`Busy()` timing bugs (`3eda8a8`,
`dfc5aae`) and has already **found a new one**: a lost-wakeup race in
`waitReadyForSend` (it checked readiness, then subscribed — missing the
prompt-ready frame if it landed in between and the harness then went quiet).
Real harnesses repaint continuously and mask it; the fake paints its prompt once
and blocks, exposing it deterministically under `-race -count`. Fixed by
subscribing before the check.

The bugs this layer locks (`3eda8a8` quiescence, `dfc5aae` busy-during-subagent)
are **timing bugs in `pkg/chat`'s watcher goroutine**, not pattern bugs in the
adapter. Corpus tests feed bytes straight into `adapter.OnScreen`; the
`quiescence_test.go` unit tests call `maybeIdleComplete()` directly. Neither
runs the real timer goroutine end to end. A fake that replays "spinner flickers
off for one frame, then settles" through a real PTY is the regression test that
actually catches them — and the next timing regression automatically.

### Authoring a scenario

The builder (`internal/fakeharness/builder.go`) stamps the exact claude-code
glyph vocabulary, kept in one place so a future TUI drift updates fixtures and
adapter patterns together:

```go
script := fakeharness.New("claude-code").
    Session("…uuid…").                                   // resume hint for session-id extraction
    Idle().                                              // ready composer (must be first; Send's gate waits for it)
    AwaitSubmit().                                       // block until CSI 13u; capture the typed prompt
    Working(30, "Cerebrating").                          // busy frame (spinner + "esc to interrupt")
    MarkerFlicker(30, "Pondered", "3s", "drafting").     // ✻ marker on a NON-busy frame, mid-turn ← the trap
    Working(30, "Exploring").                            // spinner returns: work continues
    Reply(40, "Answer: "+fakeharness.PromptRef(), "Synthesized", "12s"). // settled end-of-turn (echoes prompt)
    ExitOnQuit().                                        // exit on double-Ctrl-C so RunTurn's quit is prompt
    Build()
```

Frame vocabulary (all take a leading `delayMs` — wall-clock sleep before
painting, which is how cadence is reproduced):

| Builder method | Frame | `Busy()` | Fires marker? |
|---|---|---|---|
| `Idle()` | ready composer (`Claude Code` + `❯`) | no | no |
| `Working(d, status)` | spinner + `esc to interrupt` | **yes** | no |
| `Marker(d, verb, dur)` | `✻ verb for dur`, still busy | **yes** | yes (intermediate) |
| `MarkerFlicker(d, verb, dur, note)` | `✻ verb for dur`, footer flickered off | no | yes (the trap) |
| `Flicker(d, note)` | sub-agent line, no spinner/footer | no | no |
| `Reply(d, body, verb, dur)` | bullet + final `✻` + settled, echoes prompt | no | yes (final) |
| `SettleIdle(d, body)` | settled bullet, **no** `✻` marker | no | no |
| `Raw(d, text)` | verbatim line for the wrapper's line classifier (e.g. `API Error: 429 …`) | n/a | n/a |

Input/lifecycle: `AwaitSubmit()` (CSI 13u, captures prompt), `AwaitMenuChoice()`
(digit+CR), `Exit(code)`, `ExitOnQuit()` (double-Ctrl-C → exit 0). `PromptRef()`
returns the placeholder substituted with the captured prompt in any echoed frame.

**Codex** uses a different completion model — no `Busy()`, no quiescence: the
chat layer completes a turn the instant a fresh `Token usage: …` footer appears.
`New("codex")` switches the vocabulary; `Idle()` paints the `›` composer, and
`CodexWorking(d, status)` / `CodexReply(d, body)` paint the in-flight and
end-of-turn frames (the latter carries a per-call-distinct token-usage footer,
since codex dedupes by exact footer text). Submit is still CSI 13u, so
`AwaitSubmit()` pins that contract for codex too. See the `TestIntegration_Codex_*`
scenarios.

### Driving it

- **`pkg/chat`** (timing scenarios): `openFake(t, script)` returns an open
  `Conversation` with shrunk completion windows (`testIdleGap` /
  `testMarkerGap`, applied per-`Conversation` via unexported `Options` overrides
  — not a global, so the watcher goroutines stay race-free). Then `sendOneTurn`
  + `waitForTerminalTurn`.
- **`pkg/harness` / `cmd/harness-chatd`** (wrapper/HTTP scenarios): `fakeBin(t)`
  + `scriptEnv(t, script)`; pass the env to `RunTurn`/the HTTP request. These use
  the **default** 2s/8s gaps (real RunTurn behavior), so per-turn latency is ~2s.

The binary is built once per test process via `fakeharness.BuildOnce()` and
cleaned up in each package's `TestMain` via `fakeharness.Cleanup()`. Tests
`t.Skip` if the Go toolchain is unavailable.

## Invariants worth asserting (any layer, version-independent)

These hold regardless of glyphs, so they're the durable contract. Prefer them
over asserting on specific rendered text:

1. Exactly one assistant turn per `Send`.
2. **Never report `complete` while `Busy()`** — the literal invariant the
   quiescence work protects.
3. **Sentinel round-trip**: a unique token in the prompt reappears verbatim in
   the captured reply. The single highest-value check — it catches truncation
   (capturing a pre-final preamble) and extraction drift in one assertion. See
   `TestIntegration_SubAgentFlicker_DoesNotTruncate`.
4. No raw ANSI/control bytes leak into extracted reply text.
5. Liveness: every `Send` completes or errors within timeout (never hangs).
6. `Close` is idempotent; control / turn-in-flight errors fire as specified.

## Verifying a regression lock actually locks

A test that never fails on the buggy code is theater. To confirm one bites,
temporarily break the production path and watch it go red. Example for the
flicker lock:

```
# in pkg/chat/conversation.go, neuter the claude-code marker deferral:
#   if c.opts.Harness == "claude-code" {   →   if false && c.opts.Harness == "claude-code" {
go test ./pkg/chat -run TestIntegration_SubAgentFlicker -count=1   # must FAIL on the sentinel
# revert
```

## Gotchas baked into the fake (learned the hard way)

- **Raw mode disables `ONLCR`**, so the binary emits `\r\n` explicitly. Without
  the CR, `\n`-only output staircases and a long line can wrap and split a
  detection-critical string (e.g. the resume UUID) across rows — which depends
  on prompt length and so fails non-obviously.
- **The submit key is CSI 13u, not newline.** `AwaitSubmit()` waits for exactly
  that, which also pins the submit-key contract: if the wrapper stops sending it,
  the fake never advances and the test fails loudly.
- **Session-id extraction runs at `TurnComplete`** against the current screen, so
  the resume hint must be on the completion frame — `Reply`/`SettleIdle` include
  it.

## Known limitation / follow-ups

- `test/fakeharness/mock` (see `docs/mock-harness.md`) is a separate,
  **line-oriented** flag-driven fake used by several wrapper/HTTP tests
  (`screen_test`, `sse_input_test`, tmux, screenbench, e2e). Its `--ready-prompt`
  / `--needs-input` modes still read stdin by newline; they pass today because
  those tests don't submit a turn through the CSI-13u gate. If a future test
  needs the mock to consume a real submit, make its input raw / submit-key-
  agnostic (input flags only — keep `OPOST` so its `fmt.Println` output isn't
  staircased) or migrate that test to `internal/fakeharness`.
- **Layer 0** freezes four outward surfaces as golden snapshots, regenerated
  after an INTENTIONAL change with `UPDATE_GOLDEN=1 go test ./<pkg>/`:
  - chatd HTTP routes + wire DTOs (`cmd/harness-chatd/testdata/{routes,wire_contract}.golden`).
  - both binaries' CLI flags — name/type/default/usage
    (`cmd/harness-{wrapper,chatd}/testdata/flags.golden`). The FlagSets are built
    by enumerable helpers (`harnessWrapperFlagSet`, `chatdFlagSet`) so the test
    and the parser share one definition.
  - the exported `pkg/chat` Go API (`pkg/chat/testdata/go_api.golden`) — struct
    fields + method sets via reflection, plus a hand-listed registry of
    package-level funcs / typed consts / error sentinels (removing one breaks
    compilation of `contract_test.go`).
- **Layer 4** (`pkg/harness/conformance_test.go`) is gated behind
  `HARNESS_WRAPPER_CONFORMANCE=1` and skips any harness whose binary is absent,
  so it is safe in normal runs and meant for a nightly job:
  `HARNESS_WRAPPER_CONFORMANCE=1 go test ./pkg/harness/ -run Conformance`.
  `TestConformance_VersionDrift` compares each installed binary's `--version`
  against the `versions.json` pin; `TestConformance_SentinelRoundTrip` drives one
  real turn per harness and asserts the sentinel survives. The drift check earns
  its keep immediately — it caught the codex pin sitting at `0.140.0` after the
  adapter had already moved to `0.141.0`, and a second drift of claude-code
  (`2.1.141` → `2.1.185`). Both are now resolved: the pins were bumped once the
  live sentinel round-trip and a corpus re-bake confirmed the adapters against
  the installed versions, and conformance now reports zero drift.

  Re-baking against a current binary surfaced two tooling bugs worth knowing:
  the recorder submitted with a raw `\n` (which enhanced-keyboard versions
  ignore — see the submit-key gotcha above), and its corpus/script path lookup
  plus the `make rebake-corpus` target broke when screenbench became a nested
  module. All fixed. `interrupted-mid-reply` initially resisted re-bake: it
  Ctrl-C'd on a fixed 1500ms sleep calibrated to 2.1.141's latency, and against
  2.1.185 that fired before the reply streamed (cancel during "thinking", no
  marker). The durable fix was to make it **marker-driven** — wait for the `⏺`
  reply bullet, then a short settle, so the interrupt lands mid-stream
  regardless of model latency. (The busy `esc to interrupt` footer is no good
  as a wait marker: it is ANSI-split in the byte stream and never matches
  contiguously, falling back to idle-timeout that only fires after the turn
  completes.) Now re-baked at 2.1.185. Only the hand-authored `adversarial`
  scenario stays at `2.1.141` — a mixed-version corpus is fine because every
  `meta.json` states the version it was taken against, and the adapter tests
  assert version-independent structure, not text.
- The builder covers claude-code and codex. gemini / opencode / pi have stub
  adapters (no screen markers yet); when their detection lands, add their glyph
  vocabulary and scenarios the same way.
