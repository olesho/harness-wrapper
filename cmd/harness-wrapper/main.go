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

	res, err := wrapper.Run(context.Background(), wrapper.Config{
		BinaryPath: binPath,
		Args:       parsed.HarnessArgs,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Trace:      traceEmitter,
		Harness:    parsed.HarnessName,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
		return 1
	}
	return exitCodeFor(res)
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
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "supported harness names: codex, claude, gemini")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "By default trace events are dropped, since stderr would corrupt an")
	fmt.Fprintln(w, "interactive harness TUI. Pass --trace-file or --trace-stderr to enable.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Tmux mode lets you detach from a long-running agent: the harness")
	fmt.Fprintln(w, "keeps running inside the tmux session, and `harness-wrapper attach`")
	fmt.Fprintln(w, "(or `tmux attach -t hw-<NAME>`) reconnects you. Trace events go to")
	fmt.Fprintln(w, "~/.harness-wrapper/sessions/<NAME>.trace.ndjson by default.")
}
