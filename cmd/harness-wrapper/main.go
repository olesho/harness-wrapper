// Command harness-wrapper is a thin CLI front-end for pkg/wrapper.Run.
// It supervises an external CLI agent harness under a pseudoterminal and
// forwards the user's terminal to it.
//
// Usage:
//
//	harness-wrapper [wrapper-flags] <name> -- <harness args>
//
// Supported harness names: codex, claude.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 1 {
		switch args[0] {
		case "-h", "--help", "help":
			printUsage(os.Stdout)
			return 0
		}
	}
	// Tmux-mode subcommands. These don't follow the `<harness> -- <args>`
	// shape, so they're routed before the main parser.
	if len(args) >= 1 {
		switch args[0] {
		case "attach", "status", "kill", "list":
			return runTmuxSubcommand(args)
		case "run":
			// One-shot prompt mode: the proper substitution for `claude -p`.
			// Drives one turn via pkg/chat and prints the reply.
			return runOneShot(args[1:])
		case "structured-run":
			// Structured sibling of `run`: drives one turn and emits a single
			// machine-readable turnproto.StructuredTurnResult JSON line.
			return runStructuredRun(args[1:])
		}
	}
	return runHarnessWrapper(args)
}

func runHarnessWrapper(args []string) int {
	parsed, err := parseHarnessWrapperArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// --sandbox-defaults is a run/structured-run policy toggle; an interactive
	// passthrough session should make that call in the harness itself, so it is
	// rejected — explicitly, not silently ignored. The check sits BEFORE
	// resolveHarness so the rejection is deterministic on machines without the
	// harness binary on PATH, and before the tmux branch so no session is ever
	// spawned for a rejected invocation.
	if parsed.SandboxDefaults {
		fmt.Fprintln(os.Stderr, "harness-wrapper: --sandbox-defaults is only supported by run and structured-run")
		return 2
	}
	binPath, err := resolveHarness(parsed.HarnessName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	// Parent of a tmux-backed run: spawn the detached session and exit.
	// The harness keeps running inside the pane via a re-exec into this
	// same binary with --tmux-child set.
	if parsed.TmuxSession != "" {
		return runTmuxSpawn(parsed, binPath)
	}

	traceEmitter, closeTrace, err := openTraceEmitter(parsed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
		return 1
	}
	defer closeTrace()

	ctx, stopSignalWatcher := signalAwareContext(context.Background(), traceEmitter)
	defer stopSignalWatcher()

	res, err := wrapper.Run(ctx, wrapper.Config{
		BinaryPath: binPath,
		Args:       parsed.HarnessArgs,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Trace:      traceEmitter,
		Harness:    parsed.HarnessName,
		Effort:     parsed.Effort,
		Model:      parsed.Model,
	})
	if err != nil {
		emitCLIExitTrace(traceEmitter, res, err)
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
		return 1
	}
	emitCLIExitTrace(traceEmitter, res, nil)
	return exitCodeFor(res)
}

func signalAwareContext(parent context.Context, emitter trace.Emitter) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-signals:
			if emitter != nil {
				emitter.Emit(trace.Event{
					At:   time.Now(),
					Kind: "wrapper_cli_signal",
					Fields: map[string]any{
						"signal": sig.String(),
					},
				})
			}
			cancel()
		case <-done:
		}
	}()

	var stopOnce sync.Once
	return ctx, func() {
		stopOnce.Do(func() {
			signal.Stop(signals)
			close(done)
			cancel()
		})
	}
}

// emitCLIExitTrace records the wrapper CLI's final view of the run. The lower
// wrapper layer also emits pty_closed/harness_exited, but this event gives tmux
// status a final result even if the pane exits through an edge path before
// every lower-level diagnostic is visible in the trace.
func emitCLIExitTrace(emitter trace.Emitter, res wrapper.Result, runErr error) {
	if emitter == nil {
		return
	}
	fields := map[string]any{
		"status":     string(res.Status),
		"exit_code":  res.ExitCode,
		"signal":     res.Signal,
		"reason":     res.Reason,
		"pid":        res.PID,
		"started_at": res.StartedAt,
		"ended_at":   res.EndedAt,
	}
	if !res.StartedAt.IsZero() && !res.EndedAt.IsZero() {
		fields["duration_ms"] = res.EndedAt.Sub(res.StartedAt).Milliseconds()
	}
	if runErr != nil {
		fields["error"] = runErr.Error()
	}
	emitter.Emit(trace.Event{
		At:     time.Now(),
		Kind:   "wrapper_cli_exited",
		Fields: fields,
	})
}

