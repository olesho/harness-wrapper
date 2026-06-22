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
	Harness    string             // selects a built-in classifier ("claude", "codex", "gemini", …)
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
