package claudecodetui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/olesho/harness-wrapper/internal/harnesscore"
	"github.com/olesho/harness-wrapper/internal/procgroup"
	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/adapter/internal/proc"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// The transport: claude runs as
//
//	claude (--session-id ID | --resume ID) <open_config args> --debug-file <live dir>/debug.log
//
// on a pseudo-terminal, one process for the Session, with no -p. An input is
// typed into claude's composer, then Enter; nothing reads the screen. The
// UserPromptSubmit hook binds the prompt claude takes to the input and is its
// receipt; Stop and StopFailure end its turn; the debug log brackets the turn
// (tracker). Interrupt presses Esc, and the debug log's [onCancel] is claude
// taking it; Ctrl-U clears the composer before the next input. Close presses
// Ctrl-C until claude quits, then signals its group.

const (
	// pollEvery is how often the live hooks and the debug log are read.
	pollEvery = 20 * time.Millisecond
	// startWait bounds the wait for claude to come up.
	startWait = time.Minute
	// settle is how long Start waits once claude is up, for its composer to
	// take keys.
	settle = 500 * time.Millisecond
	// settleWait bounds Submit's wait for claude to end the turn before.
	settleWait = 10 * time.Second
	// submitWait bounds the wait for an input's receipt.
	submitWait = time.Minute
	// enterGap is the pause between an input's text and its Enter: Enter in
	// the same read as the text is taken as part of a paste.
	enterGap = 150 * time.Millisecond
	// cancelWait bounds Interrupt's wait for claude to take Esc.
	cancelWait = 10 * time.Second
	// clearGap is the pause after Ctrl-U, before the input is typed.
	clearGap = 50 * time.Millisecond
	// quitWait is how long claude may take to quit on Ctrl-C.
	quitWait = 5 * time.Second
	// outputTail is how much of the terminal's output an exit's detail
	// reads.
	outputTail = 16 << 10
	// drainGrace is how long the terminal is read after claude exits.
	drainGrace = 2 * time.Second
)

// Keys.
const (
	keyEnter = "\r"
	keyCtrlC = "\x03"
	// keyEsc interrupts claude's turn: a lone ESC, which claude tells from
	// the start of a sequence by the pause after it.
	keyEsc = "\x1b"
	// keyCtrlU clears claude's composer, where claude puts back a prompt
	// interrupted before its first token.
	keyCtrlU = "\x15"
	// pasteStart and pasteEnd bracket a paste: how a multi-line input is
	// typed, so its newlines do not submit it early.
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// killWait bounds Stop's wait for the group to end after SIGKILL. A var for
// tests.
var killWait = 5 * time.Second

type transport struct {
	id      string
	version string // claude's, as `claude --version` said it
	dir     string // the live directory
	report  func(adapter.Event)
	cmd     *exec.Cmd
	tty     *os.File
	out     *proc.TailBuffer // the terminal's output
	diag    *proc.TailBuffer // what the transport says of claude's exit

	wmu sync.Mutex // serializes keys

	mu       sync.Mutex
	k        *tracker
	wake     chan struct{} // closed, and replaced, on every change
	stopping bool
	killed   bool

	debug    tail
	procDone chan struct{} // closed once claude exited
	loopDone chan struct{} // closed once the last events were read
	exited   chan struct{} // closed once Exited was reported
}

func openFailed(reason contract.OpenFailure, format string, args ...any) error {
	return &contract.Error{Code: contract.CodeOpenFailed, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Start launches claude for a Session on a terminal and waits until it is up:
// its SessionStart hook for the Session came, and its debug log carries its
// engine's lines.
func (Profile) Start(ctx context.Context, req adapter.Start) (adapter.Transport, error) {
	cfg, err := claudecode.ParseOpenConfig(req.OpenConfig)
	if err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "%v", err)
	}
	id := req.SessionID
	if id == "" {
		id = sessionid.NewUUID()
	}
	if !sessionid.IsUUID(id) {
		return nil, openFailed(contract.OpenConfigInvalid, "claude's session ids are UUIDs, not %q", id)
	}
	if _, err := os.Stat(cfg.Binary); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "%v", err)
	}
	if req.Loaded {
		return nil, openFailed(contract.OpenConfigInvalid, "%s loads no session", Name)
	}
	env := append(adapter.HostEnv(), cfg.Env...)
	// claude's version, for Open's result and the version policy. Under
	// flexible a claude other than the pin runs, and the debug log's [engine]
	// line (awaitUp) still decides whether the profile can read it.
	v, err := claudecode.Version(ctx, cfg.Binary, env)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, openFailed(contract.OpenBinaryNotFound, "%s --version: %v", cfg.Binary, err)
	}
	if err := adapter.CheckHarnessVersion(cfg.VersionPolicy, "claude", claudecode.Pinned(), v); err != nil {
		return nil, err
	}

	dir := live.Dir(cfg.Spool, id)
	if err := live.Prepare(dir); err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "live directory: %v", err)
	}
	spool := claudecode.SessionSpool(cfg.Spool, id)
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, openFailed(contract.OpenConfigInvalid, "spool: %v", err)
	}
	debugFile := filepath.Join(dir, "debug.log")
	if err := os.Remove(debugFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, openFailed(contract.OpenConfigInvalid, "debug log: %v", err)
	}
	env = append(env, harnesscore.EnvSpool+"="+spool, harnesscore.EnvHarnessSessionID+"="+id, live.EnvDir+"="+dir,
		// The TUI checkpoints the files a turn edits (stream-json does not by
		// default). Its snapshot, written before the prompt, can put the
		// prompt in the transcript after the reply to it (claude 2.1.283),
		// and the record could not tell whose the reply is.
		"CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING=1")
	if c := req.Credential; c != nil {
		if c.Kind != claudecode.CredentialKind {
			return nil, openFailed(contract.OpenConfigInvalid, "credential kind %q, want %s", c.Kind, claudecode.CredentialKind)
		}
		tok, err := proc.ReadToken(c.File)
		if err != nil {
			return nil, openFailed(contract.OpenAuthRequired, "%v", err)
		}
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+tok)
	}
	args := append(append(claudecode.SessionArgs(req.Mode, id, cfg), cfg.Args...), "--debug-file", debugFile)

	t := &transport{
		id: id, version: v, dir: dir, report: req.Report,
		out: proc.NewTailBuffer(outputTail), diag: proc.NewTailBuffer(4 << 10),
		k: newTracker(id), wake: make(chan struct{}),
		debug:    tail{path: debugFile},
		procDone: make(chan struct{}), loopDone: make(chan struct{}), exited: make(chan struct{}),
	}
	if err := t.start(cfg.Binary, args, cfg.WorkingDir, env); err != nil {
		return nil, openFailed(contract.OpenBinaryNotFound, "start %s: %v", cfg.Binary, err)
	}
	if err := t.awaitUp(ctx); err != nil {
		t.abandon()
		return nil, err
	}
	return t, nil
}

