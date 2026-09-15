package wrapper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

// LoginConfig configures a human-led sign-in; see StartLogin.
type LoginConfig struct {
	// Harness names the harness to sign in: "claude" (or "claude-code") or
	// "codex". Required.
	Harness string

	// BinaryPath is the harness executable, as in Config.BinaryPath.
	// Required.
	BinaryPath string

	// StateDir keeps the login. Contained sessions that name the same
	// directory as Containment.StateDir use it. Created with mode 0700 when
	// missing. Required.
	StateDir string

	// Containment is the rest of the policy the login runs under, such as
	// RestrictTCP with ConnectTCP [443], which is enough for codex's sign-in
	// (claude's binds a localhost listener, so its login is refused under
	// RestrictTCP). nil, or an empty Kind, means landlock. Its StateDir, when
	// set, must name StateDir.
	Containment *Containment

	// Env is the environment the harness starts from, as in Config.Env,
	// before containment filters it. The harness's own credential variables
	// are never passed to a login or to its status check.
	Env []string

	// Output, when non-nil, receives the login command's raw terminal output.
	// It is written from the output loop, so a slow writer slows the harness.
	Output io.Writer

	// Trace receives the wrapper's diagnostic events for the login and status
	// commands.
	Trace trace.Emitter
}

// LoginPrompt is what the person signing in needs from the harness.
type LoginPrompt struct {
	// URL is the sign-in page. Open it in a browser on any device.
	URL string

	// UserCode is the one-time code to enter on that page (codex's device
	// login). Empty when the page needs none.
	UserCode string

	// WantsCode reports that the harness is waiting for the code the page
	// shows once the person has signed in (claude). Pass it to SubmitCode.
	WantsCode bool
}

// LoginResult reports how a sign-in ended.
type LoginResult struct {
	// LoggedIn reports whether the harness's own status command, run
	// contained in the same StateDir after the login command ended, found a
	// login there.
	LoggedIn bool

	// Status is the status command's output, with escape sequences removed.
	Status string

	// Output is the login command's output, with escape sequences removed.
	// It is empty for LoginStatus.
	Output string

	// Result is the login command's own outcome, or the status command's for
	// LoginStatus.
	Result Result

	// Containment is the policy that command ran under.
	Containment *containment.Applied
}

// Login is a human-led sign-in in progress; see StartLogin.
type Login struct {
	cfg  LoginConfig
	flow *contain.LoginFlow
	sess *Session
	out  *loginOutput

	waitOnce sync.Once
	result   LoginResult
	err      error
}

const (
	// loginCols is wide enough that no harness wraps a sign-in URL across
	// lines.
	loginCols, loginRows = 1024, 40

	// loginStatusTimeout bounds the status command.
	loginStatusTimeout = time.Minute

	// loginLinger is how long a login command may keep running after it has
	// reported success before it gets one Enter, and then a Stop.
	loginLinger = 3 * time.Second

	// codeEnterDelay separates a submitted code from its Enter, so a harness
	// reading raw keystrokes takes the Enter as a key, not as pasted text.
	codeEnterDelay = 150 * time.Millisecond
)

// StartLogin starts the harness's own login command inside a contained
// launch whose login is kept in cfg.StateDir, for a person to complete:
//
//   - Prompt returns the sign-in page to open, and codex's one-time code.
//   - SubmitCode passes on the code claude's page shows after signing in.
//   - Wait reports whether the harness is signed in once the command has
//     ended. It runs the harness's own status command in the same StateDir to
//     decide.
//
// A login runs only the login and status commands its harness profile pins,
// so it works before the profile is activated for sessions. Neither command
// receives the harness's credential variables, and neither runs tools, so
// codex's bypass-rung requirement does not apply. Linux only, like every
// contained launch.
func StartLogin(ctx context.Context, cfg LoginConfig) (*Login, error) {
	cfg, flow, err := prepareLogin(cfg)
	if err != nil {
		return nil, err
	}
	out := newLoginOutput(flow, cfg.Output)
	sess, err := startLoginCommand(ctx, cfg, flow.Args, out)
	if err != nil {
		return nil, err
	}
	l := &Login{cfg: cfg, flow: flow, sess: sess, out: out}
	go l.endAfterSuccess()
	return l, nil
}

