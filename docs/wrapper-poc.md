# Phase 1: Transparent Wrapper POC

> **Status: implemented.** This is the original staged POC plan, kept for historical
> context. The transparent wrapper shipped and has since grown the full supervisor API
> (status classifier, `*Session` handle, attach primitives). For the current shape see
> [`wrapper.md`](wrapper.md) and the [README](../README.md).

The first wrapper implementation should be a transparent CLI that wraps existing harnesses without changing how users interact with them.

The goal is to prove PTY execution, pass-through terminal behavior, trace logging, and basic process visibility before building richer caller integrations.

The transparent wrapper must not write to any caller-owned persistent storage. In Phase 1, it should only run the actual harness CLI and emit trace logs that explain what the wrapper observed.

## POC Goals

- Run an existing CLI harness under `github.com/creack/pty`.
- Pass user input through to the harness.
- Pass harness output through to the user's terminal.
- Emit trace logs while the user interacts normally.
- Log process metadata, exit code, start time, end time, and command arguments.
- Keep the user experience as close as possible to running the harness directly.

## Non-Goals

- No external orchestrator integration.
- No retry policy.
- No event chaining.
- No headful UI.
- No complex state detection.
- No long-lived daemon.
- No writes to caller-owned persistent storage.

The POC should only classify what can be observed reliably from the process lifecycle and output stream.

## Command Shape

The transparent wrapper should accept a harness name and pass the remaining arguments through to that harness.

```text
harness-wrapper codex -- <codex args>
harness-wrapper claude -- <claude args>
```

Examples:

```text
harness-wrapper codex -- .
harness-wrapper claude -- --dangerously-skip-permissions
```

The separator `--` keeps wrapper flags separate from harness flags.

## Trace Logs

The POC should emit trace logs. Trace logs are for visibility, not durable caller-owned storage.

By default, trace logs can go to stderr so stdout remains close to the actual harness output. A flag can optionally write traces to a file:

```text
harness-wrapper codex --trace-file ./trace.log -- .
```

Trace logs should include:

- wrapper start
- harness name
- binary path
- arguments
- working directory
- environment overrides, if any
- start time
- end time
- exit code
- terminal size, if available
- interrupt or signal handling
- PTY start and close events
- coarse output activity timestamps

Trace logs should not include full terminal transcripts by default. Full transcripts belong to later caller-owned storage.

## Transparent PTY Flow

The POC should bridge the user's terminal and the harness PTY:

```text
user terminal stdin  -> harness PTY
harness PTY output   -> user terminal stdout
harness lifecycle    -> trace logs
```

The wrapper should restore the user's terminal state even if the wrapped process exits with an error or the user interrupts the process.

The wrapper passes raw PTY bytes — including ANSI escapes, cursor moves, redraws — straight through to the caller's `Stdout`. **Phase 1 does not strip or normalize terminal escape sequences.** Any cleaned, human-readable transcript representation is a later feature owned by the caller, not the wrapper.

## Minimal State Detection

The POC should start with minimal states:

- `idle`: output has not changed past the classification threshold and no actionable state was detected.
- `failed`: process exits with a non-zero code.
- `interrupted`: wrapper or child process receives an interrupt.
- `unknown`: wrapper cannot determine a reliable final state.

The POC may also record output hints for later analysis, but it should not block the transparent user flow to classify advanced states. `completed` remains a caller-level result and should not be inferred by the transparent wrapper.

Advanced states can come after the POC:

- `retry_later`
- `blocked_by_cost`
- `waiting_for_input`

## Implementation Steps

1. Create the Go module.
2. Add `cmd/harness-wrapper`.
3. Add command parsing.
4. Add harness binary resolution for `codex` and `claude`.
5. Add PTY start and terminal pass-through.
6. Add trace logging.
7. Add optional `--trace-file`.
8. Add mock harness tests for transparent pass-through.
9. Try the wrapper manually with Codex and Claude Code.
10. Add basic state classification from output-diff idle detection, exit code, and interrupt handling.

## POC Tests

Use the mock CLI harness first.

Test cases:

- Wrapper starts the mock harness under PTY.
- User input reaches the mock harness.
- Mock harness output is streamed back to the caller.
- Trace logs include process start, process exit, and final state.
- Unchanged output past the threshold records `idle`.
- Non-zero exit code records `failed`.
- Interrupt records `interrupted`.
- Terminal state is restored after process exit.

Live harness tests should stay manual or opt-in until the POC behavior is stable.

## Success Criteria

The POC is successful when this works:

```text
harness-wrapper codex -- <normal codex invocation>
```

The user should be able to use Codex normally, and the harness wrapper should produce enough trace logs to understand what happened around process startup, PTY lifecycle, interrupts, and process exit.
