package chat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

// These tests model claude as an APPLICATION whose state is separate from the
// frame on the PTY. Keys change the application state the moment they arrive;
// what the screen shows is whatever the scenario's repaint policy lets through.
// That separation is the whole point: a frame that did not change after Enter
// cannot tell "Enter was rejected" from "Enter was accepted and the next screen
// has not painted yet", and in the second case a retried Enter lands on the
// NEXT dialog — on a bypass launch, its default row is "No, exit".

const (
	appTrust  = "trust"
	appBypass = "bypass"
	appReady  = "composer"
	appExited = "exited"
)

var (
	appTrustOptions  = []string{"No, exit", "Yes, I trust this folder"}
	appBypassOptions = []string{"No, exit", "Yes, I accept"}
)

// appState is the application's view: which screen it is on and where its
// highlight is.
type appState struct {
	screen string
	marker int
}

// appWrite is one write as the application received it.
type appWrite struct {
	p  []byte
	at appState
}

type dialogApp struct {
	t   *testing.T
	c   *Conversation
	scr *screen.Screen

	mu     sync.Mutex
	state  appState
	wrap   bool // arrows wrap around the menu instead of stopping at its ends
	writes []appWrite

	// afterTrustAccepted is where accepting the trust dialog leads (the bypass
	// dialog on a bypass launch, else the composer).
	afterTrustAccepted string
	// onKey decides what to paint after the application handled one key; it
	// returns the frame to paint, or "" to paint nothing (a stale screen). It
	// runs without the lock held. Nil paints the new state immediately.
	onKey func(a *dialogApp, key string, prev, next appState) string
	// dropNext makes the application ignore the next n arrow keys entirely.
	dropArrows int
}

func (a *dialogApp) options(s string) []string {
	if s == appBypass {
		return appBypassOptions
	}
	return appTrustOptions
}

func frameFor(s appState) string {
	switch s.screen {
	case appTrust:
		return selectorFrame("Quick safety check: Is this a project you created or one you trust?", appTrustOptions, s.marker)
	case appBypass:
		return "WARNING: Claude Code running in Bypass Permissions mode\r\n\r\nBy proceeding, you accept all risks.\r\n\r\n" +
			menuRows(appBypassOptions, s.marker) + "\r\nEnter to confirm · Esc to cancel\r\n"
	case appExited:
		return "$ \r\n"
	default:
		return composerFrame
	}
}

func selectorFrame(prompt string, opts []string, marker int) string {
	return prompt + "\r\n\r\n" + menuRows(opts, marker) + "\r\nEnter to confirm · Esc to cancel\r\n"
}

func menuRows(opts []string, marker int) string {
	var b strings.Builder
	for i, o := range opts {
		if i == marker {
			b.WriteString("❯ " + o + "\r\n")
		} else {
			b.WriteString("  " + o + "\r\n")
		}
	}
	return b.String()
}

func newDialogApp(t *testing.T, budget time.Duration) *dialogApp {
	t.Helper()
	a := &dialogApp{t: t, scr: screen.New(120, 40), state: appState{screen: appTrust}, afterTrustAccepted: appBypass}
	a.c = &Conversation{
		opts: Options{
			Harness:               chatClaudeCode,
			permModeRenderTimeout: budget,
		},
		screen:       a.scr,
		eventCh:      make(chan ConversationEvent, 8),
		closed:       make(chan struct{}),
		inputStateCh: make(chan struct{}, 1),
		queue:        newControlQueue(),
	}
	a.c.writeStdin = a.write
	a.paint(frameFor(a.state))
	return a
}

func (a *dialogApp) paint(frame string) {
	if _, err := a.scr.Write([]byte("\x1b[2J\x1b[H" + frame)); err != nil {
		a.t.Errorf("paint: %v", err)
	}
}