// LoginStatus runs the harness's own status command, contained, in
// cfg.StateDir, and reports whether the harness is signed in there. It starts
// no login, so cfg.Output is unused.
func LoginStatus(ctx context.Context, cfg LoginConfig) (LoginResult, error) {
	cfg, flow, err := prepareLogin(cfg)
	if err != nil {
		return LoginResult{}, err
	}
	return runLoginStatus(ctx, cfg, flow)
}

// Prompt waits until the login command has printed everything the person
// needs: the sign-in page, and the one-time code or the code prompt its flow
// has. It fails, quoting the command's output, if the command ends first.
func (l *Login) Prompt(ctx context.Context) (LoginPrompt, error) {
	select {
	case <-l.out.ready:
		return l.out.prompt(), nil
	case <-l.sess.doneCh:
		select {
		case <-l.out.ready:
			return l.out.prompt(), nil
		default:
		}
		return LoginPrompt{}, fmt.Errorf("wrapper: the %s login ended before it printed a sign-in page: %s",
			l.flow.Profile, lastLines(l.out.text(), 12))
	case <-ctx.Done():
		return LoginPrompt{}, ctx.Err()
	}
}

// SubmitCode types the code the sign-in page showed at the harness's prompt
// and presses Enter. Only a harness whose prompt wants one takes it (see
// LoginPrompt.WantsCode). A harness that rejects the code may prompt again.
func (l *Login) SubmitCode(code string) error {
	if l.flow.CodePrompt == "" {
		return fmt.Errorf("%w: the %s login takes no code; finish signing in on the page", ErrInvalidConfig, l.flow.Profile)
	}
	code = strings.TrimSpace(code)
	if code == "" || strings.ContainsFunc(code, unicode.IsControl) {
		return fmt.Errorf("%w: a sign-in code is one line of printable text", ErrInvalidConfig)
	}
	if _, err := l.sess.WriteStdin([]byte(code)); err != nil {
		return err
	}
	time.Sleep(codeEnterDelay)
	_, err := l.sess.WriteStdin([]byte{'\r'})
	return err
}

// Stop ends the login command, as Session.Stop does. Wait still reports what
// the StateDir holds.
func (l *Login) Stop(ctx context.Context) error { return l.sess.Stop(ctx) }

// Wait waits for the login command to end, whether signed in, failed or
// stopped. Then it runs the harness's status command in the same StateDir and
// reports the result. Every call returns the same result.
func (l *Login) Wait() (LoginResult, error) {
	l.waitOnce.Do(func() {
		res, err := l.sess.Wait()
		l.result = LoginResult{Output: l.out.text(), Result: res, Containment: l.sess.Containment()}
		if err != nil {
			l.err = err
			return
		}
		// The status runs even when the caller's context has ended the login:
		// what the StateDir holds is the answer either way.
		st, err := runLoginStatus(context.Background(), l.cfg, l.flow)
		l.result.LoggedIn, l.result.Status = st.LoggedIn, st.Status
		l.err = err
	})
	return l.result, l.err
}

// endAfterSuccess ends a login command that keeps running once it has reported
// success: one Enter, for a "press Enter" pause, and then a Stop.
func (l *Login) endAfterSuccess() {
	select {
	case <-l.out.success:
	case <-l.sess.doneCh:
		return
	}
	steps := []func(){
		func() { _, _ = l.sess.WriteStdin([]byte{'\r'}) },
		func() { _ = l.sess.Stop(context.Background()) },
	}
	for _, step := range steps {
		select {
		case <-l.sess.doneCh:
			return
		case <-time.After(loginLinger):
			step()
		}
	}
}

