# ADR-003: `pkg/env` pluggable-environments scope — keep the env core internal

**Status:** Accepted — keep-internal (2026-07-21)

## Context

The meta-harness "environment layer" exposes a public surface across three TS subpaths
(`env`, `env-daytona`, `env-openshell`): a core engine, two orthogonal contracts
(a `Provisioner` for *where* the machine comes from and a `Containment` for *what* the agent
may touch) meeting at a `Workspace`, the `compose`/`local`/`none` combinators and degenerate
implementations, a test-only `ContainerWorkspace`, and the Daytona/OpenShell subpackages.

**Corrected baseline — the Go env core already exists.** This layer is *not* a TS-only gap
awaiting a Go port. It is already a full, clean-room Go re-implementation, but it lives under
`internal/env`, not `pkg/env`. `internal/env/doc.go` describes itself as *"a clean-room Go
re-implementation of the meta-harness environment-layer core."* Every symbol the TS `env`
package exposes already has a Go counterpart:

- **Core engine & contracts** → `internal/env/env.go:31` `Env(ctx, cfg)` lifecycle;
  `internal/env/types.go` (`Provisioner`, `Workspace`, `Containment`, `ContainmentLayer`,
  `CredentialInjector`, `Redactor`, `WorkspaceSpec`, `EnvConfig`, `Environment`); plus
  `retention.go` and `argv.go`.
- **`compose`** → `internal/env/compose.go:57` `Compose(inner, layer)` (core-owned combinator).
- **`local`** → `internal/env/local.go:210` `Local(root)` provisioner.
- **`none`** → `internal/env/none.go:45` `None()` containment (`identityLayer`).
- **`ContainerWorkspace`** → `internal/env/container.go:34` (**test-only**; header at
  `container.go:14` marks it *"Test-only container workspace … suitable for CI"*).
- **`detectContainerRuntime`** → `internal/env/container.go:24` `DetectContainerRuntime()`
  (already idiomatic Go naming, exported).
- **`env-daytona` subpath** → `internal/env/daytona/daytona.go:120` `Daytona(config)`,
  `file_injector.go:29` `FileCredentialInjector`, plus `leak_probe.go` and `sweep.go`.
- **`env-openshell` subpath** → `internal/env/openshell/openshell.go:280` `OpenShell(opts)`,
  `GeneratePolicy` (`:194`), `ResolveGuestURL` (`:103`), and
  `(*OpenShellContainment).Acquire` (`:359`).

What `pkg/env/` actually contains today is **only** the host-turn client: `turn.go`
(`RunStructuredTurn` + `StructuredTurnConfig`) and `turn_test.go`. `RunStructuredTurn`
(`pkg/env/turn.go:102`) operates over an `internal/env.Workspace` via the `ienv` import
(`pkg/env/turn.go:25`) and is already public — it is orthogonal to this decision and works
over any `Workspace` regardless of where the core package lives.

So the real question is not "port the TS layer or declare Go minimal-by-design" — Go is
neither un-ported nor minimal-by-design. The actual question is a **visibility / packaging**
one:

> **Promote** `internal/env` (+ `internal/env/daytona`, `internal/env/openshell`) to public
> `pkg/env/...` subpackages to match the TS public surface, **OR** deliberately **keep them
> internal** and document that choice.

## Decision

**Keep the environment core internal (`internal/env/**`) for now, and document the promotion
path.** No code moves.

**The decisive input — there is no consumer.** A `grep` across all non-test `.go` for callers
of `Env(`, `Local`, `None`, `daytona.Daytona`, `openshell.OpenShell` finds **zero production
callers**. The single reference to any provisioner symbol outside the `internal/env` tree is a
test: `pkg/env/turn_test.go:54` uses `ienv.Local` — in-module test code that can keep importing
`internal/env` unchanged. The one non-test file that names `internal/env` at all,
`cmd/harness-wrapper/sandbox_defaults.go:11`, only *mentions* it in a doc comment and calls
nothing.

**Consumer hinge (the question this ADR turns on):** *Is there — or will there imminently be —
an out-of-module Go consumer that needs to construct provisioners/containments (`Local`,
`None`, `Daytona`, `OpenShell`, `Compose`, `Env`) directly, rather than only calling the
already-public `RunStructuredTurn`?*

