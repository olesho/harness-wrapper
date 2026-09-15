package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// defaultLoginTimeout is how long contain-login waits for the person to sign
// in: codex's one-time code expires after 15 minutes.
const defaultLoginTimeout = 15 * time.Minute

// runContainLogin signs a harness in from inside the containment boundary, for
// a person at this terminal:
//
//	harness-wrapper contain-login [--status] [--timeout DUR] [--verbose] [--contain-* flags] [--trace-file PATH | --trace-stderr] <harness> [--]
//
// It runs the harness's own login command contained, keeping the login in
// --contain-state-dir, which later contained sessions name to run as it. It
// prints the sign-in page (and codex's one-time code), reads the code claude's
// page shows from stdin, and finishes with the harness's own status command
// run in the same directory. --status runs only that command. --contain
// defaults to landlock. Exit 0 when the harness is signed in there, 1 when it
// is not or the login was refused, 2 on a usage error, 130 when interrupted.
func runContainLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, rest, err := parseContainLoginArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login:", err)
		return 2
	}
	parsed, err := parseHarnessWrapperArgs(withContainKind(rest))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if err := loginOnlyFlags(parsed); err != nil {
		_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login:", err)
		return 2
	}
	if parsed.Contain.StateDir == "" {
		_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login: --contain-state-dir DIR is required: the login is kept there, for contained sessions that name the same directory")
		return 2
	}
	binPath, err := resolveHarness(parsed.HarnessName)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	emitter, closeTrace, err := openTraceEmitter(parsed)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login:", err)
		return 2
	}
	defer closeTrace()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	cfg := wrapper.LoginConfig{
		Harness:     parsed.HarnessName,
		BinaryPath:  binPath,
		StateDir:    parsed.Contain.StateDir,
		Containment: parsed.Contain.request(),
		Env:         cleanedEnv(),
		Trace:       emitter,
	}
	if opts.status {
		res, err := wrapper.LoginStatus(ctx, cfg)
		if err != nil {
			return loginFailure(stderr, err)
		}
		return reportLogin(stdout, cfg, res, opts.verbose)
	}

	l, err := wrapper.StartLogin(ctx, cfg)
	if err != nil {
		return loginFailure(stderr, err)
	}
	p, err := l.Prompt(ctx)
	if err != nil {
		_ = l.Stop(context.Background())
		if ctx.Err() != nil {
			_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login: stopped before the sign-in page appeared:", context.Cause(ctx))
			return 130
		}
		_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login:", err)
		return 1
	}
	printLoginPrompt(stdout, p)
	if p.WantsCode {
		go func() {
			sc := bufio.NewScanner(stdin)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" {
					continue
				}
				if err := l.SubmitCode(line); err != nil {
					_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login:", err)
				}
			}
		}()
	}
	res, err := l.Wait()
	if err != nil {
		return loginFailure(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout)
	code := reportLogin(stdout, cfg, res, opts.verbose)
	if code != 0 && ctx.Err() != nil {
		_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login: stopped:", context.Cause(ctx))
		return 130
	}
	return code
}

type containLoginOptions struct {
	status  bool
	verbose bool
	timeout time.Duration
}

// parseContainLoginArgs takes contain-login's own flags out of the arguments
// before "--" and returns the rest, "--" included (added when absent: the
// login's arguments come from the harness profile, never the command line).
func parseContainLoginArgs(args []string) (containLoginOptions, []string, error) {
	opts := containLoginOptions{timeout: defaultLoginTimeout}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			if len(args[i+1:]) > 0 {
				return opts, nil, fmt.Errorf("takes no harness arguments: the login and status commands come from the harness profile")
			}
			break
		}
		switch {
		case a == "--status":
			opts.status = true
		case a == "--verbose" || a == "-v":
			opts.verbose = true
		case a == "--timeout" || strings.HasPrefix(a, "--timeout="):
			v, ok := strings.CutPrefix(a, "--timeout=")
			if !ok {
				if i+1 >= len(args) {
					return opts, nil, fmt.Errorf("--timeout needs a duration")
				}
				i++
				v = args[i]
			}
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return opts, nil, fmt.Errorf("--timeout %q is not a positive duration", v)
			}
			opts.timeout = d
		default:
			rest = append(rest, a)
		}
	}
	return opts, append(rest, "--"), nil
}