// write is the PTY: the application handles each key in order, then the
// scenario decides what reaches the screen.
func (a *dialogApp) write(p []byte) (int, error) {
	a.mu.Lock()
	a.writes = append(a.writes, appWrite{p: append([]byte(nil), p...), at: a.state})
	var paints []func() string
	rest := string(p)
	for rest != "" {
		key := ""
		switch {
		case strings.HasPrefix(rest, "\x1b[B"), strings.HasPrefix(rest, "\x1b[A"):
			key, rest = rest[:3], rest[3:]
		case strings.HasPrefix(rest, "\r"):
			key, rest = "\r", rest[1:]
		default:
			key, rest = rest[:1], rest[1:]
		}
		prev := a.state
		a.handleLocked(key)
		next := a.state
		onKey := a.onKey
		paints = append(paints, func() string {
			if onKey == nil {
				return frameFor(next)
			}
			return onKey(a, key, prev, next)
		})
	}
	a.mu.Unlock()
	for _, f := range paints {
		if frame := f(); frame != "" {
			a.paint(frame)
		}
	}
	return len(p), nil
}

func (a *dialogApp) handleLocked(key string) {
	s := &a.state
	if s.screen != appTrust && s.screen != appBypass {
		return
	}
	n := len(a.options(s.screen))
	switch key {
	case "\x1b[B", "\x1b[A":
		if a.dropArrows > 0 {
			a.dropArrows--
			return
		}
		d := 1
		if key == "\x1b[A" {
			d = -1
		}
		m := s.marker + d
		switch {
		case m < 0 && a.wrap:
			m = n - 1
		case m >= n && a.wrap:
			m = 0
		case m < 0:
			m = 0
		case m >= n:
			m = n - 1
		}
		s.marker = m
	case "\r":
		chosen := a.options(s.screen)[s.marker]
		switch {
		case chosen == "No, exit":
			*s = appState{screen: appExited}
		case s.screen == appTrust:
			*s = appState{screen: a.afterTrustAccepted}
		default:
			*s = appState{screen: appReady}
		}
	}
}

func (a *dialogApp) recorded() []appWrite {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]appWrite(nil), a.writes...)
}

func (a *dialogApp) current() appState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// enters counts writes that carried a confirm key, and returns the application
// states they arrived in.
func (a *dialogApp) enters() []appState {
	var at []appState
	for _, w := range a.recorded() {
		if bytes.Contains(w.p, []byte("\r")) {
			at = append(at, w.at)
		}
	}
	return at
}

func (a *dialogApp) trustRequest(t *testing.T) (*turns.InputRequest, *turns.InputOption) {
	t.Helper()
	req, ok := claudecode.DetectInput(a.scr.Snapshot().Text)
	if !ok {
		t.Fatal("fixture no longer detects as a trust dialog")
	}
	opt := findOption(req, "Yes, I trust this folder")
	if opt == nil {
		t.Fatalf("no trust option in %+v", req.Options)
	}
	return req, opt
}

// The plan-review reproduction, with the application modelled: the first Enter
// is ACCEPTED and the next dialog (bypass acceptance, "No, exit" highlighted)
// is active, but the PTY keeps showing the old trust frame past every timeout
// and backoff. No key may reach that next dialog.
func TestAnswer_AcceptedEnterWithStaleFrameSendsNothingMore(t *testing.T) {
	a := newDialogApp(t, 60*time.Millisecond)
	a.onKey = func(_ *dialogApp, key string, _, next appState) string {
		if key == "\r" {
			return "" // accepted; the next screen never paints
		}
		return frameFor(next)
	}
	req, opt := a.trustRequest(t)

	start := time.Now()
	_ = a.c.answerAndConfirm(context.Background(), req, opt)

	for _, w := range a.recorded() {
		if w.at.screen != appTrust {
			t.Errorf("wrote %q to the %s screen after the trust dialog had taken its answer", w.p, w.at.screen)
		}
	}
	if got := a.current().screen; got != appBypass {
		t.Errorf("application ended on %q, want it still waiting at the bypass dialog (nothing answered it)", got)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v; a stale frame must still end in a bounded failure", time.Since(start))
	}
}

