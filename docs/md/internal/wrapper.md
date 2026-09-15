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
	Model      string             // model for this run ("" = harness default); never validated
	PermissionMode string         // launch-time permission rung ("" = harness default)
	Classifier Classifier         // explicit classifier; wins over Harness
	OnLine     func(line string)  // durable line tap (session-id / transcript hooks)
}
```

When `Stdin` and `Stdout` are both real `*os.File` TTYs the wrapper auto-enables raw mode and SIGWINCH
forwarding for the run, restoring terminal state on return. Headless callers (file/pipe/buffer stdout)
skip both — no flag.

`Effort`, `Model` and `PermissionMode` are the three execution-mode knobs. Each has its own argv
translator, and all three are applied in order inside `wrapper.Start` — `argsWithHarnessEffort`, then
`argsWithHarnessModel`, then `argsWithHarnessPermissionMode` — after `validateConfig` / `applyDefaults`
and before `startSession`. That call site, not a free-floating statement, is where a fourth knob
translator would go. The three are not symmetric: `Effort` is validated and hard-fails, `Model` is
never validated (an unsupported harness is a silent no-op), and `PermissionMode` is validated by
`validatePermissionMode`, restricted to the claude/codex harnesses, value-rejecting per harness
(codex has no `plan`), and additionally rejected when a bypass-enabling flag — see the exported
`BypassEnablingFlags(cfg.Harness)` — is already present in `Args`.

Alongside those, the package exports helpers for reasoning about the rungs themselves:
`PermissionRungs()` (the canonical rungs, least→most permissive, a fresh slice per call),
`MorePermissive()`, `BypassEnablingFlags()` and `BypassReachableFlags()` — the latter being the wider
set that decides whether bypass is on the session's Shift+Tab ring, and the reason claude's
unlock-only `--allow-dangerously-skip-permissions` is recognized for ring membership while staying out
of the contradiction check above. Their full signatures and godoc live in the generated
`docs/MODULES.md`, which `harness docs markdown` regenerates from the AST — consult it there rather
than duplicating a signature list here that would drift against it.

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

`Wait` returns once the harness's output has been read to its end, so the `Result` and
`RecentOutput` hold everything the harness printed before it exited. A process the harness left
behind that keeps the terminal open gets one second, after which the output is cut off. On Linux a
leftover that holds the terminal without writing still delays `Wait` until it exits, because closing
the master cannot interrupt a read already waiting on it.

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

`Config.Trace` is a `trace.Emitter` — diagnostic observations. The trace vocabulary is **not** part of
the API stability surface; never make control-flow decisions on it.

```go
type Event struct {
	At     time.Time      `json:"at"`
	Kind   string         `json:"kind"`
	Fields map[string]any `json:"fields,omitempty"`
}
type Emitter interface{ Emit(Event) }