// prepareLogin validates cfg, resolves the harness's login flow and completes
// the containment request with the StateDir, which it creates.
func prepareLogin(cfg LoginConfig) (LoginConfig, *contain.LoginFlow, error) {
	switch {
	case cfg.Harness == "":
		return cfg, nil, fmt.Errorf("%w: Harness is required", ErrInvalidConfig)
	case cfg.BinaryPath == "":
		return cfg, nil, fmt.Errorf("%w: BinaryPath is required", ErrInvalidConfig)
	case cfg.StateDir == "":
		return cfg, nil, fmt.Errorf("%w: StateDir is required: it is where the login is kept", ErrInvalidConfig)
	case runtime.GOOS != "linux":
		return cfg, nil, ErrContainmentUnsupported
	}
	flow, err := contain.LoginFlowFor(cfg.Harness)
	if err != nil {
		return cfg, nil, fmt.Errorf("%w: %w", ErrContainmentRefused, err)
	}
	dir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return cfg, nil, fmt.Errorf("%w: StateDir: %v", ErrInvalidConfig, err)
	}
	var req Containment
	if cfg.Containment != nil {
		req = *cfg.Containment
	}
	if req.Kind == "" {
		req.Kind = ContainmentLandlock
	}
	if req.StateDir != "" {
		if other, err := filepath.Abs(req.StateDir); err != nil || other != dir {
			return cfg, nil, fmt.Errorf("%w: Containment.StateDir %s is not StateDir %s", ErrInvalidConfig, req.StateDir, dir)
		}
	}
	req.StateDir = dir
	if _, err := containment.Normalize(&req); err != nil {
		return cfg, nil, fmt.Errorf("%w: %w", ErrContainmentRefused, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return cfg, nil, fmt.Errorf("%w: StateDir: %v", ErrInvalidConfig, err)
	}
	cfg.StateDir = dir
	cfg.Containment = &req
	return cfg, flow, nil
}

// startLoginCommand starts one of the flow's commands in login mode. It runs
// in the StateDir itself, which the launch grants anyway. Its classifier never
// fires, because a person may take minutes to sign in and no quiet period
// means the login is over.
func startLoginCommand(ctx context.Context, cfg LoginConfig, args []string, stdout io.Writer) (*Session, error) {
	s, err := Start(contain.WithLaunchOptions(ctx, contain.LaunchOptions{Login: true}), Config{
		BinaryPath:     cfg.BinaryPath,
		Args:           args,
		WorkingDir:     cfg.StateDir,
		Env:            cfg.Env,
		Stdout:         stdout,
		Trace:          cfg.Trace,
		Harness:        cfg.Harness,
		Classifier:     ClassifierFunc(func(ClassifierInput) Classification { return Classification{} }),
		StaleThreshold: -1,
		Containment:    cfg.Containment,
	})
	if err != nil {
		return nil, err
	}
	_ = s.Resize(loginCols, loginRows)
	return s, nil
}

// runLoginStatus runs the flow's status command and matches its output.
func runLoginStatus(ctx context.Context, cfg LoginConfig, flow *contain.LoginFlow) (LoginResult, error) {
	ctx, cancel := context.WithTimeout(ctx, loginStatusTimeout)
	defer cancel()
	out := newLoginOutput(nil, nil)
	s, err := startLoginCommand(ctx, cfg, flow.Status, out)
	if err != nil {
		return LoginResult{}, err
	}
	res, err := s.Wait()
	st := LoginResult{Status: strings.TrimSpace(out.text()), Result: res, Containment: s.Containment()}
	if err != nil {
		return st, err
	}
	if res.Status == StatusInterrupted && ctx.Err() != nil {
		return st, fmt.Errorf("wrapper: the %s status command did not finish: %w", flow.Profile, ctx.Err())
	}
	st.LoggedIn = flow.LoggedIn.MatchString(st.Status)
	return st, nil
}

// loginOutputCap bounds the output a login keeps. The prompt comes first, in
// the first few hundred bytes.
const loginOutputCap = 1 << 20

// loginOutput keeps a login or status command's output and, for a login, finds
// in it what the person signing in needs. Every Write rescans the kept output,
// so an escape sequence or a line split across writes is read whole.
type loginOutput struct {
	flow *contain.LoginFlow // nil: keep the output only
	tee  io.Writer

	mu      sync.Mutex
	raw     []byte
	found   LoginPrompt
	ready   chan struct{} // closed once found is complete
	success chan struct{} // closed once the flow's success text appeared
	isReady bool
	isDone  bool
}

func newLoginOutput(flow *contain.LoginFlow, tee io.Writer) *loginOutput {
	return &loginOutput{flow: flow, tee: tee, ready: make(chan struct{}), success: make(chan struct{})}
}

func (o *loginOutput) Write(p []byte) (int, error) {
	if o.tee != nil {
		_, _ = o.tee.Write(p)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if room := loginOutputCap - len(o.raw); room > 0 {
		o.raw = append(o.raw, p[:min(len(p), room)]...)
	}
	if o.flow != nil && (!o.isReady || !o.isDone) {
		o.scanLocked()
	}
	return len(p), nil
}

func (o *loginOutput) scanLocked() {
	text := terminalText(string(o.raw))
	if !o.isReady {
		if p, ok := findLoginPrompt(o.flow, text); ok {
			o.found, o.isReady = p, true
			close(o.ready)
		}
	}
	if !o.isDone && o.flow.Success != "" && strings.Contains(text, o.flow.Success) {
		o.isDone = true
		close(o.success)
	}
}

func (o *loginOutput) prompt() LoginPrompt {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.found
}

func (o *loginOutput) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return terminalText(string(o.raw))
}

