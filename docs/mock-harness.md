# Mock CLI Harness

The mock CLI harness is a deterministic test binary that behaves like an interactive agent CLI. It exists to test the harness wrapper without requiring Claude Code, Codex, Google CLI, credentials, network access, or paid usage.

The mock harness should run under the same PTY runner as real harnesses. It should emit realistic terminal output, wait for input when requested, simulate stalls, and exit with predictable codes.

## Goals

- Exercise the wrapper against repeatable process states.
- Validate PTY behavior before integrating real CLI harnesses.
- Test transcript recording, state detection, attach, detach, cancellation, and timeout behavior.
- Provide a shared behavior matrix for Claude Code, Codex, Google CLI, and future harness adapters.

## Location

Use one generic mock harness first:

```text
test/fakeharness/mock/
└── main.go
```

Harness-specific fake commands can be added later only if their behavior needs to diverge:

```text
test/fakeharness/
├── mock/
├── claude/
├── codex/
└── google/
```

The generic mock should be enough for the first wrapper tests because adapter-specific tests can configure different output patterns and command names around the same fake binary.

## Command Shape

```text
mock-harness --mode completed
mock-harness --mode stuck
mock-harness --mode cost-limited
mock-harness --mode needs-input
mock-harness --mode failed
mock-harness --mode api-error
```

Optional flags:

```text
--delay 250ms
--exit-code 1
--failed-msg "Fatal: workspace is not writable."
--steps 3
--prompt "Continue? [y/N] "
--expected-input "y"
--api-error-msg "API Error: 529 Overloaded."
--api-error-repeat 1
--api-error-repeat-gap 100ms
--api-error-recover
--api-error-heartbeat 200ms
```

## Modes

### `completed`

Simulates a successful agent run.

Behavior:

1. Prints a startup banner.
2. Prints several progress messages.
3. Prints a final completion marker.
4. Exits with code `0`.

Example output:

```text
Mock Agent CLI
Planning task...
Editing files...
Verification passed.
DONE
```

Expected wrapper state: `idle` after output stops changing and no actionable state is detected.

### `stuck`

Simulates a harness that remains alive but stops making meaningful progress.

Behavior:

1. Prints a startup banner.
2. Prints one progress message.
3. Sleeps indefinitely or longer than the test timeout.

Example output:

```text
Mock Agent CLI
Thinking...
```

Expected wrapper state: `retry_later` after the inactivity threshold.

### `cost-limited`

Simulates a harness that stops because budget, quota, credits, or cost allowance is exhausted.

Behavior:

1. Prints a startup banner.
2. Prints a cost-limit message.
3. Exits with a configurable non-zero code or waits, depending on the test case.

Example output:

```text
Mock Agent CLI
ERROR: quota exceeded. Please try again after your usage limit resets.
```

Expected wrapper state: `blocked_by_cost`.

### `needs-input`

Simulates a harness asking the user a question.

Behavior:

1. Prints a startup banner.
2. Prints a question prompt.
3. Waits for input on stdin.
4. If the input matches `--expected-input`, prints a completion marker and exits `0`.
5. If the input does not match, prints a rejection message and keeps waiting or exits with a configurable code.

Example output:

```text
Mock Agent CLI
Need approval to continue.
Continue? [y/N]
```

Expected wrapper state before user input: `waiting_for_input`.

Expected wrapper state after attach and accepted input: `completed`.

### `failed`

Simulates an unrecoverable harness failure.

Behavior:

1. Prints a startup banner.
2. Prints an error message.
3. Exits with a non-zero code.

Example output:

```text
Mock Agent CLI
Fatal: workspace is not writable.
```

Expected wrapper state: `failed`.

### `api-error`

Simulates the harness's upstream model API returning an error mid-run (e.g. an HTTP 429/529), which the wrapper surfaces **without** killing the harness.

Behavior:

1. Prints a startup banner.
2. Prints an API-error line (default `API Error: 529 Overloaded.`), repeated `--api-error-repeat` times with `--api-error-repeat-gap` between prints.
3. If `--api-error-recover` is set, resumes completed-style progress and exits `0`; otherwise heartbeats every `--api-error-heartbeat` until it receives a signal.

Example output:

```text
Mock Agent CLI
API Error: 529 Overloaded.
```

Expected wrapper state: `api_error` (non-terminal — the wrapper keeps the harness alive and reports the upstream HTTP status code).

## Transcript Profiles (not implemented)

The original design proposed a `--transcript generic|codex|claude|google` flag so the
mock could emit harness-flavored wording. It was never built, and the flag does not
exist. In practice, adapter detectors are tested against **real recorded output** under
`test/corpus/` (see [`../test/corpus/README.md`](../test/corpus/README.md)) rather than
mock-generated text, so the mock stays harness-neutral.

## PTY Behavior

The mock harness should behave like a terminal program:

- Detect whether stdin/stdout are attached to a terminal when useful.
- Flush output after each prompt or progress message.
- Read line-oriented input from stdin for `needs-input`.
- Support long-running processes for stuck and attach tests.
- Exit predictably when it receives `SIGTERM` or context cancellation.

## Test Usage

Tests should build the mock harness into a temporary test binary, then run that binary through the real PTY runner.

Test scenarios:

- Completed mode returns `idle` after output stops changing and no actionable state is detected.
- Stuck mode returns `retry_later`.
- Cost-limited mode returns `blocked_by_cost`.
- Needs-input mode returns `waiting_for_input`.
- Attach can send the expected input to needs-input mode.
- Detach returns control without killing the process.
- Failed mode returns `failed`.
- Transcript contains all emitted output.
- Cancellation stops a running mock harness.
- Timeout stops a running mock harness.

## Implementation Notes

The mock harness should be intentionally boring Go code:

- Use `flag` for arguments.
- Use `fmt.Fprintln` for output.
- Use `bufio.Reader` for stdin.
- Use `time.Sleep` and timers for delays.
- Handle `os.Interrupt` and `SIGTERM` cleanly.

Avoid coupling the mock harness to any specific caller's internals. It should behave like an external CLI process.
