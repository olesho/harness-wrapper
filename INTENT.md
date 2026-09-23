# harness-wrapper — Project Intent

harness-wrapper gives programs API access to CLI coding agents that were built only for a person at a
terminal, turning each one into a supervised, programmable session. This document states why the
project exists and the criteria a change is judged against.

## Purpose

Claude Code, Codex, OpenCode and pi are coding agents: each runs a model in a loop that reads, edits
and executes on its own. They ship as interactive terminal programs built for a person at a keyboard,
and that is their only complete interface. A program that wants to put one to work — hand it a task,
hold a multi-turn conversation with it, run it unattended, know when it has finished and why — has no
API to call: the agent paints a TUI, pauses on dialogs, fails in its own words, and changes all of that
between releases without notice.

harness-wrapper is that API, built from the outside without the vendor's cooperation. It starts the
agent under a PTY, watches what it does, and exposes it to programs — as a Go library, an HTTP + SSE
gateway, a one-shot wire protocol and a CLI — in one vocabulary: whether the run is working, waiting,
finished, or stopped and why; where a turn begins and ends; what the agent said; and which session to
resume. A caller should be able to drive any supported agent the same way and act on the result without
knowing that agent's quirks.

## Users and Scope

Orchestrators and agent platforms need to launch a harness for a job, know reliably when a turn
completed or why it did not, and get back its reply, transcript and session id. Go programs embed the
chat and one-shot libraries directly. Hosts in other languages use the HTTP + SSE gateway and its
reference clients, or the structured-run wire protocol. People at a shell use the CLI for passthrough,
one-shot, and tmux-detached runs.

harness-wrapper owns:

- **Supervision and classification** — the PTY supervisor, the normalized `Status` vocabulary and the
  `ErrorClass` taxonomy.
- **Reading the harness** — vt100 screen emulation, per-harness turn adapters, and read-only parsers
  for each harness's own transcript.
- **Driving the harness** — the `Conversation` API with control, interactive input for blocking
  dialogs, and a pluggable metadata `Store`; one-shot turns and the frozen `turnproto` contract.
- **Launch policy** — per-harness capability profiles (session ids, resume, hooks), the canonical
  permission rungs translated into native flags, and opt-in Landlock containment around the harness
  and everything it starts.
- **Keeping honest about upstream** — discovery of installed binaries, pinned verified versions, the
  recorded corpus and the drift pipeline.

The four core layers (`wrapper → screen → turns → chat`) answer *how do we drive a harness*; the
packages beside them (`harness`, `oneshot`, `turnproto`, `env`) answer *how do we run one job*, and
only ever consume the core. Layer detail belongs in the architecture documentation.

## Design Principles

1. **The screen is a contract we don't own.** Every marker and dialog an adapter keys on can move in
   the next upstream release. Pin the version an adapter was verified against, keep recorded captures
   of real sessions, and treat drift detection as part of the product, not test hygiene.
2. **A wrong verdict is worse than no verdict.** Callers stop, retry, park or bill on a
   classification. Classify only on evidence that names the condition; when the output does not say,
   report `unknown` rather than guess. Adversarial cases — the assistant quoting a marker, a timestamp
   that looks like a status code — belong in the corpus.
3. **Normalize, don't leak.** Callers see one status vocabulary, one error taxonomy, one turn model
   and one permission ladder. Harness-specific strings and flags stay inside the adapters and
   profiles.
4. **Prefer the harness's own record.** Screen-scraped text is best-effort. When the harness writes a
   transcript, that is the source for what the model said; the screen is for state.
5. **Keep the stack one-way and the core transport-free.** Each layer depends only on the one below.
   HTTP, framing and auth live in `cmd/` binaries that import `pkg/chat`, so every transport shares
   one set of conversation semantics.
6. **Evolve public contracts deliberately.** The Go API, gateway routes, the `turnproto` wire format
   and the conformance corpus shared with meta-harness are relied on by other repositories. The same
   turn must classify the same way through the CLI, the gateway, the library and the TypeScript twin.
   Change them additively, and break them only on purpose.
7. **Say what is enforced, not what is intended.** The agent acts on its own, and programmatic access
   removes the person who would otherwise watch it — so what launch policy actually enforces matters.
   A permission rung binds at launch and binds fully only with a person at the TUI; containment is
   opt-in, additive, and refuses rather than degrades.
   Document the gap between a setting's name and its effect, and never widen it silently.

## Boundaries

harness-wrapper is not an orchestrator. It does not decide what work to run, schedule it, retry it,
or record it; that belongs to whoever calls it. It reports what happened on one run precisely enough
for them to decide.

It is not an agent. It does not host models, call model APIs, or reimplement an agent's tools; it
supervises the vendor's binary as shipped. Credentials and accounts belong to the harness and its
caller — harness-wrapper can host a human-led sign-in but does not own the account. It stores
conversation metadata only; message bodies live in the harness's own logs.

New capabilities should make a harness easier to supervise, classify, drive, or contain. A new harness
joins through the same bounded workflow as the existing ones — markers, recorded scenarios including
adversarial ones, a version pin and a drift check — not through a special case. Convenience features
must not weaken the normalized vocabulary, the one-way layering, or the honesty of what is enforced.

## What Success Looks Like

- A program can put any supported agent to work through an API, with no person at its terminal.
- An unattended run always ends in a verdict a caller can act on, and the verdict is right.
- A caller can switch harnesses without rewriting how it interprets runs, turns and errors.
- An upstream release that breaks detection is caught by the drift pipeline before a caller is.
- One turn produces the same status through every surface and in both languages.
- Adding a harness is a bounded, documented piece of work.
- A contained harness cannot reach outside its policy, and an uncontained one behaves as if
  containment did not exist.

## Related Documents

This document describes enduring purpose and decision criteria, not a release plan or a claim that
every desired guarantee is already complete.

- [README](README.md): what the toolkit is and how to use each surface.
- [Architecture](docs/md/internal/architecture.md): the layers, data flow and import rules.
- [Glossary](docs/md/internal/glossary.md): canonical vocabulary.
- [Permissions & sandboxing](docs/md/guide/permissions.md) and
  [Landlock containment](docs/md/guide/containment.md): what launch policy enforces.
- [Versions & drift](docs/md/internal/versions-drift.md): pins, corpus and the drift pipeline.
- [Architecture decisions](docs/md/internal/decisions/): specific design choices.
- [Agent guide](AGENTS.md): contributor tooling and gates.

Keep this intent stable as implementation evolves. Revise it when the project's purpose or boundaries
change; track current work in the issue tracker.
