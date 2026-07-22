# Wrapper & Status

`pkg/wrapper` is the foundation layer. It launches a harness under a pseudoterminal, supervises the
process, reads its observable state, and classifies the run into a small **normalized status**
vocabulary — so a caller can retry, pause, resume, or complete a run without coupling to a specific
CLI.

## Entry points

```go
func Run(ctx context.Context, cfg Config) (Result, error)
func Start(ctx context.Context, cfg Config) (*Session, error)
func ClassifyOutput(harness, output string) Classification
```

`Run` is synchronous — it starts the harness, supervises it, and returns when the process exits or
`ctx` is cancelled. `Start` returns a live `*Session` for callers that need to observe transitions,
stream output, or stop cleanly; `Run` is a thin convenience over `Start` + `Wait`.

A non-nil error always means the **wrapper itself** failed to start (`ErrInvalidConfig`,
`ErrBinaryNotFound`, `ErrPTYAllocation`). Once `Start` returns a session, every *harness* outcome
flows through `Result`/`SessionEvent` with a nil error. Context cancellation yields
`StatusInterrupted` and does **not** propagate `ctx.Err()`.

`ClassifyOutput` is a stateless helper: it runs the resolved per-harness classifier over a finished
output blob (e.g. a log tail), forcing the idle gate open so cost/retry/transport patterns are
eligible. It's how a failed run's supervisor upgrades a bare `failed` exit into an actionable status.

## Config

```go
type Config struct {
	BinaryPath string        // required; absolute or PATH-resolvable
	Args       []string
	WorkingDir string
	Env        []string

	Stdin  io.Reader         // nil = no input forwarding; a *os.File TTY enables raw-mode passthrough
	Stdout io.Writer         // required

	IdleQuiet      time.Duration // quiet threshold (default 15s)
	IdleClassify   time.Duration // idle-classification threshold, ≥ IdleQuiet (default 60s)
	StaleThreshold time.Duration // mid-run stale advisory (default 5m; negative disables)
	WaitDelay      time.Duration // SIGTERM→SIGKILL grace on cancellation (default 5s)

	Trace      trace.Emitter      // diagnostic events; observability only
	Harness    string             // selects a built-in classifier ("claude", "codex", "opencode", "pi", …)
	Effort     string             // reasoning effort ("low"|"medium"|"high"|"xhigh"|"max"; "" = default)
	Classifier Classifier         // explicit classifier; wins over Harness
	OnLine     func(line string)  // durable line tap (session-id / transcript hooks)
}
```

When `Stdin` and `Stdout` are both real `*os.File` TTYs the wrapper auto-enables raw mode and SIGWINCH
forwarding for the run, restoring terminal state on return. Headless callers (file/pipe/buffer stdout)
skip both — no flag.

## Status

`Result.Status` and `SessionEvent.Status` share one vocabulary. **Terminal** statuses end the run (the
wrapper SIGTERMs the harness); **non-terminal** ones are mid-run advisories emitted while it keeps
running.

```go
type Status string
const (
	StatusIdle            Status = "idle"               // exited cleanly / quiet with no actionable state
	StatusFailed          Status = "failed"             // non-zero exit code
	StatusBlockedByCost   Status = "blocked_by_cost"    // budget/quota/rate-limit hit (terminal)
	StatusRetryLater      Status = "retry_later"        // transient/recoverable error (terminal)
	StatusAPIError        Status = "api_error"          // upstream API error, harness still running (non-terminal)
	StatusWaitingForInput Status = "waiting_for_input"  // paused at an interactive prompt (non-terminal)
	StatusStale           Status = "stale"              // no output for StaleThreshold (non-terminal advisory)
	StatusInterrupted     Status = "interrupted"        // terminated by signal or caller
	StatusUnknown         Status = "unknown"            // could not classify
	StatusBinaryNotFound  Status = "binary_not_found"   // configured binary not on PATH (ExitCode -1)
)
```

`StatusStale` never appears in `Result.Status` — only on `Session.Events()`.

