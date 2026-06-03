# Harness Wrapper

Each CLI harness runs behind a wrapper. The wrapper starts the harness, monitors the process, reads its observable state, and translates harness-specific behavior into a normalized run status.

The wrapper exists because CLI agent harnesses are interactive processes, not simple batch commands. They can complete, stall, hit cost or quota limits, or stop to ask the user a question. Callers need those cases represented consistently so a run can be retried, paused, resumed, or completed without coupling the calling code to a specific CLI.

## Goals

- Provide a stable supervision layer for Claude Code, Codex, and future CLI harnesses.
- Normalize harness-specific process behavior into a small set of run states.
- Preserve enough execution context to inspect, retry, resume, or continue a run.
- Keep harness integration details out of the calling code.
- Allow a user to attach to a paused or active harness session when human control is needed.

## Implementation Stack

The wrapper will be implemented in Go.

Primary dependencies:

- [`github.com/creack/pty`](https://github.com/creack/pty): starts CLI harnesses under a pseudoterminal so interactive tools behave as if they are attached to a real terminal.
- Go standard library `os/exec`: constructs and controls the underlying harness process.
- Go standard library `context`: handles cancellation, deadlines, and run-level timeouts.
- Go standard library `io`: streams PTY output into state detectors and caller-provided output sinks.

Go is a good fit for the wrapper because the process supervisor should eventually be a small, durable binary with explicit process lifecycle control.

## First POC

The first implementation should be a transparent harness wrapper CLI around existing harnesses. It should run an actual harness CLI under a PTY, pass terminal input and output through, and emit trace logs without changing the normal harness interaction model.

See the [wrapper POC](wrapper-poc.md) for the staged implementation plan.

## Wrapper Responsibilities

- Launch the configured CLI harness with the caller's input and execution context.
- Track process liveness, exit status, output activity, and session identity.
- Detect idle output, stalled progress, cost limits, recoverable errors, and user-input prompts.
- Return raw harness output, transcript references, or session references to the caller for persistence.
- Return a normalized run state to the caller.
- Preserve enough context to retry, resume, or continue the run later.
- Support attaching a user terminal to a live PTY session when the harness needs user input or manual inspection.

## Observed Harness States

- **Idle**: The harness output has stopped changing and the latest output does not match any actionable state. The wrapper should emit `harness_idle`; the caller can then inspect the latest output, collect artifacts, verify, or continue.
- **Stuck**: The harness is still running but no useful progress is being made and the latest output indicates a recoverable problem, such as an API error. The caller should stop or suspend the attempt and retry later according to its retry policy.
- **Cost-limited**: The harness stopped because it ran out of available budget, credits, quota, or cost allowance. The caller should record the blocked state and resume only when continuation is possible.
- **Needs input**: The harness is asking the user a question or waiting for user-provided input. The caller should pause, persist the question, and emit an event that external coordination is required.

## Normalized Run States

- `idle`: The harness output has stopped changing and no actionable state was detected.
- `retry_later`: The harness appears stuck or temporarily unable to make progress.
- `blocked_by_cost`: The harness cannot continue until budget, credits, quota, or cost limits allow continuation.
- `waiting_for_input`: The harness asked a question and needs user input before continuing.
- `failed`: The harness exited with an unrecoverable error.

`completed` is a caller-level outcome, not a wrapper-level state. The caller may mark a run completed after it receives `idle`, inspects the latest output, collects artifacts, and verifies the result.

## Idle Detection

The wrapper should not assume that a harness is finished just because the user interacted with it or because the harness stopped printing briefly. Agent CLIs can ask for user input multiple times.

Idle detection should be based on output changes over time:

1. Track the latest terminal output snapshot.
2. Track when that snapshot last changed.
3. If output does not change for a short threshold, such as 15 seconds, continue waiting.
4. If output still has not changed for a longer threshold, such as 1 minute, classify the latest output.
5. If the latest output matches a prompt, emit `waiting_for_input`.
6. If it matches cost or quota exhaustion, emit `blocked_by_cost`.
7. If it matches a recoverable/API error, emit `retry_later`.
8. If it matches none of the actionable states, emit `harness_idle`.

The exact thresholds should be configurable per harness. The defaults can start with a 15 second quiet threshold and a 1 minute classification threshold.

## Interaction Modes

Clients can interact with the wrapper in two modes.

### Event Based

The wrapper pushes or emits events as its internal state changes.

Useful events:

- `harness_started`
- `harness_output_changed`
- `harness_idle`
- `harness_waiting_for_input`
- `harness_blocked_by_cost`
- `harness_retry_later`
- `harness_failed`
- `harness_user_attached`
- `harness_user_detached`

### Polling Based

A client periodically calls `Inspect` and renders the current wrapper state.

Polling should return the same state model used by event-based mode. The only difference is delivery: event-based clients subscribe to changes, while polling clients repeatedly ask for the latest state.

## Interface

The wrapper is a Go library at `pkg/wrapper`. It is importable by the in-repo CLI binary (`cmd/harness-wrapper`) and by any external Go module that wants to embed supervised harness runs.

> **Historical note.** An earlier draft of this document proposed a 5-method `HarnessWrapper` interface (`Start` / `Inspect` / `Continue` / `Attach` / `Stop`) keyed on a `runID string`. That shape is daemon-RPC-flavored — it only makes sense if the wrapper owns a registry of live runs that callers look up by opaque ID. For an in-process Go library, callers already hold a Go handle to the live run and don't need string IDs to find it. The interface below is the actual Go shape.

### Entry points

The package exposes two entry points. `Run` is synchronous and best for one-shot,
foreground use; `Start` returns a live `*Session` handle for callers that need to
observe state transitions, stream output, or stop the run cleanly. `Run` is a thin
convenience over `Start` + `Wait`.

```go
package wrapper

func Run(ctx context.Context, cfg Config) (Result, error)
func Start(ctx context.Context, cfg Config) (*Session, error)

// ClassifyOutput is a stateless, post-hoc classification helper — not a run
// entry point. It applies the same per-harness patterns the wrapper uses live
// to a finished output blob (e.g. a log tail) and returns the Classification.
func ClassifyOutput(harness, output string) Classification
```

`Run` is synchronous: it starts the harness, supervises it, and returns when the process exits or `ctx` is cancelled. A non-nil `err` always indicates the wrapper itself failed; harness outcomes (clean exit, non-zero exit, signal kill, classified stop) come back through `Result` with `err == nil`. Context cancellation produces `Result.Status == StatusInterrupted` and does **not** propagate `ctx.Err()` as the returned error.

`ClassifyOutput` runs the resolved per-harness classifier (selected by `harness` name, falling back to the generic cost/transport default for unknown names) over output the wrapper already produced — for example a daemon reading an exited agent's log tail — and returns the same `Classification` (cost, rate-limit, transport/connection-refused, API errors with HTTP code) the live run would have surfaced. It forces the idle gate open so cost/retry/transport patterns are eligible, and leaves the prompt gate closed since a finished process is not awaiting input. It is also what a failed run's own supervisor uses internally to upgrade a bare `StatusFailed` exit into an actionable status when the harness died too fast for the live classifier to poll.

### Config

```go
type Config struct {
	BinaryPath string        // required; absolute or PATH-resolvable
	Args       []string
	WorkingDir string
	Env        []string

	Stdin  io.Reader         // nil = no input forwarding; an *os.File TTY enables raw-mode passthrough
	Stdout io.Writer         // required

	IdleQuiet      time.Duration // quiet threshold (default 15s)
	IdleClassify   time.Duration // idle-classification threshold, must be >= IdleQuiet (default 60s)
	StaleThreshold time.Duration // mid-run stale advisory (default 5m; set negative to disable)
	WaitDelay      time.Duration // SIGTERM→SIGKILL grace on cancellation (default 5s)

	Trace      trace.Emitter // diagnostic events; observability only
	Harness    string        // selects a built-in classifier ("claude", "codex", "gemini")
	Classifier Classifier    // explicit classifier; wins over Harness when both are set
}
```

When `Stdin` and `Stdout` are both real `*os.File` TTYs the wrapper auto-enables raw mode and SIGWINCH forwarding for the duration of the run, restoring terminal state on return. Headless callers (file/pipe/`bytes.Buffer` `Stdout`, nil or non-file `Stdin`) skip both — no caller flag.

### Status

`Result.Status` and `SessionEvent.Status` share one normalized vocabulary. *Terminal*
statuses end the run (the wrapper SIGTERMs the harness); *non-terminal* ones are mid-run
advisories emitted while the harness keeps running.

```go
type Status string
const (
	StatusIdle            Status = "idle"               // exited cleanly, no actionable state
	StatusFailed          Status = "failed"             // non-zero exit code
	StatusBlockedByCost   Status = "blocked_by_cost"    // budget/quota/rate-limit hit (terminal)
	StatusRetryLater      Status = "retry_later"        // transient/recoverable error (terminal)
	StatusAPIError        Status = "api_error"          // upstream API error, harness still running (non-terminal)
	StatusWaitingForInput Status = "waiting_for_input"  // paused at an interactive prompt (non-terminal)
	StatusStale           Status = "stale"              // no output for StaleThreshold (non-terminal advisory)
	StatusInterrupted     Status = "interrupted"        // terminated by signal or caller
	StatusUnknown         Status = "unknown"            // could not classify
	StatusBinaryNotFound  Status = "binary_not_found"   // configured binary not on PATH
)
```

### Result and sentinel errors

```go
type Result struct {
	Status       Status
	ExitCode     int       // 128+signum if signal-killed; -1 if never started
	Signal       string    // signal name if signal-killed, else empty
	Reason       string    // human-readable detail for Failed/Interrupted/Unknown (not for parsing)
	PID          int
	StartedAt    time.Time
	EndedAt      time.Time
	LastOutputAt time.Time
}

var (
	ErrInvalidConfig  = errors.New("wrapper: invalid config")
	ErrBinaryNotFound = errors.New("wrapper: binary not found")
	ErrPTYAllocation  = errors.New("wrapper: pty allocation failed")
	ErrPTYRead        = errors.New("wrapper: pty read failed")
)
```

A non-nil error from `Run`/`Start` means the wrapper itself failed to start the harness — invalid `Config` (`ErrInvalidConfig`), a missing binary (`ErrBinaryNotFound`), or PTY allocation failure (`ErrPTYAllocation`). Once `Start` returns a non-nil `*Session`, every harness outcome instead flows through `Result`/`SessionEvent` with a nil error. `Run` additionally mirrors a missing-binary failure into its returned `Result` (`Status == StatusBinaryNotFound`, `ExitCode == -1`) so callers that inspect only the `Result` still see the classified status — the `ErrBinaryNotFound` error is returned alongside it.

### Session handle

`Start` returns a `*Session` that supervises the run in the background. Concurrent calls to its methods are safe.

```go
func (s *Session) Wait() (Result, error)          // block for the terminal Result (repeatable)
func (s *Session) Stop(ctx context.Context) error // request graceful SIGTERM→SIGKILL shutdown
func (s *Session) Snapshot() Snapshot             // point-in-time view (status, reason, timestamps)
func (s *Session) Events() <-chan SessionEvent    // status-change stream; closed after the terminal event
func (s *Session) PID() int
func (s *Session) RecentOutput() string           // last ~64KB of raw PTY output

// In-process live I/O (used by pkg/chat and in-process watchers):
func (s *Session) AttachOutput(w io.Writer) func()          // tee PTY output to w; returns a detach func
func (s *Session) WriteStdin(p []byte) (int, error)         // forward keystrokes to the harness
func (s *Session) Resize(cols, rows uint16) error           // resize the PTY
func (s *Session) AcquireWriter() (release func(), ok bool) // claim the exclusive stdin writer
```

`SessionEvent` carries the normalized status plus optional detail parsed from harness output:

```go
type SessionEvent struct {
	At         time.Time
	Status     Status
	Reason     string
	Terminated bool          // true on the final event, after which Events() is closed

	HTTPCode   int           // upstream status code when Status == StatusAPIError
	RetryAfter time.Duration // wait hint parsed from the harness's error message
	ResumeAt   time.Time     // absolute reset time from a session-limit banner
	                         //   (e.g. Claude Code's "resets 6:40pm (Europe/Warsaw)")
}
```

### Trace vs. events

`Config.Trace` is a `trace.Emitter` — diagnostic observations the wrapper emits as it runs (`wrapper_started`, `pty_opened`, `harness_stale`, `harness_exited`). The trace vocabulary is **not** part of the API stability surface; do not make control-flow decisions based on trace event ordering or presence.

`*Session.Events()` is different: it's the typed contract for subscribing to harness state transitions (the `Status` vocabulary above). Use events, not trace, for control flow.

### PTY Execution

The wrapper starts each harness command under a pseudoterminal with `pty.Start`. The PTY stream is the canonical source for output capture and state detection.

```go
cmd := exec.CommandContext(ctx, cfg.BinaryPath, cfg.Args...)
cmd.Dir = cfg.WorkingDir
cmd.Env = cfg.Env

ptmx, err := pty.Start(cmd)
if err != nil {
	return nil, fmt.Errorf("%w: %v", ErrPTYAllocation, err)
}
defer ptmx.Close()
```

The supervisor reads from the PTY continuously, copies output to `Stdout` (and any
`AttachOutput` sinks), updates the last-activity timestamp, and feeds the rolling
recent-output buffer into the classifier. The caller owns persistence of transcripts,
run state, events, and artifacts.

## Cross-process attach (future work)

The `*Session` attach primitives above (`AttachOutput`, `WriteStdin`, `Resize`,
`AcquireWriter`) are **in-process**: they let multiple watchers in the *same* process
share one live session, which is how `pkg/chat` tees output and serializes keystrokes.
What still does **not** exist is **cross-process** attach — letting a separately-spawned
process drive a run that a *different* process started headlessly, so a user can answer a
`waiting_for_input` question, inspect a stuck run, or take manual control after the fact.

A foreground run needs no attach concept: when the wrapper runs in the user's terminal,
the user is *already* on the PTY. The hard case is reaching a PTY master another process
owns, which requires one of:

- A long-lived daemon that owns the PTY master and bridges new clients to it.
- An IPC mechanism that passes the PTY fd between processes (Unix-domain sockets with `SCM_RIGHTS`).
- The calling application itself running as a daemon and exposing a control socket.

This is the daemon/attach path tracked as item 3 in [`plans/roadmap-v1.md`](../plans/roadmap-v1.md),
where the chosen design is a byte-proxy daemon (`harness-wrapperd`) rather than fd-passing.
For shell users today, the `harness-wrapper` CLI already covers the detached case with a
tmux-backed path (`harness-wrapper --tmux-session NAME …` plus the `attach` / `list` /
`status` / `kill` subcommands). Until the programmatic daemon lands, a headless library
caller whose run needs human input has only two escape hatches: context cancellation and
inspecting the harness's own persisted transcript afterward.

The client-side bridge will look approximately like this — living in the future daemon
client, not on `Run` or `*Session`:

```go
// Sketch only; not part of the current pkg/wrapper API.
oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
defer term.Restore(int(os.Stdin.Fd()), oldState)
go io.Copy(masterConn, os.Stdin)  // user keystrokes -> daemon -> live PTY master
_, err = io.Copy(os.Stdout, masterConn)  // PTY output -> user terminal
```

A clean detach sequence (e.g., `Ctrl-]`) returns control to the caller without killing the harness process.

Headful mode (TUI/web) is also deferred. Whatever UI eventually ships will reuse the same attach primitives.

## Detection Signals

The first implementation should prefer simple, observable signals before adding complex interpretation:

- Process exit code and termination reason.
- Output snapshot diff and time since the latest output change.
- Known cost, quota, or budget messages in harness output.
- Known user-input prompt patterns in harness output.
- Known recoverable/API error patterns in harness output.
- Harness session metadata when available.
- Explicit output artifacts produced by the harness run.

The first detection tests should use the [mock CLI harness](mock-harness.md), not live agent CLIs.

## Retry And Resume

Retries should be policy-driven. A stuck process should usually become `retry_later`, while a cost-limited process should become `blocked_by_cost` and wait until continuation is possible. A process waiting for user input should not be retried blindly; it should persist the question and wait for external input.
