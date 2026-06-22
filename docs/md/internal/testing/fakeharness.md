# Fake Harness

Layer 3 of the [testing tiers](README.md) is the one that exercises **timing** — the idle-completion
watcher, the marker-confirm gap, `Busy()` flicker — which adapter-level [corpus replay](corpus.md)
cannot reach because it bypasses the conversation loop and its wall-clock timers.

`cmd/fakeharness` is a **real binary** that `chat.Open` (and `harness.RunTurn`, and the chatd HTTP
server) spawn over a real `creack/pty`. Replaying a script therefore drives the genuine `screen`
emulator, `turns.Watch` pump, and `idleCompletionWatcher` goroutines on real time. **It is not a
mock.**

## Authoring a scenario

A test builds a `fakeharness.Script` with the fluent builder, marshals it to JSON, and points the
binary at it via the `FAKEHARNESS_SCRIPT` env var. The binary switches its PTY slave to raw mode (as a
real TUI does) and replays the timeline: paint frames on a delay, block until the wrapper types an
expected byte sequence, optionally echo the captured prompt back.

The builder stamps the exact claude-code glyph vocabulary in one place, so a future TUI drift updates
fixtures and adapter patterns together:

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

Every frame method takes a leading `delayMs` — a wall-clock sleep before painting, which is how cadence
is reproduced.

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

Input / lifecycle: `AwaitSubmit()` (CSI 13u, captures the prompt), `AwaitMenuChoice()` (digit+CR),
`Exit(code)`, `ExitOnQuit()` (double-Ctrl-C → exit 0). `PromptRef()` is the placeholder substituted
with the captured prompt in any echoed frame.

**Codex** uses a different completion model — no `Busy()`, no quiescence: the chat layer completes a
turn the instant a fresh `Token usage: …` footer appears. `New("codex")` switches the vocabulary;
`Idle()` paints the `›` composer, and `CodexWorking(d, status)` / `CodexReply(d, body)` paint the
in-flight and end-of-turn frames (the latter carries a per-call-distinct token-usage footer, since
codex dedupes by exact footer text). Submit is still CSI 13u.

## Why a real PTY matters

This layer regression-locks the quiescence / `Busy()` timing bugs and has **found new ones** — e.g. a
lost-wakeup race in `waitReadyForSend` (it checked readiness, then subscribed, missing the
prompt-ready frame if it landed in between). Real harnesses repaint continuously and mask it; the fake
paints its prompt once and blocks, exposing it deterministically under `-race -count`. The bugs this
layer locks are **timing bugs in `pkg/chat`'s watcher goroutine**, not pattern bugs in the adapter —
neither corpus replay nor the unit tests run the real timer goroutine end to end.

## Driving it

- **`pkg/chat`** (timing scenarios): `openFake(t, script)` returns a `Conversation` with shrunk
  completion windows (per-`Conversation` `Options` overrides, so the watcher goroutines stay
  race-free), then `sendOneTurn` + `waitForTerminalTurn`.
- **`pkg/harness` / `cmd/harness-chatd`** (wrapper/HTTP scenarios): `fakeBin(t)` + `scriptEnv(t, script)`;
  these use the **default** gaps (real `RunTurn` behavior), so per-turn latency is ~2s.

The binary is built once per test process via `fakeharness.BuildOnce()` and cleaned up in each
package's `TestMain` via `fakeharness.Cleanup()`. Tests `t.Skip` if the Go toolchain is unavailable.

## Gotchas baked into the fake (learned the hard way)

- **Raw mode disables `ONLCR`**, so the binary emits `\r\n` explicitly — without the CR, `\n`-only
  output staircases and a long line can wrap and split a detection-critical string (e.g. the resume
  UUID) across rows.
- **The submit key is CSI 13u, not newline.** `AwaitSubmit()` waits for exactly that, pinning the
  submit-key contract: if the wrapper stops sending it, the fake never advances and the test fails
  loudly.
- **Session-id extraction runs at `TurnComplete`** against the current screen, so the resume hint must
  be on the completion frame — `Reply`/`SettleIdle` include it.

## The older line-oriented mock

`test/fakeharness/mock` is a separate, **line-oriented** flag-driven fake used by several wrapper/HTTP
tests (`screen_test`, `sse_input_test`, tmux, screenbench). Its `--ready-prompt` / `--needs-input`
modes read stdin by newline; they pass today because those tests don't submit a turn through the
CSI-13u gate. If a future test needs the mock to consume a real submit, make its input raw /
submit-key-agnostic (keep `OPOST` so its `fmt.Println` output isn't staircased) or migrate that test to
`internal/fakeharness`.

The builder covers claude-code and codex. gemini / opencode / pi have [stub adapters](../../guide/adapters.md)
(no screen markers yet); when their detection lands, add their glyph vocabulary and scenarios the same
way.
