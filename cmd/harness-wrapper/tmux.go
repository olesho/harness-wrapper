package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// tmuxSessionPrefix is prepended to every tmux session name owned by
// harness-wrapper so `harness-wrapper list` can filter cleanly without
// interfering with sessions a user created by hand.
const tmuxSessionPrefix = "hw-"

// tmuxErrPrefix labels tmux-mode errors on stderr.
const tmuxErrPrefix = "harness-wrapper:"

// envTraceFile is set on every tmux session via `tmux set-environment`
// so that `attach`/`status`/`kill` can recover the trace-file path from
// the session itself without a separate registry file.
const envTraceFile = "HW_TRACE_FILE"

// defaultTmuxSocket is the dedicated tmux server harness-wrapper sessions live
// on, selected with `tmux -L <socket>`.
//
// WHY THIS EXISTS -- do not "simplify" it back to the default socket. A tmux
// server started with -L begins with tmux's DEFAULT options, so ambient global
// state on the user's shared server cannot reach our sessions. The specific
// state that motivated it is `remain-on-exit`: when it is on, a pane whose
// process has exited is RETAINED as a dead pane (pane_dead=1, pane_pid gone),
// which keeps the whole session alive forever holding no process at all. It
// gets stuck on because other tools set it globally and restore it only from a
// cleanup path that a SIGKILLed test binary never runs (observed: loomcli's
// automode test suite, see PUPPET-346). Every harness-wrapper session on the
// box then becomes immortal and invisible to `harness-wrapper list` consumers
// as "finished".
//
// TMUX_TMPDIR was tried first and rejected: it produced "error connecting to
// ... (File name too long)" for deep directories, whereas -L keeps the socket
// under the standard short tmux-$UID dir.
//
// Trade-off, stated plainly: hw sessions no longer show up in a bare `tmux ls`.
// `harness-wrapper list/status/attach/kill` are the documented interface, and
// the raw escape hatch is `tmux -L harness-wrapper attach -t hw-<name>`.
// HW_TMUX_SOCKET overrides the socket for debugging.
const defaultTmuxSocket = "harness-wrapper"

// envTmuxSocket overrides defaultTmuxSocket. It is env-only on purpose: a flag
// would have to be forwarded through tmuxReexecArgv (see the warning on that
// function), while an env var crosses the re-exec for free.
const envTmuxSocket = "HW_TMUX_SOCKET"

// tmuxSocketName returns the tmux socket harness-wrapper operates on.
func tmuxSocketName() string {
	if v := os.Getenv(envTmuxSocket); v != "" {
		return v
	}
	return defaultTmuxSocket
}

// tmuxCmd builds a tmux invocation pinned to harness-wrapper's own socket.
// EVERY tmux call in this package must go through it; a call that misses the
// -L talks to a different server and will not see our sessions at all.
func tmuxCmd(args ...string) *exec.Cmd {
	return exec.Command("tmux", append([]string{"-L", tmuxSocketName()}, args...)...) //nolint:gosec // G204: explicit tmux invocation
}

