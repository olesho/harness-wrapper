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

### Phase 1 (current): single `Run` function

```go
package wrapper

func Run(ctx context.Context, cfg Config) (Result, error)

type Config struct {
	BinaryPath, WorkingDir string
	Args                   []string
	Env                    []string
	Stdin, Stdout          *os.File       // Stdout required; nil Stdin = no input forwarding
	IdleQuiet, IdleClassify, WaitDelay time.Duration
	Trace                  trace.Emitter
}

type Status string
const (
	StatusIdle        Status = "idle"
	StatusFailed      Status = "failed"
	StatusInterrupted Status = "interrupted"
	StatusUnknown     Status = "unknown"
)

type Result struct {
	Status                       Status
	ExitCode                     int     // 128+signum if signal-killed; -1 if never started
	Signal, Reason               string
	PID                          int
	StartedAt, EndedAt           time.Time
	LastOutputAt                 time.Time
}

var (
	ErrInvalidConfig  = errors.New("wrapper: invalid config")
	ErrBinaryNotFound = errors.New("wrapper: binary not found")
	ErrPTYAllocation  = errors.New("wrapper: pty allocation failed")
	ErrPTYRead        = errors.New("wrapper: pty read failed")
)
```

`Run` is synchronous: it starts the harness, supervises it, and returns when the process exits or `ctx` is cancelled. A non-nil `err` always indicates the wrapper itself failed; harness outcomes (clean exit, non-zero exit, signal kill, idle classification) come back through `Result` with `err == nil`. Context cancellation produces `Result.Status == StatusInterrupted` and does **not** propagate `ctx.Err()` as the returned error.

When `Stdin` and `Stdout` are both real TTYs the wrapper auto-enables raw mode and SIGWINCH forwarding for the duration of `Run`, restoring terminal state on return. Headless callers (file-backed `Stdout`, nil or pipe `Stdin`) skip both — no caller flag.

### Phase 2 (planned): `Start` + `*Session`

Callers need a non-terminal handle while a wrapper session is running so they can observe state transitions, request a clean stop with a reason, and inspect the current snapshot. Phase 2 adds, additively:

```go
func Start(ctx context.Context, cfg Config) (*Session, error)

func (s *Session) Wait(ctx context.Context) (Result, error)
func (s *Session) Stop(ctx context.Context, reason StopReason) error
func (s *Session) Snapshot() Snapshot
func (s *Session) Events() <-chan Event   // typed state-change events
```

`Run` becomes a one-line convenience over `Start`/`Wait`. The four Phase 1 status constants stay; three more land alongside (`StatusRetryLater`, `StatusBlockedByCost`, `StatusWaitingForInput`). `Config` grows optional fields (`RunID`, `Classifier`, `Tee`, `OutputBufferSize`); `Result` grows fields (`HarnessSessionID`, `LatestOutput`). All zero-value defaults preserve Phase 1 behavior.

### Trace vs. events

`Config.Trace` is a `trace.Emitter` — diagnostic observations the wrapper emits as it runs (`wrapper_started`, `pty_opened`, `output_quiet`, `harness_exited`). The trace event vocabulary is **not** part of the API stability surface; do not make control-flow decisions based on trace event ordering or presence.

State-change events (Phase 2's `*Session.Events()`) are different: they're a typed contract for callers to subscribe to harness state transitions. Phase 1 has no non-terminal state transitions to emit, so the events channel only appears in Phase 2.

### `HarnessSessionID`, `LatestOutput`, etc.

Fields like `HarnessSessionID`, `TranscriptRef`, `OutputRef`, `LatestOutput`, `Question`, `Attached`, `RetryAfter` from earlier drafts are **Phase 2 only** — they require harness-specific banner parsing (session IDs), a rolling output buffer (latest output), or attach support (Attached). None ship in Phase 1's `Result`. Adding them later is fully additive.

### PTY Execution

The wrapper should start each harness command with `pty.Start` or `pty.StartWithSize`. The PTY stream becomes the canonical source for transcript capture and state detection.

```go
cmd := exec.CommandContext(ctx, binary, args...)
cmd.Dir = request.WorkingDirectory
cmd.Env = buildEnvironment(request.Environment)

terminal, err := pty.Start(cmd)
if err != nil {
	return HarnessState{}, err
}
defer terminal.Close()
```

The implementation should read from the PTY continuously, emit output to a caller-provided sink, update the last-activity timestamp, and feed chunks into the state detector. The caller owns persistence of transcripts, run state, events, and artifacts.

## Future Work: User Attach

A future release will let a user attach their terminal to a wrapper session that a caller started headlessly — so they can answer a `waiting_for_input` question, inspect a stuck run, or take manual control. This is **not** in Phase 1 or initial Phase 2.

Attach is harder than it looks. The Phase 1 wrapper is a one-shot, foreground process: when it runs in the user's terminal, the user is *already* attached and no separate attach flow is needed. The interesting case is when a caller started the wrapper headlessly and a separate `attach <run-id>` invocation needs to drive the same PTY master. That requires one of:

- A long-lived daemon that owns the PTY master fd and bridges new clients to it.
- An IPC mechanism that passes the PTY fd between processes (Unix-domain sockets with `SCM_RIGHTS`).
- The calling application itself running as a daemon and exposing a control socket.

None of those architectures are decided yet. This means:

- Phase 1's foreground use case works without any attach concept (the user runs the CLI and is on the PTY directly).
- Phase 2's headless use case ships without attach support; if a caller starts a wrapper session and it needs human input, the only Phase 2 escape hatches are context cancellation and inspecting the persisted transcript after the fact.
- The `pkg/wrapper` API does **not** include `Attach()` methods or attach-shaped fields (`Attached bool`, `AttachOptions`). They will be designed alongside the daemon/IPC architecture decision.

The bridge mechanics, when attach lands, will look approximately like this — but in a separate `pkg/wrapper/attach` package with its own interface, not as a method on `Run` or `*Session`:

```go
// Sketch only; not a Phase 1 or Phase 2 API.
oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
defer term.Restore(int(os.Stdin.Fd()), oldState)
go io.Copy(masterFd, os.Stdin)  // user keystrokes -> live PTY master
_, err = io.Copy(os.Stdout, masterFd)  // PTY output -> user terminal
```

A clean detach sequence (e.g., `Ctrl-]`) would return control to the caller without killing the harness process.

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