// The first arrow is slow, not lost: the application moves the highlight at
// once, but the PTY runs one arrow behind — each arrow's frame paints only when
// the NEXT arrow arrives. On a menu that wraps, a re-sent arrow walks the
// application's highlight back onto "No, exit" at the very moment the late
// frame shows the target. Enter must never be pressed on that frame.
func TestAnswer_DelayedNavigationNeverConfirmsTheWrongRow(t *testing.T) {
	a := newDialogApp(t, 60*time.Millisecond)
	a.wrap = true
	var pending string
	a.onKey = func(_ *dialogApp, key string, _, next appState) string {
		if key == "\x1b[B" || key == "\x1b[A" {
			late := pending
			pending = frameFor(next)
			return late // the previous arrow's frame, or nothing for the first
		}
		return frameFor(next)
	}
	req, opt := a.trustRequest(t)
	_ = a.c.answerAndConfirm(context.Background(), req, opt)

	for _, at := range a.enters() {
		if at.screen == appTrust && appTrustOptions[at.marker] != "Yes, I trust this folder" {
			t.Fatalf("pressed Enter with the application's highlight on %q", appTrustOptions[at.marker])
		}
	}
	if a.current().screen == appExited {
		t.Fatal("the answer made claude exit")
	}
}

// Lost navigation must recover or fail promptly — and never by pressing Enter
// on the row the highlight is actually on.
func TestAnswer_LostNavigationFailsPromptlyWithoutEnter(t *testing.T) {
	a := newDialogApp(t, 60*time.Millisecond)
	a.dropArrows = 1 << 30 // this dialog never moves
	req, opt := a.trustRequest(t)

	start := time.Now()
	err := a.c.answerAndConfirm(context.Background(), req, opt)
	if !errors.Is(err, ErrInputUnresolved) {
		t.Fatalf("err = %v, want ErrInputUnresolved", err)
	}
	if n := len(a.enters()); n != 0 {
		t.Fatalf("pressed Enter %d times on a dialog whose highlight never left %q", n, "No, exit")
	}
	var ue *InputUnresolvedError
	if errors.As(err, &ue) && ue.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0: no answer was ever written", ue.Attempts)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v to give up on a dialog that never moved", time.Since(start))
	}
}

// A different dialog replacing this one while navigating is a resolution of
// this one: its keys must not be pressed into the newcomer.
func TestAnswer_NextDialogDuringNavigationGetsNoEnter(t *testing.T) {
	a := newDialogApp(t, 60*time.Millisecond)
	a.onKey = func(a *dialogApp, key string, _, next appState) string {
		if key == "\x1b[B" && next.screen == appTrust {
			a.state = appState{screen: appBypass} // the bypass dialog takes over mid-navigation
			return frameFor(a.state)
		}
		return frameFor(next)
	}
	req, opt := a.trustRequest(t)
	_ = a.c.answerAndConfirm(context.Background(), req, opt)
	for _, w := range a.recorded() {
		if w.at.screen == appBypass {
			t.Errorf("wrote %q into the bypass dialog that replaced the trust dialog", w.p)
		}
	}
}

// A half-painted frame (the anchor up, the menu not yet) is neither the
// highlight landing nor the dialog clearing. Enter goes out exactly once, after
// the complete frame shows the highlight on the target.
func TestAnswer_PartialRepaintIsNotEvidence(t *testing.T) {
	a := newDialogApp(t, 2*time.Second)
	a.afterTrustAccepted = appReady
	a.onKey = func(a *dialogApp, key string, _, next appState) string {
		if key == "\x1b[B" {
			go func(s appState) {
				a.paint("Quick safety check: Is this a project you created or one you trust?\r\n\r\n")
				time.Sleep(80 * time.Millisecond)
				a.paint(frameFor(s))
			}(next)
			return ""
		}
		return frameFor(next)
	}
	req, opt := a.trustRequest(t)
	if err := a.c.answerAndConfirm(context.Background(), req, opt); err != nil {
		t.Fatalf("answerAndConfirm: %v", err)
	}
	ents := a.enters()
	if len(ents) != 1 || appTrustOptions[ents[0].marker] != "Yes, I trust this folder" {
		t.Fatalf("Enter presses arrived at %+v; want exactly one, with the highlight on the target", ents)
	}
}