// withContainKind adds --contain landlock before "--" when no --contain was
// given, so --contain-* refinements need not repeat it.
func withContainKind(args []string) []string {
	for _, a := range args {
		if a == "--" {
			break
		}
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && name == "contain" {
			return args
		}
	}
	return append([]string{"--contain", wrapper.ContainmentLandlock}, args...)
}

// loginOnlyFlags rejects the wrapper flags a login has no use for.
func loginOnlyFlags(p harnessWrapperArgs) error {
	var bad []string
	for flag, set := range map[string]bool{
		"--effort": p.Effort != "", "--model": p.Model != "", "--permission-mode": p.PermissionMode != "",
		"--tmux-session": p.TmuxSession != "", "--tmux-child": p.TmuxChild != "",
		"--auto-accept": p.AutoAccept, "--sandbox-defaults": p.SandboxDefaults,
	} {
		if set {
			bad = append(bad, flag)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("takes only --contain-*, --trace-*, --status, --timeout and --verbose; not %s", strings.Join(bad, ", "))
	}
	return nil
}

func printLoginPrompt(w io.Writer, p wrapper.LoginPrompt) {
	_, _ = fmt.Fprintln(w, "Open this page in a browser, on any device, and sign in:")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "  "+p.URL)
	_, _ = fmt.Fprintln(w)
	if p.UserCode != "" {
		_, _ = fmt.Fprintln(w, "When the page asks for a one-time code, enter:")
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "  "+p.UserCode)
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "Waiting for the sign-in to finish...")
	}
	if p.WantsCode {
		_, _ = fmt.Fprint(w, "Then paste the code the page shows here and press Enter: ")
	}
}

// reportLogin prints how the login ended and returns the exit code.
func reportLogin(w io.Writer, cfg wrapper.LoginConfig, res wrapper.LoginResult, verbose bool) int {
	if verbose && res.Output != "" {
		_, _ = fmt.Fprintln(w, "--- login output")
		_, _ = fmt.Fprintln(w, strings.TrimSpace(res.Output))
		_, _ = fmt.Fprintln(w, "---")
	}
	profile := ""
	if res.Containment != nil {
		profile = res.Containment.Profile
	}
	dir := cfg.StateDir
	if res.Containment != nil && res.Containment.State.StateDir != "" {
		dir = res.Containment.State.StateDir
	}
	if !res.LoggedIn {
		_, _ = fmt.Fprintf(w, "Not signed in: %s's status command found no login in %s.\n", profile, dir)
		if res.Output != "" && !verbose {
			_, _ = fmt.Fprintln(w, "login output, last lines:", lastLoginLines(res.Output, 2))
		}
		_, _ = fmt.Fprintln(w, "status:", indentLogin(res.Status))
		return 1
	}
	_, _ = fmt.Fprintf(w, "Signed in: %s's status command found a login in %s.\n", profile, dir)
	_, _ = fmt.Fprintln(w, "status:", indentLogin(res.Status))
	_, _ = fmt.Fprintf(w, "Contained sessions that pass --contain-state-dir %s run as this login.\n", dir)
	if _, _, err := contain.ProfileID(cfg.Harness); err != nil {
		var re *contain.RefusalError
		if errors.As(err, &re) {
			err = re.Err
		}
		_, _ = fmt.Fprintf(w, "note: %v. Until then, contained sessions of it are refused.\n", err)
	}
	return 0
}

// loginFailure reports a login that could not run: a refusal is exit 1, a bad
// configuration exit 2.
func loginFailure(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintln(stderr, "harness-wrapper contain-login:", err)
	switch {
	case errors.Is(err, wrapper.ErrContainmentRefused), errors.Is(err, wrapper.ErrContainmentUnsupported):
		return 1
	case errors.Is(err, wrapper.ErrInvalidConfig):
		return 2
	default:
		return 1
	}
}

// lastLoginLines returns the last n non-blank lines of a login's output, each
// cut to its last 160 characters: the lines before them hold the sign-in page,
// already shown.
func lastLoginLines(s string, n int) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > 160 {
				line = "…" + string(r[len(r)-160:])
			}
			keep = append(keep, line)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return "\n  " + strings.Join(keep, "\n  ")
}

func indentLogin(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no output)"
	}
	if !strings.Contains(s, "\n") {
		return s
	}
	return "\n  " + strings.ReplaceAll(s, "\n", "\n  ")
}