![Normalized status classification](../diagrams/status-statemachine.svg)

## How classification works

The wrapper tracks the latest screen snapshot and the time since output last changed, and feeds a
`ClassifierInput` to the resolved classifier:

```go
type ClassifierInput struct {
	RecentOutput    string        // tail of PTY output (~64KB), ANSI intact
	SinceLastOutput time.Duration
	Quiet           bool          // SinceLastOutput ≥ IdleQuiet  (gates prompt detection)
	Idle            bool          // SinceLastOutput ≥ IdleClassify (gates cost/retry/transport)
}
```

Classification runs as a four-stage gated pipeline (first match wins):

1. **API error** (ungated) — high-confidence anchored matchers → `StatusAPIError` (non-terminal),
   carrying `HTTPCode` (0 for transport errors) and `RetryAfter`.
2. **Session limit** (ungated) — "hit your … limit" anchored on the decoration glyph → terminal
   `StatusBlockedByCost`, carrying `ResumeAt` (parsed reset time).
3. **Cost / retry / transport** (gated on `Idle`) — cost patterns, retry patterns, and transport
   fingerprints (`connection refused`, `econnreset`, `network is unreachable`, `socket hang up`, …) →
   terminal `StatusBlockedByCost` / `StatusRetryLater`.
4. **Prompt** (gated on `Quiet`) — a trailing prompt-region matcher → non-terminal
   `StatusWaitingForInput`.

Defaults: a **15s** quiet threshold, a **60s** idle-classification threshold, a **5m** stale advisory.
Thresholds are per-harness via `Config`.

## Session handle

```go
func (s *Session) Wait() (Result, error)          // block for the terminal Result (repeatable)
func (s *Session) Stop(ctx context.Context) error // graceful SIGTERM→SIGKILL shutdown
func (s *Session) Snapshot() Snapshot             // point-in-time status/reason/timestamps
func (s *Session) Events() <-chan SessionEvent    // status-change stream; closed after the terminal event
func (s *Session) PID() int
func (s *Session) RecentOutput() string           // last ~64KB of raw PTY output

// In-process live I/O (used by pkg/chat and in-process watchers):
func (s *Session) AttachOutput(w io.Writer) func()          // tee PTY output; returns a detach func
func (s *Session) WriteStdin(p []byte) (int, error)         // forward keystrokes
func (s *Session) Resize(cols, rows uint16) error
func (s *Session) AcquireWriter() (release func(), ok bool) // claim the exclusive stdin writer
```

`SessionEvent` carries the status plus optional parsed detail:

```go
type SessionEvent struct {
	At         time.Time
	Status     Status
	Reason     string
	Terminated bool          // true on the final event (Events() then closes)
	Class      ErrorClass    // canonical error taxonomy (below)
	HTTPCode   int           // upstream status when Status == StatusAPIError
	RetryAfter time.Duration // wait hint parsed from the error message
	ResumeAt   time.Time     // absolute reset time from a session-limit banner
}
```

## ErrorClass

Errors are mapped to a stable taxonomy (the `Class` field above), independent of harness wording:

| Class | `String()` | Trigger |
|---|---|---|
| `ErrRateLimited` | `RateLimited` | 429 / usage or session limit (transient) |
| `ErrAuth` | `AuthFailure` | 401 / invalid key (fatal) |
| `ErrBilling` | `BillingError` | 402 / insufficient credits / quota (fatal) |
| `ErrModelNotFound` | `ModelNotFound` | 404 |
| `ErrTimeout` | `Timeout` | request/connection timeout |
| `ErrTransient` | `Transient` | 5xx / transport reset |
| `ErrUnknown` | `Unknown` | unclassifiable |

(`ErrNone` is the zero value; `ErrContextOverflow` is reserved.) HTTP codes map directly: 401→Auth,
402→Billing, 404→ModelNotFound, 429→RateLimited, 408/5xx→Transient, 0 (transport) → Transient.

## Trace vs. events