// The measured 2.1.261 failure: the dialog takes Enter, then re-renders from
// its default state. That repaint — the same dialog, highlight moved off the
// row just confirmed — is positive evidence the answer did not take, and the
// re-answer is computed from the live screen.
func TestAnswer_ExplicitResetIsReansweredFromTheLiveScreen(t *testing.T) {
	a := newDialogApp(t, 2*time.Second)
	a.afterTrustAccepted = appReady
	resets := 1
	a.onKey = func(a *dialogApp, key string, prev, next appState) string {
		if key == "\r" && prev.screen == appTrust && resets > 0 {
			resets--
			a.state = appState{screen: appTrust} // swallowed: back to the default highlight
			return frameFor(a.state)
		}
		return frameFor(next)
	}
	req, opt := a.trustRequest(t)
	if err := a.c.answerAndConfirm(context.Background(), req, opt); err != nil {
		t.Fatalf("answerAndConfirm: %v", err)
	}
	ents := a.enters()
	if len(ents) != 2 {
		t.Fatalf("Enter pressed %d times, want 2 (one reset, one re-answer)", len(ents))
	}
	for _, at := range ents {
		if appTrustOptions[at.marker] != "Yes, I trust this folder" {
			t.Errorf("an Enter arrived with the highlight on %q", appTrustOptions[at.marker])
		}
	}
	if a.current().screen != appReady {
		t.Errorf("application on %q, want the composer", a.current().screen)
	}
}

// Cancellation and Close end the answer at once and write nothing afterwards.
func TestAnswer_CancellationAndCloseStopWriting(t *testing.T) {
	for _, how := range []string{"cancel", "close"} {
		t.Run(how, func(t *testing.T) {
			a := newDialogApp(t, 5*time.Second)
			a.dropArrows = 1 << 30
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, opt := a.trustRequest(t)
			done := make(chan error, 1)
			go func() { done <- a.c.answerAndConfirm(ctx, req, opt) }()
			time.Sleep(100 * time.Millisecond)
			before := len(a.recorded())
			if how == "cancel" {
				cancel()
			} else {
				close(a.c.closed)
			}
			select {
			case err := <-done:
				want := context.Canceled
				if how == "close" {
					want = ErrClosed
				}
				if !errors.Is(err, want) {
					t.Fatalf("err = %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("answerAndConfirm did not return after " + how)
			}
			time.Sleep(100 * time.Millisecond)
			if after := len(a.recorded()); after != before {
				t.Errorf("%d writes after %s", after-before, how)
			}
		})
	}
}

// A dialog that resets after every answer gets a bounded number of answers and
// a typed failure that says how many were actually written.
func TestAnswer_BoundedFailureReportsTheAnswersWritten(t *testing.T) {
	a := newDialogApp(t, 60*time.Millisecond)
	a.onKey = func(a *dialogApp, key string, prev, next appState) string {
		if key == "\r" && prev.screen == appTrust {
			a.state = appState{screen: appTrust}
			return frameFor(a.state)
		}
		return frameFor(next)
	}
	req, opt := a.trustRequest(t)
	start := time.Now()
	err := a.c.answerAndConfirm(context.Background(), req, opt)
	var ue *InputUnresolvedError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v, want *InputUnresolvedError", err)
	}
	if n := len(a.enters()); n != answerAttempts || ue.Attempts != n {
		t.Errorf("Enter pressed %d times, Attempts = %d; want both %d", n, ue.Attempts, answerAttempts)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v to give up", time.Since(start))
	}
}

// TestReviewAcceptedAnswerWithStaleFrame is the plan review's reproduction,
// kept verbatim: the application accepted Enter and moved to its next dialog,
// but the PTY still shows the old frame. Retrying against that frame must not
// send Enter.
func TestReviewAcceptedAnswerWithStaleFrame(t *testing.T) {
	accepted := false
	stray := 0
	f := newAnswerFake(t, 30*time.Millisecond, trustFrameMarkerOnExit, func(f *answerFake, p []byte) {
		if bytes.Equal(p, navDown) {
			f.paint(trustFrameMarkerOnTrust)
		}
		if bytes.Equal(p, confirm) {
			if accepted {
				stray++
				f.paint(composerFrame)
			}
			accepted = true
		}
	})
	req, opt := trustRequestUnnumbered(t)
	_ = f.c.answerAndConfirm(context.Background(), req, opt)
	if stray != 0 {
		t.Fatalf("sent %d extra Enter to the next application state while the PTY frame was stale", stray)
	}
}