// runTmuxSpawn implements the parent half of `harness-wrapper
// --tmux-session <name>`. It resolves the trace-file path, builds the
// re-exec command pointing at this same binary with --tmux-child set,
// runs `tmux new-session -d`, prints the session and trace path, and
// exits. The harness keeps running inside the pane until it exits or
// `harness-wrapper kill` is invoked.
//
// binPath is the resolved harness binary (passed in for parity with the
// in-process path) but tmux mode does not use it: the in-pane child
// resolves it again via resolveHarness, ensuring the harness PATH lookup
// happens in the same environment tmux's child shell will see.
func runTmuxSpawn(args harnessWrapperArgs, binPath string) int {
	_ = binPath
	if err := requireTmux(); err != nil {
		fmt.Fprintln(os.Stderr, tmuxErrPrefix, err)
		return 1
	}
	if !validSessionName(args.TmuxSession) {
		fmt.Fprintf(os.Stderr, "harness-wrapper: invalid --tmux-session value %q (allowed: [A-Za-z0-9_-], 1-64 chars)\n", args.TmuxSession)
		return 2
	}
	tmuxName := tmuxSessionPrefix + args.TmuxSession

	tracePath, err := resolveTracePath(args.TraceFile, args.TmuxSession)
	if err != nil {
		fmt.Fprintln(os.Stderr, tmuxErrPrefix, err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: mkdir trace dir: %v\n", err)
		return 1
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: locate self: %v\n", err)
		return 1
	}

	reexec := tmuxReexecArgv(args, self, tracePath)

	cmd := tmuxCmd(tmuxSpawnArgv(tmuxName, reexec)...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: tmux new-session failed: %v\n", err)
		return 1
	}

	// Belt and braces on top of the dedicated socket: force remain-on-exit off
	// for this session, so a stray config or a shared HW_TMUX_SOCKET cannot
	// retain the pane after the child exits.
	//
	// Deliberately a SEPARATE best-effort call rather than a `;`-chained
	// argument to new-session above: chaining would make a set-option failure
	// fail the whole spawn, and runTmuxSpawn reports that as exit 1 while the
	// session it just created is left running -- worse than the bug this
	// guards against. The small window this opens does not matter, because the
	// real guarantee is the child tearing its own session down when it exits
	// (see tmuxSelfTeardown). This matches the set-environment line below.
	if err := tmuxCmd(tmuxRemainOnExitOffArgv(tmuxName)...).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: tmux set remain-on-exit off failed (continuing): %v\n", err)
	}

	// Best-effort: stash the trace path in the tmux session env so
	// subsequent attach/status/kill calls can recover it.
	_ = tmuxCmd("set-environment", "-t", tmuxName, envTraceFile, tracePath).Run()

	fmt.Printf("session: %s\n", args.TmuxSession)
	fmt.Printf("tmux:    %s\n", tmuxName)
	fmt.Printf("trace:   %s\n", tracePath)
	return 0
}

// tmuxReexecArgv builds the pane command for a tmux-backed run: the same
// binary, re-entered in --tmux-child mode with the resolved trace-file path,
// followed by every execution-mode flag the parent was given and then
// `<harness> -- <harness args...>`.
//
// It is a pure function (mirroring pkg/env.buildRunnerArgv) so the forwarding
// set is unit-testable without spawning tmux. That matters: this argv is
// hand-rebuilt rather than derived from the original os.Args, so a flag that is
// added to harnessWrapperArgs but forgotten HERE is silently dropped — and for
// --permission-mode a silent drop means the pane runs an UNRESTRICTED harness
// after the user explicitly asked for a restriction. tmux_test.go freezes the
// set so that regression cannot land quietly.
func tmuxReexecArgv(a harnessWrapperArgs, self, tracePath string) []string {
	argv := []string{
		self,
		"--tmux-child", a.TmuxSession,
		"--trace-file", tracePath,
	}
	if a.Effort != "" {
		argv = append(argv, "--effort", a.Effort)
	}
	if a.Model != "" {
		argv = append(argv, "--model", a.Model)
	}
	if a.PermissionMode != "" {
		argv = append(argv, "--permission-mode", a.PermissionMode)
	}
	argv = append(argv, a.HarnessName, "--")
	return append(argv, a.HarnessArgs...)
}

// tmuxSpawnArgv builds the `new-session` argv for a detached tmux run. Pure,
// for the same reason tmuxReexecArgv is: it can be frozen by a unit test
// without a tmux server. The -L socket flag is NOT included here -- tmuxCmd
// prepends it for every invocation.
func tmuxSpawnArgv(tmuxName string, reexec []string) []string {
	argv := []string{"new-session", "-d", "-s", tmuxName}
	return append(argv, reexec...)
}

// tmuxRemainOnExitOffArgv builds the argv that clears remain-on-exit for one
// session's window. See defaultTmuxSocket for why this option is the whole
// reason PUPPET-346 existed.
func tmuxRemainOnExitOffArgv(tmuxName string) []string {
	return []string{"set-option", "-t", tmuxName, "-w", "remain-on-exit", "off"}
}