**Today the answer is no.** Only in-module test code touches the core, and Go's `internal/`
visibility rule obstructs no current usage. Absent a named out-of-module consumer, keeping the
layer internal costs nothing:

- Promotion (`internal/env` → `pkg/env`) only *widens* visibility. It is a non-breaking,
  reversible follow-up, so deferring it forecloses nothing.
- Publishing an API with no client freezes a surface we cannot validate against real usage.
  Waiting for a concrete consumer lets that consumer shape (and stress-test) the public names.

**Promote only if** the ticket owner names a concrete out-of-module consumer. If so, execute
the promotion path below — but settle each of its architectural questions *in a follow-up ADR*
before any code moves.

## Promotion path (for a future out-of-module consumer)

Recorded here so a future consumer can act without re-deriving this analysis. Each item is a
decision to make, not a foregone conclusion:

1. **Preserve the import-direction contract.** `internal/env/doc.go:14` commits to: *"Child
   packages (internal/env/openshell, internal/env/daytona) will import THIS package; the core
   imports none of them."* Promotion must keep `pkg/env` (core) ← `pkg/env/daytona` /
   `pkg/env/openshell`, mirroring the TS `env` / `env-daytona` / `env-openshell` layout. No
   import cycle; no core→child dependency.
2. **Resolve the concern collision in `pkg/env`.** `pkg/env` today is a single-purpose
   host-turn client (`turn.go`). Dropping the `Env`/`Provisioner`/`Containment` *core* into the
   same package co-locates two distinct concerns. Choose explicitly: (a) core into `pkg/env`
   alongside `turn.go`, or (b) a dedicated `pkg/env/core` (or similar) subpackage. Either way,
   repoint `turn.go`'s `ienv "…/internal/env"` import (`pkg/env/turn.go:25`) and update
   `pkg/env/turn_test.go:54`.
3. **Preserve idiomatic Go naming — do NOT mirror TS camelCase.** "Match the TS `env` shape"
   means structural/behavioral parity, not name-for-name mirroring: keep `DetectContainerRuntime`
   (Go), not `detectContainerRuntime` (TS). Guard against a mechanical rename.
4. **Scope `ContainerWorkspace` explicitly.** `internal/env/container.go:14` marks it
   *"Test-only container workspace … suitable for CI"* (image default `node:latest`, names
   `meta-harness-test-%d`). It is CI scaffolding, not a first-class provisioner. **Recommended:
   exclude it from the public surface** (leave it internal/test-only). If parity genuinely
   demands it, acknowledge that this promotes a test harness into public API and harden it
   accordingly. Do not treat it as a like-for-like symbol with TS's `ContainerWorkspace`.
5. **Decide the Daytona injector surface.** Promoting the Daytona provisioner must also settle
   the fate of its siblings that would move with it: `FileCredentialInjector`
   (`daytona/file_injector.go:29`), `leak_probe.go`, and `sweep.go` — state which are public and
   which stay internal helpers. **OpenShell has no symmetric decision:** its public surface is
   exactly the four symbols above (`OpenShell`, `GeneratePolicy`, `ResolveGuestURL`, `Acquire`);
   `internal/env/openshell/cli.go` exports **no** symbols (all lowercase helpers) and stays an
   internal helper. The Daytona-only asymmetry here is deliberate, not an oversight.

## Consequences

- **No code moves.** `internal/env/**` and `pkg/env/turn.go` stay exactly where they are;
  `harness verify` / `harness test` remain green.
- **`RunStructuredTurn` is unaffected.** It is already public and orthogonal to package
  placement; its parity is covered separately by `pkg/env/turn_test.go`.
- **The env-gated live tiers remain gated and skip by default.** Both the Daytona
  (`internal/env/daytona/live_test.go:22`) and OpenShell
  (`internal/env/openshell/containment_live_test.go:22`) live tests gate on the single shared
  const `HARNESS_WRAPPER_CONTAINMENT` and `t.Skipf` unless it is `1` (skip at `:26–27` of each).
  A plain `harness test` stays green with no Daytona credentials or OpenShell gateway; only
  `HARNESS_WRAPPER_CONTAINMENT=1` exercises real infrastructure.
- **Promotion stays a cheap, reversible follow-up.** When an out-of-module consumer
  materializes, the path above turns the decision into a mechanical move plus five settled
  design questions — no re-analysis required.
