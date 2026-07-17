package main

import (
	"flag"
	"fmt"
	"io"
)

// harnessWrapperArgs is the parsed form of `harness-wrapper` invocation.
type harnessWrapperArgs struct {
	TraceFile   string
	TraceStderr bool
	Effort      string
	Model       string
	// TmuxSession requests the wrapper spawn the run inside a detached
	// tmux session named hw-<TmuxSession> and exit immediately after
	// `tmux new-session -d` succeeds. The wrapper will re-exec itself
	// inside the pane with --tmux-child set to the same name.
	TmuxSession string
	// TmuxChild is the in-pane re-exec marker. Hidden from --help; set
	// only by the parent invocation. When non-empty the wrapper behaves
	// like a normal in-process run, but trace events carry the session
	// name so consumers can correlate.
	TmuxChild string
	// AutoAccept forces `run` to auto-answer blocking prompts (the affirmative
	// option) even when a controlling terminal is attached, restoring the fully
	// unattended behavior. Only runOneShot reads it; the default passthrough
	// ignores it (the harness TUI asks the human directly).
	AutoAccept  bool
	HarnessName string
	HarnessArgs []string
}

// parseHarnessWrapperArgs splits the args after the "harness-wrapper"
// subcommand. The "--" separator is required: everything before it is
// parsed as wrapper flags + the harness name, everything after it is
// passed verbatim to the harness.
//
//	[wrapper-flags...] <harness-name> -- <harness args...>
func parseHarnessWrapperArgs(in []string) (harnessWrapperArgs, error) {
	sep := -1
	for i, a := range in {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep == -1 {
		return harnessWrapperArgs{}, fmt.Errorf("harness-wrapper: missing -- separator before harness args")
	}
	pre := in[:sep]
	harnessArgs := in[sep+1:]

	var args harnessWrapperArgs
	fs := harnessWrapperFlagSet(&args)
	if err := fs.Parse(pre); err != nil {
		return harnessWrapperArgs{}, fmt.Errorf("harness-wrapper: %w", err)
	}
	if args.TraceFile != "" && args.TraceStderr {
		return harnessWrapperArgs{}, fmt.Errorf("harness-wrapper: --trace-file and --trace-stderr are mutually exclusive")
	}
	if args.TmuxSession != "" && args.TmuxChild != "" {
		return harnessWrapperArgs{}, fmt.Errorf("harness-wrapper: --tmux-session and --tmux-child are mutually exclusive")
	}
	if fs.NArg() == 0 {
		return harnessWrapperArgs{}, fmt.Errorf("harness-wrapper: missing harness name before --")
	}
	if fs.NArg() != 1 {
		return harnessWrapperArgs{}, fmt.Errorf(
			"harness-wrapper: expected exactly one harness name before --, got %d args (%v); wrapper flags like --trace-file must come BEFORE the harness name",
			fs.NArg(), fs.Args())
	}
	args.HarnessName = fs.Arg(0)
	args.HarnessArgs = harnessArgs
	return args, nil
}

// harnessWrapperFlagSet registers the wrapper's CLI flags onto a fresh
// FlagSet, binding them into a. It is the single definition of the flag
// surface, so the contract test can enumerate it (see contract_test.go).
func harnessWrapperFlagSet(a *harnessWrapperArgs) *flag.FlagSet {
	fs := flag.NewFlagSet("harness-wrapper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.TraceFile, "trace-file", "", "path to write trace events as NDJSON (default: trace events are dropped)")
	fs.BoolVar(&a.TraceStderr, "trace-stderr", false, "write trace events as NDJSON to stderr (mutually exclusive with --trace-file)")
	fs.StringVar(&a.Effort, "effort", "", "reasoning effort for supported harnesses (low, medium, high, xhigh, max)")
	fs.StringVar(&a.Model, "model", "", "model id for supported harnesses (claude --model, codex -c model)")
	fs.StringVar(&a.TmuxSession, "tmux-session", "", "spawn the run inside a detached tmux session named hw-<value> and exit immediately")
	fs.StringVar(&a.TmuxChild, "tmux-child", "", "internal: in-pane re-exec marker; do not set manually")
	fs.BoolVar(&a.AutoAccept, "auto-accept", false, "run: auto-answer blocking prompts (affirmative) even with a terminal attached, instead of asking the human")
	return fs
}