// awaitUp waits for claude to come up, or to fail to.
func (t *transport) awaitUp(ctx context.Context) error {
	timer := time.NewTimer(startWait)
	defer timer.Stop()
	for {
		t.mu.Lock()
		up, engine, foreign, wake := t.k.up, t.k.engine, t.k.foreign, t.wake
		t.mu.Unlock()
		switch {
		case foreign != "":
			return openFailed(contract.OpenSessionInUse, "claude opened session %s, not %s: a copy, as of a session another process holds", foreign, t.id)
		case up && engine:
			select {
			case <-time.After(settle):
				return nil
			case <-t.procDone:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-wake:
		case <-t.procDone:
			screen := plain(t.out.String())
			switch {
			case strings.Contains(screen, "already in use"):
				return openFailed(contract.OpenSessionInUse, "%s", proc.LastLine(screen))
			case strings.Contains(screen, "No conversation found"):
				return openFailed(contract.OpenSessionNotFound, "%s", proc.LastLine(screen))
			}
			return openFailed(contract.OpenConfigInvalid, "claude exited as it started: %s", proc.LastLine(screen))
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if up {
				return openFailed(contract.OpenCapabilityMissing, "claude's debug log carries no [engine] line: the profile cannot read this claude")
			}
			return openFailed(contract.OpenConfigInvalid, "claude did not start its session within %s", startWait)
		}
	}
}

func (t *transport) start(bin string, args []string, dir string, env []string) error {
	cmd := exec.Command(bin, args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	tty, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: 50, Cols: 200}, cmd.SysProcAttr)
	if err != nil {
		return err
	}
	t.cmd, t.tty = cmd, tty
	go func() { _, _ = io.Copy(t.out, tty) }()
	go t.loop()
	go t.wait()
	return nil
}

func (t *transport) SessionID() string { return t.id }

// HarnessVersion is claude's version, as `claude --version` said it.
func (t *transport) HarnessVersion() string { return t.version }

// NewNative is an input's id in this profile's terms: a uuid, which the
// UserPromptSubmit hook binds claude's prompt id to.
func (t *transport) NewNative(string) string { return sessionid.NewUUID() }

// ---- reading

