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
	// TmuxSession requests the wrapper spawn the run inside a detached
	// tmux session named hw-<TmuxSession> and exit immediately after
	// `tmux new-session -d` succeeds. The wrapper will re-exec itself
	// inside the pane with --tmux-child set to the same name.
	TmuxSession string
	// TmuxChild is the in-pane re-exec marker. Hidden from --help; set
	// only by the parent invocation. When non-empty the wrapper behaves
	// like a normal in-process run, but trace events carry the session
	// name so consumers can correlate.
	TmuxChild   string
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

	fs := flag.NewFlagSet("harness-wrapper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var traceFile string
	var traceStderr bool
	var effort string
	var tmuxSession string
	var tmuxChild string
	fs.StringVar(&traceFile, "trace-file", "", "path to write trace events as NDJSON (default: trace events are dropped)")
	fs.BoolVar(&traceStderr, "trace-stderr", false, "write trace events as NDJSON to stderr (mutually exclusive with --trace-file)")
	fs.StringVar(&effort, "effort", "", "reasoning effort for supported harnesses (low, medium, high, xhigh, max)")
	fs.StringVar(&tmuxSession, "tmux-session", "", "spawn the run inside a detached tmux session named hw-<value> and exit immediately")
	fs.StringVar(&tmuxChild, "tmux-child", "", "internal: in-pane re-exec marker; do not set manually")
	if err := fs.Parse(pre); err != nil {
		return harnessWrapperArgs{}, fmt.Errorf("harness-wrapper: %w", err)
	}
	if traceFile != "" && traceStderr {
		return harnessWrapperArgs{}, fmt.Errorf("harness-wrapper: --trace-file and --trace-stderr are mutually exclusive")
	}
	if tmuxSession != "" && tmuxChild != "" {
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
	return harnessWrapperArgs{
		TraceFile:   traceFile,
		TraceStderr: traceStderr,
		Effort:      effort,
		TmuxSession: tmuxSession,
		TmuxChild:   tmuxChild,
		HarnessName: fs.Arg(0),
		HarnessArgs: harnessArgs,
	}, nil
}