// tmuxSelfTeardown kills the tmux session a --tmux-child run is executing
// inside. It is the option-independent guarantee that a finished run leaves no
// session behind: it does not care what remain-on-exit is set to, or which
// socket ambient config came from.
//
// ORDERING IS LOAD-BEARING. kill-session destroys the pane this very process
// runs in, so the process can be signalled mid-call. The trace emitter MUST be
// flushed and closed before this is called, or the final wrapper_cli_exited
// event -- the one `harness-wrapper status` and the regression test read -- can
// be truncated away.
//
// Best-effort by design: the session may already be gone (the normal path once
// remain-on-exit is off), and a failure here must never change the exit code.
func tmuxSelfTeardown(childSession string) {
	if childSession == "" {
		return
	}
	_ = tmuxCmd("kill-session", "-t", tmuxSessionPrefix+childSession).Run()
}

// resolveTracePath picks the NDJSON trace path. If the caller passed
// --trace-file explicitly, use it verbatim (caller knows best). Otherwise
// fall back to ~/.harness-wrapper/sessions/<name>.trace.ndjson.
func resolveTracePath(explicit, sessionName string) (string, error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", fmt.Errorf("resolve trace path: %w", err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for default trace path: %w", err)
	}
	return filepath.Join(home, ".harness-wrapper", "sessions", sessionName+".trace.ndjson"), nil
}

// validSessionName rejects names with characters that would confuse tmux
// (e.g. ':', whitespace) or the filesystem.
func validSessionName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// requireTmux returns an error with a clear message when the tmux binary
// is not installed.
func requireTmux() error {
	if _, err := exec.LookPath("tmux"); err != nil {
		return fmt.Errorf("tmux not found in PATH: %w", err)
	}
	return nil
}

// runTmuxSubcommand dispatches `attach|status|kill|list|reap`. Returns the
// process exit code.
func runTmuxSubcommand(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "harness-wrapper: missing subcommand")
		return 2
	}
	sub := argv[0]
	rest := argv[1:]
	switch sub {
	case "attach":
		return runTmuxAttach(rest)
	case "status":
		return runTmuxStatus(rest)
	case "kill":
		return runTmuxKill(rest)
	case "list":
		return runTmuxList(rest)
	case "reap":
		return runTmuxReap(rest)
	default:
		fmt.Fprintf(os.Stderr, "harness-wrapper: unknown subcommand %q\n", sub)
		return 2
	}
}

func requireOneSessionArg(args []string, sub string) (string, int) {
	if len(args) != 1 {
		fmt.Fprintf(os.Stderr, "usage: harness-wrapper %s <session-name>\n", sub)
		return "", 2
	}
	if !validSessionName(args[0]) {
		fmt.Fprintf(os.Stderr, "harness-wrapper: invalid session name %q\n", args[0])
		return "", 2
	}
	return args[0], 0
}

func runTmuxAttach(args []string) int {
	name, code := requireOneSessionArg(args, "attach")
	if code != 0 {
		return code
	}
	if err := requireTmux(); err != nil {
		fmt.Fprintln(os.Stderr, tmuxErrPrefix, err)
		return 1
	}
	tmuxName := tmuxSessionPrefix + name

	// Replace this process with `tmux attach`. We don't want to layer an
	// extra Go process between the user's terminal and tmux; exec is the
	// right primitive here.
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		fmt.Fprintln(os.Stderr, tmuxErrPrefix, err)
		return 1
	}
	// -L must be carried here too: without it this attaches to the user's
	// default server, where our sessions do not exist.
	attachArgv := []string{"tmux", "-L", tmuxSocketName(), "attach", "-t", tmuxName}
	if err := syscall.Exec(tmuxBin, attachArgv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: exec tmux attach: %v\n", err)
		fmt.Fprintf(os.Stderr, "harness-wrapper: raw equivalent: tmux -L %s attach -t %s\n", tmuxSocketName(), tmuxName)
		return 1
	}
	return 0 // unreachable
}