var Discard Emitter                              // the default when Config.Trace is nil
func NewWriterEmitter(w io.Writer) Emitter        // one JSON object per line
func NewSlogAdapter(logger *slog.Logger) Emitter  // Kind → message, Fields → attrs
```

| Phase | Kinds |
|---|---|
| Startup | `wrapper_started`, `pty_opened` |
| Terminal setup (TTY passthrough only) | `winsize_initial`, `raw_mode_enabled`, `raw_mode_setup_failed`, `winsize_changed` |
| Quiet thresholds | `output_quiet`, `output_classify_threshold`, `harness_stale` |
| Classification | `harness_api_error`, `harness_blocked_by_cost`, `harness_retry_later`, `harness_waiting_for_input`, `harness_classified` |
| Shutdown | `pty_closed`, `harness_exited` |

`pty_closed` carries `output_drained`: `false` means the output was cut off because a process the
harness left behind still held the terminal.

The CLI adds two of its own around the supervised run: `wrapper_cli_signal` and `wrapper_cli_exited` —
which is what makes [`harness-wrapper status --json`](../guide/cli.md#detached-tmux) able to report a
detached session's last known state from the trace file alone.

Classification traces fire on **every** dispatching tick; `SessionEvent`s do not — identical
consecutive status/reason pairs are suppressed. So a trace stream is noisier than the event stream by
design.

`Session.Events()` is the typed contract for state transitions (the `Status` vocabulary). Use events,
not trace, for control flow.

## Buffers, cadences and drop policy

Beyond the four duration defaults above, the supervisor has fixed internal bounds. They matter because
two of them **drop rather than block** — the wrapper will never let an observer back-pressure the
harness:

| Bound | Value | Behaviour when full |
|---|---|---|
| Recent-output buffer | ~64 KiB tail | oldest bytes fall off; this is what `RecentOutput()` and the classifier see |
| PTY read chunk | 32 KiB | — |
| `Session.Events()` channel | 16 | **event dropped** |
| Attach-sink queue (per `AttachOutput` writer) | 64 chunks | **bytes dropped for that sink only** |
| Classifier dispatch channel | 1 | tick skipped |
| Classifier poll cadence | `IdleQuiet / 3`, floored at 100 ms | — (5 s at the default `IdleQuiet`) |

The one path that is deliberately **lossless** is `Config.OnLine`: it is called synchronously on the
PTY read goroutine, so a slow callback back-pressures the harness rather than losing a line. That is
what makes it safe to hang session-id capture and transcript streaming off it.

Classification does not begin until the harness has emitted its first byte, and every threshold latch
resets whenever new output arrives.

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
already works unattended without the env var: the acceptance screen is detected as a
`bypass_acceptance` and auto-answered by the unattended policy's entry for that kind, so the
flag's value is cross-implementation parity plus the `IS_SANDBOX` effects,
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

**Reading the composition back out.** `wrapper.EffectiveLaunchRung(harness, args, mode)` is the
argv→rung **inverse** of `argsWithHarnessPermissionMode`: it replays the suppression rule rather than
trusting the knob, so argv wins over the knob, a bypass-enabling flag in argv is itself a definite
bypass, and it returns `""` for anything it cannot name (never as a stand-in for "default"). It is
idempotent over injection — passing already-injected args resolves to the same rung. `structured-run`
now puts its answer **on the wire** as `StructuredTurnResult.permission_mode`
([guide](../guide/cli.md#the-reported-permission-mode)); note it reports the **args half only**, so a
`--sandbox-defaults` run and a bare `--permission-mode bypass` run both report `bypass` while
differing in root and acceptance-screen behaviour.

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
| claude bypass-acceptance screen (`bypass`) | human answers | surfaced (`bypassAnchor` → `bypass_acceptance`, nil policy) → tty chooser | auto-accepted by the `bypass_acceptance` policy entry |
| codex `-s` sandbox axis | enforced by codex | enforced by codex | **enforced by codex** |
| codex `-a` approval axis | human answers | surfaced → tty chooser | **auto-approved** by `oneshot.AutoAcceptAnswer` |

Known limitations behind that table, all tracked as follow-ups and deliberately **not** addressed
here:

- no detector for claude-code's per-tool permission dialog, so `plan` / `manual` / `ask` stall
  unattended turns to the deadline;
- `pkg/oneshot.AutoAcceptAnswer` is wired unconditionally, so it auto-approves `codex.KindApproval`
  even when a restrictive rung was requested (an approval policy would have to be kind-aware);
(A third limitation used to sit here: the bypass acceptance screen shared the `trust_prompt` kind
with folder trust, so no policy could target it independently. **Resolved** — `claudecode.DetectInput`
now stamps it `bypass_acceptance` (`claudecode.KindBypassAcceptance`), and the unattended policies
name both kinds explicitly so the behaviour in the table above is unchanged.)

The flag name and its usage string are a deliberate cross-repo mirror: `cmd/harness-wrapper/testdata/flags.golden`
here and the TypeScript half's `test/cli/testdata/wrapper-flags.golden` are meant to agree. If they
diverge, raise it in the META-HARNESS ticket rather than forking behavior on one side.

Dedup rules make the injection idempotent against caller-supplied values:

- the arg is not appended when already present as the exact token **or** in the
  `--dangerously-skip-permissions=<value>` spelling — the `<value>` is **not read**, so
  `--dangerously-skip-permissions=false` counts as present. That now has a second consequence beyond
  suppressing the injection: `EffectiveLaunchRung` matches the same prefix, so such an argv puts a
  possibly-wrong `bypass` on the wire in `StructuredTurnResult.permission_mode`. Over-reporting is the
  safe direction under that field's never-under-report rule, but it makes `bypass` a claim about
  *this repo's* reading of argv, not about claude's actual parse;
- `IS_SANDBOX=1` is not appended when the env already defines the `IS_SANDBOX` key (whatever its
  value — containers may set it), matching the key exactly at the `=` boundary so a
  prefix-sharing key like `IS_SANDBOXED` never suppresses it.

The injection lives in `cmd/harness-wrapper` (`applySandboxDefaults`), **not** in
`pkg/harness.RunTurn`: `TurnConfig.Args` keeps its documented verbatim passthrough, and the
danger-carrying policy toggle stays auditable at the CLI boundary. There is **no silent injection
anywhere** — without the flag, nothing is added.

`IS_SANDBOX=1` has exactly one writer — `applySandboxDefaults` in `cmd/harness-wrapper`
(`grep IS_SANDBOX` over non-test Go: every other hit is a comment). A `bypass` rung arriving from
`pkg/wrapper`, `pkg/chat`, `pkg/oneshot` or the chatd wire never implies it. Over the wire that is
the documented contract, not a gap — see
[`permission_mode` semantics](../guide/gateway.md#permission-mode-semantics).

That env/args split is load-bearing and deliberate: the **arg** half must be reachable by every
`wrapper.Start` caller — passthrough included — so it lives in `pkg/wrapper`; the **env** half grants
root and must stay auditable in one CLI file, so it lives in `applySandboxDefaults` and nowhere else.
For the same reason the compose path guards on the literal harness name `"claude"` with no
`normHarness` normalization (unlike `pkg/wrapper`, which does normalize): the CLI's supported names
are exactly `claude`, `codex`, `opencode`, `pi`, so the `claude-code` alias never reaches it from the
CLI, and normalizing would quietly widen the set of invocations receiving the root-enabling env half.

## Contained launches

`Config.Containment` selects a separate start path, `startContainedSession`; with it nil, `Start`
takes the `exec.Cmd` + `pty.Start` path exactly as before and never reaches `internal/contain`.

1. `contain.Prepare` normalizes the request, resolves the harness profile, identifies the executable,
   pins every granted path (openat2, `O_PATH`) and runs the overlap, managed-state and cgroupfs checks
   against those objects, provisions private state and the minimal environment, creates the session
   cgroup (recording it before launch), and builds the ruleset from the pinned descriptors. Any
   failure is `ErrContainmentRefused` (stage in the trace) and nothing has started.
2. The PTY pair is opened from raw descriptors (`/dev/ptmx`, `TIOCGPTPEER`), outside the netpoller;
   the session terminal gets its own device rule.
3. The child is started from a locked thread with a private descriptor table and a thread-scoped
   domain ([ADR-004](decisions/adr-004-thread-scoped-landlock.md)); the master becomes an `*os.File`
   through `os.NewFile` only afterwards. It stays a blocking descriptor, so its output is read in
   `poll(2)` alongside an eventfd: after the drain budget the supervisor signals that eventfd, because
   closing a blocking master would not end a read that a terminal holder keeps waiting.
4. The session waits on an `os.Process` (pidfd), reproduces `exec.CommandContext`'s cancellation
   (SIGTERM to the group, then SIGKILL to the harness after `WaitDelay`), and ends through
   `finishContained`: under cgroup supervision SIGTERM to the group unless termination already sent it,
   the grace period, `cgroup.kill`, `populated 0`, cgroup removed, ephemeral state deleted — so `Wait`
   returns only once the cgroup is empty, however the harness exited. Without supervision it escalates
   over the process group as an uncontained session does and keeps the private state
   (`cleanup: incomplete`).

`Session.Containment()` returns the applied policy (and, after `Wait`, the cleanup outcome). Trace
events: `containment_applied` (the applied policy, after a successful start), `containment_refused`
(`stage`, `error`) and `containment_cleanup` (`supervision`, `cleanup`). Errors:
`ErrContainmentUnsupported` and `ErrContainmentRefused` wrap `ErrInvalidConfig`; an `EACCES` from exec
is `ErrLaunchDenied`, which wraps `ErrPTYAllocation`; a missing binary is `ErrBinaryNotFound`. See
[Landlock containment](../guide/containment.md) for the policy model.

### Login launches

`StartLogin` (login.go) is a thin client of `Start`. It resolves the profile's `LoginFlow`
(`contain.LoginFlowFor`: the pinned login and status arguments and the patterns that read their
output), makes the `StateDir` absolute and creates it, and starts the login command with
`contain.LaunchOptions{Login: true}` on the context. That option reaches `Prepare` as `Input.Login`, and
it is not reachable from outside the module. In login mode `Prepare`:

- admits an inactive profile;
- refuses any arguments but the flow's two commands, a request without `StateDir`, managed state, and
  a login whose profile says it binds TCP (`tcp_bind`) under `RestrictTCP`;
- skips codex's rung check;
- removes the profile's `auth_env` names from the caller's environment before seeding and before
  building the child's, so `PassEnv` cannot bring them back.

The session runs in the `StateDir` with a classifier that never fires (signing in can take minutes of
quiet) and a 1024-column terminal. `loginOutput` is its `Stdout`. Each write rescans the whole
kept output (at most 1 MiB) rendered by `terminalText`: escapes removed, OSC 8 targets kept as words,
cursor-forward and column moves rendered as spaces. The URL and the one-time code are read only from
complete lines, so a write that ends mid-line never yields a truncated value. `Prompt` returns once
the URL, the code (when the flow has one) and the code prompt (when it has one) are all there.
`SubmitCode` types the code, waits 150 ms and presses Enter. Once the success text appears,
`endAfterSuccess` gives a lingering command one Enter after 3 s, then a `Stop`. `Wait` runs the status
command, again in login mode, and matches `LoggedIn` against its output.

Tests beyond the mock harness, all Linux-only:

- `internal/contain` exercises the domain with the test binary as the child: every controlled
  operation, grants racing symlink and ancestor swaps, per-session state, cgroup escapes and
  teardown, the main-thread hand-off, and `TestContainedSpawnStress` (`HW_CONTAIN_STRESS=200x320`,
  the release gate).
- `TestRealClaudeContained` and `TestRealCodexContained` (`pkg/wrapper`) are credential-free smoke
  runs of the pinned binaries under their real profiles, skipped unless `HW_REAL_CLAUDE` names the
  claude-code 2.1.270 binary or `HW_REAL_CODEX` the npm shim of an `@openai/codex` 0.144.5 package
  (with `node` on PATH). Claude reaches its composer with all TCP denied and runs a `!` bash-mode
  command; codex runs one tool call against an in-process fake model provider. The scheduled
  `harness-smoke` job of the landlock-security workflow runs both in the ABI 9 floor kernel. They
  are not the authenticated conformance runs that activate a profile.
- `TestRealClaudeLoginPrompt` and `TestRealCodexLoginPrompt` drive the real login commands, contained,
  up to their prompts. They contact the vendors' sign-in services without an account, so they also
  need `HW_REAL_LOGIN=1`. Claude's made-up code must store no login; codex's device code is read and
  the login stopped.
- `TestRealClaudeSignedIn` and `TestRealCodexSignedIn` run one prompt, with TCP restricted to 443, as
  the login a person stored with `contain-login` in `HW_REAL_CLAUDE_STATE_DIR` or
  `HW_REAL_CODEX_STATE_DIR`. They use that account's quota.

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