// loop reads the debug log and the live hooks until claude exits, and once
// more after.
func (t *transport) loop() {
	defer close(t.loopDone)
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			t.poll()
		case <-t.procDone:
			t.poll()
			return
		}
	}
}

// poll takes what claude wrote since the last poll: the debug log first, which
// claude writes before the hooks that follow the same moment.
func (t *transport) poll() {
	lines := t.debug.lines()
	evs, _ := live.Read(t.dir)
	var out []adapter.Event
	var fatal []string
	take := func(s step) {
		out = append(out, s.events...)
		if s.fatal != "" {
			fatal = append(fatal, s.fatal)
		}
	}
	now := time.Now()
	t.mu.Lock()
	for _, l := range lines {
		take(t.k.debug(parseDebugLine(l), now))
	}
	for _, ev := range evs {
		take(t.k.live(ev, now))
	}
	take(t.k.tick(now))
	if len(lines) == 0 && len(evs) == 0 && len(out) == 0 {
		t.mu.Unlock()
		return
	}
	close(t.wake)
	t.wake = make(chan struct{})
	t.mu.Unlock()
	for _, ev := range evs {
		live.Remove(t.dir, ev)
	}
	for _, ev := range out {
		t.report(ev)
	}
	for _, f := range fatal {
		_, _ = fmt.Fprintln(t.diag, f)
	}
	if len(fatal) > 0 {
		go t.Stop(context.Background(), quitWait)
	}
}

// wait turns claude's exit into the Session's: Exited, once the last hooks
// were read.
func (t *transport) wait() {
	err := t.cmd.Wait()
	close(t.procDone)
	<-t.loopDone
	go func() {
		time.Sleep(drainGrace)
		_ = t.tty.Close() // a descendant may hold the terminal open
	}()
	t.mu.Lock()
	stopping, killed := t.stopping, t.killed
	t.mu.Unlock()
	detail := t.diag.String()
	if detail == "" && !stopping {
		detail = plain(t.out.String())
	}
	t.report(adapter.Event{Kind: adapter.Exited, Exit: proc.Exit(t.cmd, err, stopping, killed, detail)})
	close(t.exited)
}

// ansi matches the terminal's control sequences.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-_]|[\x00-\x08\x0b-\x1f\x7f]`)

// plain is the terminal's output as text.
func plain(s string) string { return ansi.ReplaceAllString(s, "") }

// ---- writing

var errExited = errors.New("claude exited")

func (t *transport) keys(s string) error {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	select {
	case <-t.procDone:
		return errExited
	default:
	}
	_, err := io.WriteString(t.tty, s)
	return err
}

// waitFor waits until cond, which is called with t.mu held, holds; claude's
// exit, ctx or d ending first is an error.
func (t *transport) waitFor(ctx context.Context, d time.Duration, cond func() bool) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		t.mu.Lock()
		ok, wake := cond(), t.wake
		t.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-wake:
		case <-t.procDone:
			return errExited
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("not within %s", d)
		}
	}
}

// typed is how an input is typed: as it is when it is one line of printable
// text, else as a paste, which keeps its newlines from submitting it.
func typed(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	if !strings.ContainsFunc(text, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return text
	}
	return pasteStart + strings.ReplaceAll(text, pasteEnd, "") + pasteEnd
}

// Submit types the input and Enter once claude has ended the turn before, and
// returns once the UserPromptSubmit hook bound claude's prompt to it.
func (t *transport) Submit(ctx context.Context, s adapter.Submission) error {
	select {
	case <-t.procDone:
		return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "claude exited"}
	default:
	}
	if err := t.waitFor(ctx, settleWait, t.k.settled); err != nil {
		return fmt.Errorf("%w: claude has not ended the turn before: %v", adapter.ErrNotSubmitted, err)
	}
	t.mu.Lock()
	wipe := t.k.wipe
	t.k.wipe = 0
	t.mu.Unlock()
	if wipe > 0 {
		// After an interrupt claude may have put the prompt back in its
		// composer, where the input would be appended to it.
		if err := t.keys(strings.Repeat(keyCtrlU, wipe)); err != nil {
			if errors.Is(err, errExited) {
				return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "claude exited"}
			}
			return fmt.Errorf("%w: clearing the composer: %v", adapter.ErrNotSubmitted, err)
		}
		time.Sleep(clearGap)
	}
	if err := live.SetPending(t.dir, s.Native); err != nil {
		return fmt.Errorf("%w: %v", adapter.ErrNotSubmitted, err)
	}
	t.mu.Lock()
	t.k.begin(s.Native, strings.Count(typed(s.Text), "\n")+1)
	t.mu.Unlock()
	if err := t.keys(typed(s.Text)); err != nil {
		t.mu.Lock()
		t.k.turn = nil
		t.mu.Unlock()
		_ = live.ClearPending(t.dir)
		if errors.Is(err, errExited) {
			return &contract.Error{Code: contract.CodeExited, Certainty: contract.NotSubmitted, Message: "claude exited"}
		}
		return fmt.Errorf("typing the input: %w", err)
	}
	time.Sleep(enterGap)
	if err := t.keys(keyEnter); err != nil {
		return fmt.Errorf("pressing Enter: %w", err)
	}
	if err := t.waitFor(ctx, submitWait, func() bool { return t.k.received(s.Native) }); err != nil {
		return fmt.Errorf("claude did not take the input: %w", err)
	}
	return nil
}

