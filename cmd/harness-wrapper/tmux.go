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

// envTraceFile is set on every tmux session via `tmux set-environment`
// so that `attach`/`status`/`kill` can recover the trace-file path from
// the session itself without a separate registry file.
const envTraceFile = "HW_TRACE_FILE"

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
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
		return 1
	}
	if !validSessionName(args.TmuxSession) {
		fmt.Fprintf(os.Stderr, "harness-wrapper: invalid --tmux-session value %q (allowed: [A-Za-z0-9_-], 1-64 chars)\n", args.TmuxSession)
		return 2
	}
	tmuxName := tmuxSessionPrefix + args.TmuxSession

	tracePath, err := resolveTracePath(args.TraceFile, args.TmuxSession)
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
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

	// Re-exec command: same binary, but in --tmux-child mode with the
	// resolved trace-file path. tmux runs this as the pane command.
	reexec := []string{
		self,
		"--tmux-child", args.TmuxSession,
		"--trace-file", tracePath,
	}
	if args.Effort != "" {
		reexec = append(reexec, "--effort", args.Effort)
	}
	if args.Model != "" {
		reexec = append(reexec, "--model", args.Model)
	}
	if args.MaxTokens > 0 {
		reexec = append(reexec, "--max-tokens", fmt.Sprintf("%d", args.MaxTokens))
	}
	reexec = append(reexec, args.HarnessName, "--")
	reexec = append(reexec, args.HarnessArgs...)

	tmuxArgs := []string{"new-session", "-d", "-s", tmuxName}
	tmuxArgs = append(tmuxArgs, reexec...)

	cmd := exec.Command("tmux", tmuxArgs...) //nolint:gosec // G204: explicit tmux invocation
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: tmux new-session failed: %v\n", err)
		return 1
	}

	// Best-effort: stash the trace path in the tmux session env so
	// subsequent attach/status/kill calls can recover it.
	_ = exec.Command("tmux", "set-environment", "-t", tmuxName, envTraceFile, tracePath).Run() //nolint:gosec

	fmt.Printf("session: %s\n", args.TmuxSession)
	fmt.Printf("tmux:    %s\n", tmuxName)
	fmt.Printf("trace:   %s\n", tracePath)
	return 0
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

// runTmuxSubcommand dispatches `attach|status|kill|list`. Returns the
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
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
		return 1
	}
	tmuxName := tmuxSessionPrefix + name

	// Replace this process with `tmux attach`. We don't want to layer an
	// extra Go process between the user's terminal and tmux; exec is the
	// right primitive here.
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
		return 1
	}
	if err := syscall.Exec(tmuxBin, []string{"tmux", "attach", "-t", tmuxName}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "harness-wrapper: exec tmux attach: %v\n", err)
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
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
		return 1
	}
	tmuxName := tmuxSessionPrefix + name
	cmd := exec.Command("tmux", "kill-session", "-t", tmuxName) //nolint:gosec
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
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
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
	cmd := exec.Command("tmux", "list-sessions", "-F", "#{session_name}") //nolint:gosec
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
		fmt.Fprintln(os.Stderr, "harness-wrapper:", err)
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
	out, err := exec.Command("tmux", "show-environment", "-t", tmuxName, envTraceFile).Output() //nolint:gosec
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
	cmd := exec.Command("tmux", "has-session", "-t", tmuxSessionPrefix+name) //nolint:gosec
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
	defer f.Close()

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