// findLoginPrompt reads the sign-in page, the one-time code and the code
// prompt from a login's output. The page and the code come only from complete
// lines, so neither is read cut short by a write that ended mid-line; the
// prompt waits at the end of an unterminated line.
func findLoginPrompt(flow *contain.LoginFlow, text string) (LoginPrompt, bool) {
	lines := text[:strings.LastIndexByte(text, '\n')+1]
	var p LoginPrompt
	if p.URL = firstMatch(flow.URL, lines); p.URL == "" {
		return p, false
	}
	if flow.UserCode != nil {
		if p.UserCode = firstMatch(flow.UserCode, lines); p.UserCode == "" {
			return p, false
		}
	}
	if flow.CodePrompt != "" {
		if !strings.Contains(text, flow.CodePrompt) {
			return p, false
		}
		p.WantsCode = true
	}
	return p, true
}

// firstMatch returns re's first match in s: its first group when it has one.
func firstMatch(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	switch {
	case m == nil:
		return ""
	case len(m) > 1:
		return m[1]
	default:
		return m[0]
	}
}

// terminalText renders terminal output as plain text. It removes escape
// sequences, keeps an OSC 8 hyperlink's target as a word of its own (a
// harness may print a URL only as a link, or as visible text a narrow terminal
// wraps), renders forward cursor movement within a line as spaces (Bun and
// Ink indent with it), and turns every line ending into "\n".
func terminalText(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	col := 0 // characters since the last line ending
	pad := func(n int) {
		n = min(n, 512)
		for range n {
			b.WriteByte(' ')
		}
		col += max(n, 0)
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == 0x1b && i+1 < len(raw) && raw[i+1] == ']':
			// OSC, up to its terminator. An unterminated one is dropped whole:
			// its target may be cut short.
			end, next := oscEnd(raw, i+2)
			if rest, ok := strings.CutPrefix(raw[i+2:end], "8;"); ok && next > end {
				if _, uri, ok := strings.Cut(rest, ";"); ok && uri != "" {
					b.WriteString(" " + uri + " ")
					col += len(uri) + 2
				}
			}
			i = next - 1
		case c == 0x1b && i+1 < len(raw) && raw[i+1] == '[':
			// CSI: parameters and intermediates, then a final byte.
			j := i + 2
			for j < len(raw) && (raw[j] < 0x40 || raw[j] > 0x7e) {
				j++
			}
			if j < len(raw) {
				n, err := strconv.Atoi(raw[i+2 : j])
				if err != nil || n < 1 {
					n = 1
				}
				switch raw[j] {
				case 'C': // cursor forward
					pad(n)
				case 'G': // cursor to column n
					pad(n - 1 - col)
				}
			}
			i = j
		case c == 0x1b:
			// Any other escape: intermediates, then a final byte.
			j := i + 1
			for j < len(raw) && raw[j] >= 0x20 && raw[j] <= 0x2f {
				j++
			}
			i = j
		case c == '\r':
			for i+1 < len(raw) && raw[i+1] == '\r' {
				i++
			}
			if i+1 < len(raw) && raw[i+1] == '\n' {
				continue
			}
			b.WriteByte('\n')
			col = 0
		case c == '\n':
			b.WriteByte(c)
			col = 0
		case c == '\t' || (c >= 0x20 && c != 0x7f):
			b.WriteByte(c)
			if c&0xc0 != 0x80 {
				col++
			}
		}
	}
	return b.String()
}

// oscEnd finds the end of an operating system command whose body starts at
// from: the terminator's index, and the index after it. An unterminated one
// runs to the end of s, and both are len(s).
func oscEnd(s string, from int) (end, next int) {
	for j := from; j < len(s); j++ {
		switch {
		case s[j] == 0x07:
			return j, j + 1
		case s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\':
			return j, j + 2
		}
	}
	return len(s), len(s)
}

// lastLines returns the last n non-blank lines of s, joined by " | ".
func lastLines(s string, n int) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			keep = append(keep, line)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	if len(keep) == 0 {
		return "(no output)"
	}
	return strings.Join(keep, " | ")
}
