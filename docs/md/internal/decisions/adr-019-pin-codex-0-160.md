# ADR-019: the codex pin moves to 0.160.0; claude stays at 2.1.283

**Status:** Accepted (2026-10-05)

**Intent:** principle 4, *prefer the harness's own record*, and principle 6, *evolve public contracts
deliberately* ([INTENT](../../../../INTENT.md#design-principles)). Under principle 4, what a loaded
thread is given back comes from what came with it. Under principle 6, a move of a pin keeps every
Session the old pin saved loadable, and a pin moves only once every live test speaks for it.

## Context

The pins stood at claude 2.1.283 and codex 0.144.5; npm had claude 2.1.289 and codex 0.160.0.

codex 0.160.0 passes the app-server adapter's conformance suite but for two things:

- It answers a thread's name, in `thread/read`, from its state database (`state_5.sqlite`), no
  longer from `session_index.jsonl`. A thread loaded into a new environment brings
  `session_index.jsonl` and its name in it, but no archive carries the database — it holds the old
  environment's paths — so codex holds no name there, and the profile refuses the open
  (`state_mismatch`).
- It has `http_headers_helper`, a command whose output is a server's headers, which
  [ADR-016](adr-016-headers-from-files.md) waited for.

claude 2.1.289 passes the stream-json adapter's conformance suite, the hook live tests and agentd's
real-account e2e, but not `TestRunTurn_RealClaudeLargePromptIntact`: it takes a prompt the screen
driver delivers as a bracketed paste as pasted content, not a request ("Your message contained only
pasted text, with no request of your own"), and leaves the paste's first line in the composer.
2.1.283 answers the same prompt.

## Decision

1. **The codex pin moves** to 0.160.0, in `pkg/versions/versions.json` and the vendored
   meta-harness snapshot; claude stays at 2.1.283 until the screen driver submits a long prompt in
   a way 2.1.289 treats as a request.
2. **The old codex pin stays a load source.** The thread codex 0.144.5 saved is kept, and 0.160.0
   loads it; 0.160.0's own saved thread is recorded beside it.
3. **A loaded thread gets its name back**: first opened in its new environment, when codex holds no
   name for it and the `session_index.jsonl` that came with it gives the name the thread was saved
   with, the profile sets that name through `thread/name/set`, then checks the thread as before. A
   thread that came without its name is still refused.
4. **codex reads a connector's header from its file** through a helper, as claude does (ADR-016,
   amended); the profiles share the helper's script (`adapter.HeadersScript`).

## Consequences

- Archives saved under codex 0.144.5 load under 0.160.0.
- A codex connector header no longer passes through codex's environment.
- Verified on 0.160.0: the conformance suite, every scenario; the load fixtures; and
  `TestCodexKeeperLive`, which signed in to a real ChatGPT login with a device code, refreshed it
  and signed out.