func runTmuxKill(args []string) int {
	name, code := requireOneSessionArg(args, "kill")
	if code != 0 {
		return code
	}
	if err := requireTmux(); err != nil {
		fmt.Fprintln(os.Stderr, tmuxErrPrefix, err)
		return 1
	}
	tmuxName := tmuxSessionPrefix + name
	cmd := tmuxCmd("kill-session", "-t", tmuxName)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: tmux kill-session failed: %v\n", err)
		return 1
	}
	return 0
}

func runTmuxList(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "usage: harness-wrapper list")
		return 2
	}
	if err := requireTmux(); err != nil {
		fmt.Fprintln(os.Stderr, tmuxErrPrefix, err)
		return 1
	}
	names, err := listHWSessions()
	if err != nil {
		// tmux returns non-zero when no server is running; treat as empty.
		return 0
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Println(n)
	}
	return 0
}

// listHWSessions returns the bare session names (without the hw- prefix).
func listHWSessions() ([]string, error) {
	cmd := tmuxCmd("list-sessions", "-F", "#{session_name}")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasPrefix(line, tmuxSessionPrefix) {
			continue
		}
		names = append(names, strings.TrimPrefix(line, tmuxSessionPrefix))
	}
	return names, nil
}

func runTmuxStatus(args []string) int {
	wantJSON := false
	var positional []string
	for _, a := range args {
		switch a {
		case "--json":
			wantJSON = true
		default:
			positional = append(positional, a)
		}
	}
	name, code := requireOneSessionArg(positional, "status")
	if code != 0 {
		return code
	}

	tracePath, err := lookupTraceFile(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, tmuxErrPrefix, err)
		return 1
	}

	last, err := readLastTraceEvent(tracePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: read trace %q: %v\n", tracePath, err)
		return 1
	}
	alive := tmuxSessionExists(name)

	if wantJSON {
		out := map[string]any{
			"session": name,
			"alive":   alive,
			"trace":   tracePath,
		}
		if last != nil {
			out["last_event"] = last
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}

	fmt.Printf("session: %s\n", name)
	fmt.Printf("alive:   %v\n", alive)
	fmt.Printf("trace:   %s\n", tracePath)
	if last != nil {
		fmt.Printf("last:    %s", last["kind"])
		if at, ok := last["at"]; ok {
			fmt.Printf(" @ %v", at)
		}
		fmt.Println()
	}
	return 0
}

// lookupTraceFile recovers the trace-file path for a session by reading
// the tmux session env. If the session is gone (or never set the var),
// fall back to the default path scheme.
func lookupTraceFile(name string) (string, error) {
	tmuxName := tmuxSessionPrefix + name
	out, err := tmuxCmd("show-environment", "-t", tmuxName, envTraceFile).Output()
	if err == nil {
		line := strings.TrimSpace(string(out))
		if v, ok := strings.CutPrefix(line, envTraceFile+"="); ok && v != "" {
			return v, nil
		}
	}
	// Fall back to default location.
	return resolveTracePath("", name)
}

// tmuxSessionExists reports whether a tmux session with the hw- prefix
// is currently alive.
func tmuxSessionExists(name string) bool {
	cmd := tmuxCmd("has-session", "-t", tmuxSessionPrefix+name)
	return cmd.Run() == nil
}

// readLastTraceEvent reads the last NDJSON event from the trace file,
// or returns nil if the file is missing or empty.
func readLastTraceEvent(path string) (map[string]any, error) {
	f, err := os.Open(path) //nolint:gosec // user-supplied path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// Stream the file and remember the last non-empty line. Trace files
	// stay small in practice (kilobytes per run); a fancier tail seek is
	// premature.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lastLine string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lastLine = line
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if lastLine == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(lastLine), &out); err != nil {
		return nil, fmt.Errorf("parse last trace line: %w", err)
	}
	return out, nil
}
