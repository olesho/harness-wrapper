# Execution environments

Everything else in this repo answers *how do we drive a harness?* This layer answers **where does the
harness run, and what is it allowed to touch?**

It comes in two halves:

- **`internal/env`** — the environment core: a `Provisioner` axis, a `Containment` axis, the
  `Workspace` contract they meet at, the `Compose` combinator, and the `Env` lifecycle engine. It is
  deliberately internal — see [ADR-003](decisions/adr-003-env-visibility.md).
- **`pkg/env`** — the small **public** half: a host-side client that runs one
  [structured turn](turnproto.md) inside any `Workspace`.

The model is **batch request/response**: exec a command, upload a file, download a file. No PTY, no
streaming — the PTY lives inside the guest, under the harness-wrapper binary running there.

![Provisioner and Containment compose into one Workspace](../diagrams/env-layers.svg)

## Two orthogonal axes

```go
type Provisioner interface {
	Name() string
	Preflight(ctx context.Context) error            // host-side checks; allocates nothing
	Create(ctx context.Context, spec WorkspaceSpec) (Workspace, error)
}

type Containment interface {
	Name() string
	Preflight(ctx context.Context, inner Workspace) error
	Layer(ctx context.Context, policy PolicySpec) (ContainmentLayer, error)
}
```

| Axis | Question | Shipped implementations |
|---|---|---|
| Provisioner | Where does the machine come from? | `Local(root)` — plain host directories, host == guest · `Daytona(cfg)` — a remote sandbox |
| Containment | What may the agent touch once it is there? | `None()` — the identity layer · `OpenShell(opts)` — wraps exec and file transfer |

Keeping them orthogonal is the point: a containment can be tested against the local provisioner, and a
provisioner can be exercised with no containment at all.

## The Workspace contract

```go
type Workspace interface {
	Exec(ctx context.Context, argv []string, opts ExecOpts) (ExecResult, error)
	Upload(ctx context.Context, hostPath, guestPath string) error
	Download(ctx context.Context, guestPath, hostPath string) error
	GuestPath(kind PathKind) string   // PathRepo | PathHome | PathTmp
	HostAlias(host string) string     // rewrite a host-visible address for the guest
	Destroy(ctx context.Context, outcome Outcome) error
}
```

`Exec` takes **argv as a list** — there is no shell in the middle to re-split it. A non-zero exit is a
*result*, not an error: `ExecResult.Code` carries it and the error stays nil. Only a genuine spawn or
transport failure is an error. That distinction is what lets a caller treat "the harness exited 124"
as data.

`HostAlias` exists because a guest often cannot reach `localhost` the way the host can.

## Compose

```go
func Compose(inner Workspace, layer ContainmentLayer) Workspace
```

The core-owned combinator that makes the two axes independent. The composed workspace:

- **wraps every exec** through the layer, then runs the wrapped argv on the inner workspace;
- **stages transfers across the boundary** — an upload lands in a uniquely-named staging path under
  the inner workspace's temp directory and is then moved across by the layer's own transfer command.
  When the layer reports no boundary, the upload goes straight to its destination with no staging;
- **remaps paths** — the layer's mapping shadows the inner one, and an empty mapping defers to it;
- **folds host aliases** in two hops (inner first, then the layer if it maps aliases);
- **tears down in order** — layer first, then the inner workspace, aggregating both failures.

## Lifecycle

```go
func Env(ctx context.Context, cfg EnvConfig) (*Environment, error)
```

The canonical order, with teardown registered as soon as the corresponding resource exists:

1. `Provisioner.Preflight` — host-side only; nothing is allocated yet, so a failure here is free.
2. `Provisioner.Create` — resources now exist; the teardown thunk is registered **immediately**.
3. `Containment.Preflight(inner)`.
4. Acquire (if the containment supports acquisition) or `Layer(policy)`, then `Compose`.
5. Register credential **redactions first**, then apply the injectors — with each injector's cleanup
   pushed before its apply, so a half-applied injector is still cleaned up.
6. Turns run (out of scope for this package).
7. Destroy: injector cleanup in reverse, then containment teardown, then the inner workspace.