// Interrupt presses Esc, and returns once claude took it: the debug log's
// [onCancel], or the turn's end. The turn's end comes as its Ended event:
// interrupted when claude stopped it, or as Stop or StopFailure say when it
// finished as Esc landed (tracker).
func (t *transport) Interrupt(ctx context.Context) error {
	t.mu.Lock()
	f := t.k.turn
	switch {
	case f == nil || !f.started:
		t.mu.Unlock()
		return errors.New("claude is on no input's turn")
	case f.ended:
		t.mu.Unlock()
		return nil
	}
	t.k.interrupting()
	t.mu.Unlock()
	return t.esc(ctx, f)
}

// InterruptTurn stops claude's own turn native, if claude is on it, as
// Interrupt stops an input's: Esc, taken once the debug log logs [onCancel]
// or the turn ends.
func (t *transport) InterruptTurn(ctx context.Context, native string) error {
	t.mu.Lock()
	f := t.k.own
	on := f != nil && f.native == native && !f.ended
	t.mu.Unlock()
	if !on {
		return nil
	}
	return t.esc(ctx, f)
}

// esc presses Esc for turn f, and waits for claude to take it.
func (t *transport) esc(ctx context.Context, f *flight) error {
	if err := t.keys(keyEsc); err != nil {
		return fmt.Errorf("pressing Esc: %w", err)
	}
	if err := t.waitFor(ctx, cancelWait, func() bool { return f.cancel || f.ended }); err != nil {
		return fmt.Errorf("claude did not take the interrupt: %w", err)
	}
	return nil
}

// Answer: at bypass claude raises no prompts.
func (t *transport) Answer(context.Context, string, contract.Choice) error {
	return contract.Errorf(contract.CodeUnsupported, "claude raises no prompts at bypass")
}

// Stop ends claude's process group: Ctrl-C until claude quits — the first
// press stops a turn, or an armed automatic continue, the next quits — then
// SIGTERM to the group, and SIGKILL once grace ends. It reports whether the
// group is gone.
func (t *transport) Stop(ctx context.Context, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	t.mu.Lock()
	t.stopping = true
	t.mu.Unlock()
	if grace > 0 {
		for i := 0; i < 3 && !t.isExited() && time.Now().Before(deadline); i++ {
			if t.keys(keyCtrlC) != nil {
				break
			}
			t.until(ctx, t.procDone, 400*time.Millisecond)
		}
		t.until(ctx, t.procDone, min(time.Until(deadline), quitWait))
	}
	if !t.gone() {
		procgroup.Signal(t.cmd, false)
		t.untilGone(ctx, deadline)
	}
	if !t.gone() {
		t.mu.Lock()
		t.killed = true
		t.mu.Unlock()
		procgroup.Signal(t.cmd, true)
		t.untilGone(ctx, time.Now().Add(killWait))
	}
	return t.gone()
}

// abandon stops a claude Start gives up on, and waits for its exit.
func (t *transport) abandon() {
	ctx, cancel := context.WithTimeout(context.Background(), killWait)
	defer cancel()
	t.Stop(ctx, 0)
	<-t.exited
}

// kill is a crash: SIGKILL to the group, with nothing asked first.
func (t *transport) kill() {
	t.mu.Lock()
	t.killed = true
	t.mu.Unlock()
	procgroup.Signal(t.cmd, true)
}

func (t *transport) isExited() bool {
	select {
	case <-t.procDone:
		return true
	default:
		return false
	}
}

// gone reports whether claude and every process left in its group ended.
func (t *transport) gone() bool {
	select {
	case <-t.exited:
	default:
		return false
	}
	return procgroup.Empty(t.cmd)
}

func (t *transport) until(ctx context.Context, ch <-chan struct{}, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// untilGone polls for the group to end, until deadline or ctx ends.
func (t *transport) untilGone(ctx context.Context, deadline time.Time) {
	for !t.gone() {
		if !time.Now().Before(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}
