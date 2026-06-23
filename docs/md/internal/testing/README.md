# Testing Tiers

The wrapper's job is to drive interactive coding-agent TUIs over a PTY and report **turn boundaries**
and **reply content** faithfully. The hard part isn't our own API — it's that we screen-scrape tools
we don't control, and they change. The suite is structured to keep that contract stable.

## Three contracts

1. **Outward** — the HTTP routes, JSON DTOs, CLI flags, and exported `pkg/chat` API. We own it; it
   breaks only when we edit it carelessly.
2. **Inward** — our assumptions about how claude-code/codex *render* (`✻ Verb for Ns`,
   `esc to interrupt`, `❯`, `Token usage:`, the CSI-13u submit, the `/quit` exit). We don't own
   it; it breaks silently whenever those tools ship a new version.
3. **Correspondence** — the actual promise to callers: *report turn boundaries correctly and hand back
   the real final reply, not a mid-turn preamble.* This is what the timing-sensitive completion logic
   defends, and the layer with the least natural coverage.

The trap: pattern tests + corpus replay are strong on (1) and (2)-at-rest, which gives **false
confidence** — a corpus recorded from `claude-code 2.1.x` stays green forever, including the day a new
version breaks every real user. That's what layers 3 and 4 exist to catch.

## The pyramid

![Five testing tiers](../../diagrams/testing-tiers.svg)

| Layer | Tests | Hermetic? | Cadence | Where |
|---|---|---|---|---|
| 0 · API-freeze | HTTP routes + wire DTOs + CLI flags + exported `pkg/chat` API as golden snapshots | yes | per-commit | `cmd/harness-chatd/contract_test.go`, `cmd/harness-wrapper/contract_test.go`, `pkg/chat/contract_test.go` |
| 1 · Pattern units | the adapter regexes / markers | yes | per-commit | `pkg/turns/harness/**` |
| 2 · Corpus replay | recorded byte-streams → adapter → boundaries + text | yes | per-commit | `pkg/turns/harness/**/*_test.go`, [`test/corpus/**`](corpus.md) |
| 3 · [Fake-harness integration](fakeharness.md) | full PTY→screen→turns→chat→HTTP against a scriptable fake | yes | per-commit | `internal/fakeharness`, `cmd/fakeharness`, `pkg/chat/integration_test.go` |
| 4 · Live conformance + drift | real installed binaries: [version drift](../versions-drift.md) + sentinel round-trip | no | nightly | `pkg/harness/conformance_test.go` |

Layers 1–2 verify the adapter in isolation. Layer 3 is the one that exercises the **timing** machinery
— the idle-completion watcher, the marker-confirm gap, `Busy()` flicker — which adapter-level replay
cannot reach because it bypasses the conversation loop and its wall-clock timers. Layer 4 is the only
one that runs real upstream binaries.

## Layer 0: golden snapshots

Four outward surfaces are frozen and regenerated after an **intentional** change with
`UPDATE_GOLDEN=1 go test ./<pkg>/`:

- chatd HTTP routes + wire DTOs (`cmd/harness-chatd/testdata/`).
- both binaries' CLI flags — name/type/default/usage (`cmd/harness-{wrapper,chatd}/testdata/flags.golden`).
- the exported `pkg/chat` Go API — struct fields + method sets via reflection, plus a hand-listed
  registry of package funcs / typed consts / error sentinels (`pkg/chat/testdata/go_api.golden`).

## Layer 4: live conformance

`pkg/harness/conformance_test.go` is gated behind `HARNESS_WRAPPER_CONFORMANCE=1` and skips any harness
whose binary is absent, so it's safe in normal runs and meant for a nightly job.
`TestConformance_VersionDrift` compares each installed binary's `--version` against the
[`versions.json`](../versions-drift.md) pin; `TestConformance_SentinelRoundTrip` drives one real turn
per harness and asserts the sentinel survives. The drift check earned its keep immediately — it caught
codex sitting at `0.140.0` after the adapter had moved to `0.141.0`, and a claude-code drift
`2.1.141 → 2.1.185`. Both are resolved; conformance now reports zero drift.

## Invariants worth asserting (any layer, version-independent)

These hold regardless of glyphs, so they're the durable contract — prefer them over asserting on
specific rendered text:

1. Exactly one assistant turn per `Send`.
2. **Never report `complete` while `Busy()`** — the literal invariant the quiescence work protects.
3. **Sentinel round-trip**: a unique token in the prompt reappears verbatim in the captured reply. The
   single highest-value check — it catches truncation and extraction drift in one assertion.
4. No raw ANSI/control bytes leak into extracted reply text.
5. Liveness: every `Send` completes or errors within timeout (never hangs).
6. `Close` is idempotent; control / turn-in-flight errors fire as specified.

## Verifying a regression lock actually locks

A test that never fails on the buggy code is theater. To confirm one bites, temporarily break the
production path and watch it go red — e.g. neuter the claude-code marker deferral in
`pkg/chat/conversation.go` (`if c.opts.Harness == "claude-code"` → `if false && …`) and run the
sub-agent-flicker sentinel test; it must FAIL. Then revert.

## Running it

```bash
make test   # go vet + gofmt + go test -race ./...  (hermetic: layers 0–3)
HARNESS_WRAPPER_CONFORMANCE=1 go test ./pkg/harness/ -run Conformance   # layer 4 (nightly)
```

See [Corpus](corpus.md) for the recording workflow and [Fake Harness](fakeharness.md) for the
scriptable real-PTY fake that powers layer 3.