`Config.Trace` is a `trace.Emitter` — diagnostic observations (`wrapper_started`, `pty_opened`,
`output_quiet`, `output_classify_threshold`, `harness_stale`, `harness_classified`, `harness_exited`,
…). The trace vocabulary is **not** part of the API stability surface; never make control-flow
decisions on it.

`Session.Events()` is the typed contract for state transitions (the `Status` vocabulary). Use events,
not trace, for control flow.

## Sandbox-defaults injection

The CLI's `--sandbox-defaults` flag (honored by `run` and `structured-run`, rejected by the default
passthrough mode) opts into the permission injection the meta-harness structured runner performs for
claude-code — restoring **identical harness behavior for identical argv** across the two
implementations. For the `claude` harness it:

- appends `--dangerously-skip-permissions` to the harness args, and
- sets `IS_SANDBOX=1` in the harness env.

For every other harness it is a documented no-op. `IS_SANDBOX=1` is what suppresses claude-code's
"Bypass Permissions mode" acceptance screen entirely and allows running as root — relevant for
container workspaces under `internal/env`. (An explicitly passed `--dangerously-skip-permissions`
already works unattended without the env var: the acceptance screen is detected as a `trust_prompt`
and auto-answered, so the flag's value is cross-implementation parity plus the `IS_SANDBOX` effects,
not an un-hang fix. The same holds for `--permission-mode bypass`, which reaches claude as
`--permission-mode bypassPermissions`: unattended it is auto-answered, but an interactive `run`
surfaces the screen to the human and passthrough hands it straight to their terminal.)

### Two halves, and how `--permission-mode` composes

The flag's two contributions are **not** interchangeable, which is why the pairing composes rather
than being mutually excluded:

- the **args half** — `--dangerously-skip-permissions` — is also what the `bypass` rung delivers
  (`pkg/wrapper` emits `--permission-mode bypassPermissions`);
- the **env half** — `IS_SANDBOX=1`, the piece that permits running as root and suppresses the
  acceptance screen — is delivered by `--sandbox-defaults` and by nothing else.

So `--permission-mode bypass` alone is **not** a drop-in for `--sandbox-defaults`: the acceptance
screen comes back and root is disallowed. `--sandbox-defaults --permission-mode bypass` is exactly the
recipe a root container needs; a blanket mutual exclusion would have outlawed the one legitimate
combination. When the mode is bypass-class, `applySandboxDefaults` contributes the **env half only** —
it skips the arg append and lets `pkg/wrapper` own the single permission directive in argv (the
injected env is byte-identical either way).

Every other pairing is rejected up front in `parseHarnessWrapperArgs`, exit 2:

```
harness-wrapper: --sandbox-defaults is incompatible with --permission-mode <mode> (only --permission-mode bypass composes with it)
```

