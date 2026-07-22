package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/olesho/harness-wrapper/pkg/wrapper"
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
	AutoAccept bool
	// SandboxDefaults opts into meta-harness-parity permission injection for
	// claude: run/structured-run append --dangerously-skip-permissions to the
	// harness args and IS_SANDBOX=1 to the harness env (see
	// applySandboxDefaults). No-op for other harnesses; the default
	// passthrough mode rejects it (a human session should make that policy
	// call in the harness itself).
	SandboxDefaults bool
	// PermissionMode is the launch-time permission rung forwarded verbatim to
	// pkg/wrapper (Config.PermissionMode), which owns BOTH the value validation
	// and the per-harness argv translation. The canonical rungs are plan,
	// manual, ask, auto, bypass; per-harness native spellings pass through too.
	// The CLI deliberately does not validate the value — a bad mode surfaces as
	// wrapper.ErrInvalidConfig from wrapper.Start, in one place, for every
	// entry point (passthrough, run, structured-run, tmux child).
	PermissionMode string
	HarnessName    string
	HarnessArgs    []string
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
	// --sandbox-defaults composes with exactly ONE permission rung: bypass (and
	// its claude-native spelling bypassPermissions). Both express "no permission
	// gate", so the pair is coherent — and it is the recipe a root container
	// needs, since only --sandbox-defaults carries the IS_SANDBOX=1 env half
	// that permits root and suppresses claude's acceptance screen. Any other
	// rung asks for a RESTRICTION while --sandbox-defaults asks for none, so the
	// combination is contradictory and rejected rather than silently resolved.
	//
	// Like its --trace-file/--tmux-session neighbours the check is deliberately
	// harness-INDEPENDENT: it must never depend on a harness binary being on
	// PATH, so it sits before resolveHarness and before any tmux session can be
	// spawned. That it also fires for codex — where --sandbox-defaults is a
	// documented no-op — is the accepted cost of that determinism.
	//
	// This is exactly why wrapper.IsBypassPermissionMode excludes codex's
	// "danger-full-access": that value never reaches the claude-only
	// applySandboxDefaults compose path, so accepting it here would wave through
	// a pairing whose composition semantics do not exist.
	if args.SandboxDefaults && args.PermissionMode != "" && !wrapper.IsBypassPermissionMode(args.PermissionMode) {
		return harnessWrapperArgs{}, fmt.Errorf(
			"harness-wrapper: --sandbox-defaults is incompatible with --permission-mode %s (only --permission-mode bypass composes with it)",
			args.PermissionMode,
		)
	}
	if fs.NArg() == 0 {
		return harnessWrapperArgs{}, fmt.Errorf("harness-wrapper: missing harness name before --")
	}
	if fs.NArg() != 1 {
		return harnessWrapperArgs{}, fmt.Errorf(
			"harness-wrapper: expected exactly one harness name before --, got %d args (%v); wrapper flags like --trace-file must come BEFORE the harness name",
			fs.NArg(), fs.Args(),
		)
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
	fs.StringVar(&a.PermissionMode, "permission-mode", "", "launch-time permission rung for claude/codex: plan, manual, ask, auto, bypass (per-harness native spellings also pass through); bypass does NOT set IS_SANDBOX=1 (acceptance screen appears, root disallowed); combine with --sandbox-defaults for that, or --auto-accept to answer the screen in an interactive run; restrictive rungs bind fully only with a human at the TUI")
	fs.BoolVar(&a.SandboxDefaults, "sandbox-defaults", false, "run/structured-run: claude only — DANGEROUS: inject --dangerously-skip-permissions into harness args and set IS_SANDBOX=1 in the harness env (meta-harness parity; IS_SANDBOX=1 also suppresses the bypass-permissions acceptance screen and allows root); no-op for other harnesses; rejected by the default passthrough mode")
	return fs
}