// openTraceEmitter returns a trace.Emitter for the parsed CLI flags.
//
// Default behavior: trace events are dropped (trace.Discard). This is
// deliberate: the most common use case is running an interactive harness
// in a real terminal, where trace JSON dumped to stderr would corrupt
// the harness's TUI.
//
// Opt-in: --trace-file PATH or --trace-stderr.
//
// The returned closer is always non-nil.
func openTraceEmitter(args harnessWrapperArgs) (trace.Emitter, func(), error) {
	switch {
	case args.TraceFile != "":
		f, err := os.OpenFile(args.TraceFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, func() {}, fmt.Errorf("open trace file %q: %w", args.TraceFile, err)
		}
		return trace.NewWriterEmitter(f), func() { _ = f.Close() }, nil
	case args.TraceStderr:
		return trace.NewWriterEmitter(os.Stderr), func() {}, nil
	default:
		return trace.Discard, func() {}, nil
	}
}

// exitCodeFor maps a wrapper Result onto a process exit code following
// the conventional shell idiom (130 for SIGINT-style interrupt; the
// harness's own code for clean and failed exits).
func exitCodeFor(res wrapper.Result) int {
	switch res.Status {
	case wrapper.StatusIdle:
		return res.ExitCode
	case wrapper.StatusFailed:
		if res.ExitCode > 0 {
			return res.ExitCode
		}
		return 1
	case wrapper.StatusBlockedByCost:
		if res.ExitCode > 0 {
			return res.ExitCode
		}
		return 1
	case wrapper.StatusInterrupted:
		if res.ExitCode > 0 {
			return res.ExitCode
		}
		return 130
	case wrapper.StatusUnknown:
		if res.ExitCode > 0 {
			return res.ExitCode
		}
		return 0
	default:
		return 1
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: harness-wrapper [wrapper-flags] <name> -- <harness args>")
	fmt.Fprintln(w, "       harness-wrapper run [wrapper-flags] <name> -- <harness args>   (prompt on stdin)")
	fmt.Fprintln(w, "       harness-wrapper attach <session>")
	fmt.Fprintln(w, "       harness-wrapper status <session> [--json]")
	fmt.Fprintln(w, "       harness-wrapper kill <session>")
	fmt.Fprintln(w, "       harness-wrapper list")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "wrapper flags (must come BEFORE the harness name):")
	fmt.Fprintln(w, "  --trace-file PATH       write trace events as NDJSON to PATH")
	fmt.Fprintln(w, "  --trace-stderr          write trace events as NDJSON to stderr")
	fmt.Fprintln(w, "  --tmux-session NAME     spawn the run inside a detached tmux session")
	fmt.Fprintln(w, "                          named hw-<NAME> and exit immediately")
	fmt.Fprintln(w, "  --auto-accept           run: auto-answer blocking prompts (affirmative)")
	fmt.Fprintln(w, "                          even with a terminal attached, instead of asking")
	fmt.Fprintln(w, "  --sandbox-defaults      run/structured-run, claude only (DANGEROUS): inject")
	fmt.Fprintln(w, "                          --dangerously-skip-permissions and IS_SANDBOX=1;")
	fmt.Fprintln(w, "                          no-op for other harnesses; rejected by passthrough")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "supported harness names: claude, codex, opencode, pi")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "By default trace events are dropped, since stderr would corrupt an")
	fmt.Fprintln(w, "interactive harness TUI. Pass --trace-file or --trace-stderr to enable.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "`run` is the one-shot mode — the proper substitution for `claude -p` /")
	fmt.Fprintln(w, "`codex exec`. It reads the prompt from stdin, drives ONE turn through the")
	fmt.Fprintln(w, "real harness (PTY + turn detection), prints the reply to stdout, and exits")
	fmt.Fprintln(w, "non-zero if the turn errors. --timeout via the HARNESS_WRAPPER_RUN_TIMEOUT")
	fmt.Fprintln(w, "env var (default 15m).")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "When `run` is invoked from an attached terminal it is interactive by")
	fmt.Fprintln(w, "default: a blocking trust/permission prompt is surfaced to the human on")
	fmt.Fprintln(w, "/dev/tty (bounded by the run deadline, with an auto-accept fallback). With")
	fmt.Fprintln(w, "no controlling terminal (CI, pipes, nohup) it stays fully unattended.")
	fmt.Fprintln(w, "Pass --auto-accept to force the always-auto behavior even with a terminal.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Tmux mode lets you detach from a long-running agent: the harness")
	fmt.Fprintln(w, "keeps running inside the tmux session, and `harness-wrapper attach`")
	fmt.Fprintln(w, "(or `tmux attach -t hw-<NAME>`) reconnects you. Trace events go to")
	fmt.Fprintln(w, "~/.harness-wrapper/sessions/<NAME>.trace.ndjson by default.")
}