Any failure unwinds in reverse, best-effort. Unwind errors do not replace the original — they are
aggregated into a teardown error that keeps the setup failure first, and `errors.Is` / `errors.As`
traverse the whole set.

## Retention

```go
func ShouldKeep(retention Retention, outcome Outcome) bool
```

| Retention | success | failure | setup failure |
|---|:--:|:--:|:--:|
| `""` (default) | destroy | destroy | destroy |
| `RetentionKeepOnFailure` | destroy | **keep** | destroy |
| `RetentionAlways` | **keep** | **keep** | destroy |

A **setup** failure is never kept, under any policy: the workspace never reached a state worth
inspecting, and keeping it would leak resources on every misconfiguration.

## Shell-argv helpers

Some remote exec surfaces accept only a shell string, so the core ships the quoting discipline rather
than letting each driver invent one:

```go
func ShQuote(arg string) string
func ArgvToShell(argv []string) string
func EnvPrefixedShell(env map[string]string, argv []string) string
```

`EnvPrefixedShell` emits `env K=V … <quoted argv>` with **keys sorted**, so the same call always
produces the same string — which is what makes a recorded command comparable across runs.

> `container.go` (`ContainerWorkspace`, `DetectContainerRuntime`) is **test-only** infrastructure for
> exercising a real cross-boundary workspace in CI. It is a `Workspace`, not a `Provisioner`, and is
> explicitly excluded from any future public surface.

## Running a turn in a workspace

`pkg/env` is the public half — the host-side client for the
[structured turn protocol](turnproto.md):

```go
type StructuredTurnConfig struct {
	Runner      []string // default: {"harness-wrapper", "structured-run"}
	Harness     string   // required
	HarnessArgs []string

	Effort, Model, PermissionMode string
	SandboxDefaults               bool

	Prompt string
	Env    map[string]string

	PromptGuestPath     string // "" = <guest tmp>/structured-turn-prompt.txt
	TranscriptGuestPath string // both must be set to download a transcript
	TranscriptHostPath  string
}

func RunStructuredTurn(ctx context.Context, ws ienv.Workspace, cfg StructuredTurnConfig) (*turnproto.StructuredTurnResult, error)
```

The round trip:

1. Stage the prompt to a host temp file, `Upload` it, remove the host copy.
2. `Exec` the runner as argv:
   ```
   <runner…> --prompt-file <guest prompt> [--effort E] [--model M] [--permission-mode P] [--sandbox-defaults] <harness> -- <harness args…>
   ```
3. Feed the captured stdout to `turnproto.ParseLastJSONLine`.
4. Optionally `Download` the transcript.

**The prompt never touches argv.** It travels as a file, so quotes, newlines, and leading dashes
cannot corrupt the command line — the same reason the CLI exposes `--prompt-file`.

**A non-zero guest exit is not an error here.** `deadline` (124) and `errored` (1) come back as a
parsed result with a status, exactly like `completed`. An error means the exec itself failed or no
JSON result appeared on stdout — in which case a bounded tail of stderr is included in the message.

The `--sandbox-defaults` / `--permission-mode` composition rule is enforced **before** the exec, so an
incompatible pairing fails on the host rather than burning a guest round trip. See
[Permissions & sandboxing](../guide/permissions.md).

## Environment variables

| Variable | Read by | Meaning |
|---|---|---|
| `HARNESS_WRAPPER_CONTAINMENT` | the live containment test tiers | must be `1` to run tests that allocate real remote resources |
| Daytona API credentials | the Daytona driver + its live tests | the API key is also on the leak-probe list, so it must never appear in a captured transcript |

The core reads nothing else from the ambient environment: what the guest sees comes from
`ExecOpts.Env` and the injectors, never from inheritance.

## Why it is internal

[ADR-003](decisions/adr-003-env-visibility.md) records the decision to keep the core in `internal/`:
there are no out-of-module production consumers today, and the only public need — running a structured
turn against a workspace — is already served by `pkg/env`. The ADR also records the promotion path
(import direction, the concern collision in `pkg/env`, keeping the test-only container workspace out of
any public surface) so a future consumer does not have to re-derive it.