| Paired mode | Result |
|---|---|
| `bypass`, `bypassPermissions` | accepted — `--sandbox-defaults` contributes the env half only |
| `plan`, `manual`, `ask`, `auto`, `acceptEdits`, `dontAsk` | rejected, exit 2 |
| `danger-full-access` (codex's bypass-equivalent) | **rejected**, exit 2 |

The `danger-full-access` row is not an oversight. The exclusion check runs in flag parsing, **before
the harness name is known**, so it must be harness-independent: `wrapper.IsBypassPermissionMode`
recognizes only `bypass` and claude's `bypassPermissions`. Admitting codex's spelling there would let
`--sandbox-defaults --permission-mode danger-full-access codex --` slip past a check that exists to
gate the root-enabling env half. codex's own bypass handling lives in the unexported
`isCodexBypassMode`, which keeps codex vocabulary out of `cmd/`.

The exclusion check also runs **before** the passthrough rejection of `--sandbox-defaults`, so
`--sandbox-defaults --permission-mode manual claude --` reports the incompatibility, not the mode
policy. `--permission-mode` on its own is accepted in every mode, passthrough included — it is argv
the user could have typed at the harness themselves, and without `IS_SANDBOX=1` claude still shows the
acceptance screen, which passthrough has no input machinery to answer and therefore hands to the
human's own terminal.

### Runtime enforcement per path

> Restrictive rungs (`plan`, `manual`, `ask`) are fully enforced only when a human is at the TUI
> (passthrough, or `run` from a terminal for codex). Under `structured-run` and unattended `run`,
> claude's permission dialogs are not detected (the turn stalls to the deadline) and codex's approval
> prompts are auto-approved (only the `-s` sandbox axis still binds).

| Axis | passthrough | interactive `run` (tty) | unattended `run` / `structured-run` |
|---|---|---|---|
| claude per-tool permission dialog (`plan`/`manual`/`ask`) | human answers | **not surfaced** (no detector) → stalls to deadline (exit 124) | **not surfaced** → stalls to deadline (124) |
| claude bypass-acceptance screen (`bypass`) | human answers | surfaced (`bypassAnchor` → `trust_prompt`, nil policy) → tty chooser | auto-accepted by the `trust_prompt` policy |
| codex `-s` sandbox axis | enforced by codex | enforced by codex | **enforced by codex** |
| codex `-a` approval axis | human answers | surfaced → tty chooser | **auto-approved** by `oneshot.AutoAcceptAnswer` |

Known limitations behind that table, all tracked as follow-ups and deliberately **not** addressed
here:

- no detector for claude-code's per-tool permission dialog, so `plan` / `manual` / `ask` stall
  unattended turns to the deadline;
- `pkg/oneshot.AutoAcceptAnswer` is wired unconditionally, so it auto-approves `codex.KindApproval`
  even when a restrictive rung was requested (an approval policy would have to be kind-aware);
- the bypass acceptance screen shares the `trust_prompt` kind with folder trust, so no policy can
  target it independently (it would need its own `bypass_acceptance` kind).

Dedup rules make the injection idempotent against caller-supplied values:

- the arg is not appended when already present as the exact token **or** in the
  `--dangerously-skip-permissions=<value>` spelling;
- `IS_SANDBOX=1` is not appended when the env already defines the `IS_SANDBOX` key (whatever its
  value — containers may set it), matching the key exactly at the `=` boundary so a
  prefix-sharing key like `IS_SANDBOXED` never suppresses it.

The injection lives in `cmd/harness-wrapper` (`applySandboxDefaults`), **not** in
`pkg/harness.RunTurn`: `TurnConfig.Args` keeps its documented verbatim passthrough, and the
danger-carrying policy toggle stays auditable at the CLI boundary. There is **no silent injection
anywhere** — without the flag, nothing is added.

That env/args split is load-bearing and deliberate: the **arg** half must be reachable by every
`wrapper.Start` caller — passthrough included — so it lives in `pkg/wrapper`; the **env** half grants
root and must stay auditable in one CLI file, so it lives in `applySandboxDefaults` and nowhere else.
For the same reason the compose path guards on the literal harness name `"claude"` with no
`normHarness` normalization (unlike `pkg/wrapper`, which does normalize): the CLI's supported names
are exactly `claude`, `codex`, `opencode`, `pi`, so the `claude-code` alias never reaches it from the
CLI, and normalizing would quietly widen the set of invocations receiving the root-enabling env half.

## PTY execution & attach

The wrapper starts each harness under a pseudoterminal with `pty.Start`; the PTY stream is the
canonical source for output capture and state detection. The supervisor reads continuously, copies to
`Stdout` (and any `AttachOutput` sinks), updates the last-activity timestamp, and feeds the rolling
recent-output buffer into the classifier.

The `*Session` attach primitives (`AttachOutput`, `WriteStdin`, `Resize`, `AcquireWriter`) are
**in-process** — they let multiple watchers in the *same* process share one live session, which is how
[`pkg/chat`](../guide/chat.md) tees output and serializes keystrokes. **Cross-process** attach (a
separate process driving a headlessly-started run) is the daemon path tracked as item 3 in the
[Roadmap](roadmap-v1.md); shell users get the detached case today via the CLI's
[tmux mode](../guide/cli.md#detached-tmux).
