package chat

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// Held turns and the busy-gated Send (ADR-006), over the real fake harness.
// A keep-alive claude-code conversation writes its transcript where the fake
// puts it: the launch's assigned session, under a private CLAUDE_CONFIG_DIR.

const apiErrorLine = "API Error: 529 Overloaded. Retry after 30 seconds."

// keepAliveInTempDirs opens keep-alive, in a private working dir and config
// root, with the wrapper polling its classifier every 100ms (its poll is a
// third of IdleQuiet, 5s by default) so a Blocked arrives within the turn.
func keepAliveInTempDirs(t *testing.T) func(*Options) {
	return func(o *Options) {
		o.KeepAliveOnClassification = true
		o.WorkingDir = t.TempDir()
		o.Env = append(o.Env, "CLAUDE_CONFIG_DIR="+filepath.Join(t.TempDir(), "claude"))
		o.wrapperQuiet = 150 * time.Millisecond
		o.wrapperClassify = 5 * time.Second
	}
}

// awaitScreen waits until the conversation's screen shows want.
func awaitScreen(t *testing.T, conv *Conversation, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(conv.screen.Snapshot().Text, want) {
		if time.Now().After(deadline) {
			t.Fatalf("the screen never showed %q", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// heldScript submits, reports an API error the wrapper's matcher reads — the
// Blocked a held turn is held on — and then plays the rest of the turn.
func heldScript(rest func(*fakeharness.Builder) *fakeharness.Builder) fakeharness.Script {
	b := fakeharness.New("claude-code").Idle().AwaitSubmit().Working(30, "Working").Raw(20, apiErrorLine).Working(60, "Retrying")
	return rest(b).StayAliveUntilStopped().Build()
}

func TestHeldTurn_RecoversWithAReply(t *testing.T) {
	conv := openFake(t, heldScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.TranscriptUser(0).TranscriptReply(0, "RECOVERED").Reply(40, "RECOVERED", "Baked", "1s")
	}), keepAliveInTempDirs(t))
	sendOneTurn(t, conv, "go")
	turn := waitForTerminalTurn(t, conv, 10*time.Second)
	if turn.State != TurnStateComplete || turn.Text != "RECOVERED" {
		t.Fatalf("turn = %+v, want complete with the reply the harness recovered with", turn)
	}
	if turn.HTTPCode != 529 {
		t.Errorf("HTTPCode = %d, want the 529 the turn was held on", turn.HTTPCode)
	}
}

func TestHeldTurn_ErrorsWithTheHarnessTag(t *testing.T) {
	const gaveUp = "API Error: Repeated 529 Overloaded errors. The API is at capacity."
	conv := openFake(t, heldScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.TranscriptUser(0).TranscriptAPIError(0, "server_error", gaveUp).Reply(40, gaveUp, "Baked", "1s")
	}), keepAliveInTempDirs(t))
	sendOneTurn(t, conv, "go")
	turn := waitForTerminalTurn(t, conv, 10*time.Second)
	if turn.State != TurnStateErrored || !strings.Contains(turn.Reason, "harness tag: server_error") {
		t.Fatalf("turn = %+v, want errored with the harness's tag", turn)
	}
}

// With no word from the harness's record, a held turn does not complete on
// what the screen shows: a success nobody can confirm is a wrong verdict.
func TestHeldTurn_UnreadableTranscriptErrorsWithTheBlock(t *testing.T) {
	conv := openFake(t, heldScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.Reply(40, "Here you go.", "Baked", "1s")
	}), keepAliveInTempDirs(t))
	sendOneTurn(t, conv, "go")
	turn := waitForTerminalTurn(t, conv, 10*time.Second)
	if turn.State != TurnStateErrored || !strings.Contains(turn.Reason, "api error 529") || turn.Text != "" {
		t.Fatalf("turn = %+v, want errored with the Blocked it was held on", turn)
	}
	if turn.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want the Blocked's 30s", turn.RetryAfter)
	}
}

func TestHeldTurn_ExitWhileHeldErrors(t *testing.T) {
	script := fakeharness.New("claude-code").Idle().AwaitSubmit().Working(30, "Working").
		Raw(20, apiErrorLine).Working(300, "Retrying").Exit(0).Build()
	conv := openFake(t, script, keepAliveInTempDirs(t))
	sendOneTurn(t, conv, "go")
	turn := waitForTerminalTurn(t, conv, 10*time.Second)
	if turn.State != TurnStateErrored || !strings.Contains(turn.Reason, "harness exited") {
		t.Fatalf("turn = %+v, want errored: harness exited", turn)
	}
	if turn.HTTPCode != 529 {
		t.Errorf("HTTPCode = %d, want the held 529 kept", turn.HTTPCode)
	}
}

// A session-limit banner mid-turn is reported by the wrapper at once; the turn
// ends only when claude ends it, as usage-limited with the reset time.
func TestHeldTurn_SessionLimitEndsAtClaudesEndOfTurn(t *testing.T) {
	const wall = "You've hit your session limit · resets 6:40pm (UTC)"
	script := fakeharness.New("claude-code").Idle().AwaitSubmit().Working(30, "Working").
		Raw(20, "  ⎿  "+wall).Working(300, "Working").Reply(40, wall, "Baked", "1s").
		StayAliveUntilStopped().Build()
	conv := openFake(t, script, keepAliveInTempDirs(t))
	sendOneTurn(t, conv, "go")
	sent := time.Now()
	turn := waitForTerminalTurn(t, conv, 10*time.Second)
	if turn.State != TurnStateErrored || turn.Code != CodeUsageLimited || turn.ResumeAt.IsZero() {
		t.Fatalf("turn = %+v, want usage_limited with ResumeAt", turn)
	}
	if time.Since(sent) < 300*time.Millisecond {
		t.Fatalf("the turn ended %s after Send, before claude ended it", time.Since(sent))
	}
}

// The default mode keeps a Blocked ending the turn, which run-to-completion
// callers rely on (and TestHandleTurnsEvent_APIErrorFieldsForwarded pins at the
// unit level).
func TestHeldTurn_DefaultModeEndsOnTheBlock(t *testing.T) {
	conv := openFake(t, heldScript(func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.Reply(40, "Here you go.", "Baked", "1s")
	}), func(o *Options) {
		o.WorkingDir = t.TempDir()
		o.wrapperQuiet = 150 * time.Millisecond
		o.wrapperClassify = 5 * time.Second
	})
	sendOneTurn(t, conv, "go")
	turn := waitForTerminalTurn(t, conv, 10*time.Second)
	if turn.State != TurnStateErrored || turn.HTTPCode != 529 {
		t.Fatalf("turn = %+v, want errored on the Blocked", turn)
	}
}

// busyHarness paints claude working (spinner in the status line, footer hint
// below the composer box) for a while before it settles, with no turn of ours
// in flight — work a person started at the TUI, or a turn chat ended early.
func busyHarness(settleAfter time.Duration, afterSettle func(*fakeharness.Builder) *fakeharness.Builder) fakeharness.Script {
	b := fakeharness.New("claude-code").ComposerBox().Idle()
	for d := time.Duration(0); d < settleAfter; d += 100 * time.Millisecond {
		b = b.Working(100, "Working")
	}
	b = b.SettleIdle(50, "settled")
	return afterSettle(b).StayAliveUntilStopped().Build()
}

func TestSend_WaitsForABusyHarnessToSettle(t *testing.T) {
	conv := openFake(t, busyHarness(600*time.Millisecond, func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().Reply(40, "answered", "Baked", "1s")
	}))
	awaitScreen(t, conv, "Working…")
	sendOneTurn(t, conv, "go")
	if !strings.Contains(conv.screen.Snapshot().Text, "settled") {
		t.Fatal("Send typed before the harness settled")
	}
	if idle := time.Since(time.Unix(0, conv.lastBusyAt.Load())); idle < testMarkerGap {
		t.Fatalf("Send typed %s after the harness last looked busy, want at least %s", idle, testMarkerGap)
	}
	if turn := waitForTerminalTurn(t, conv, 10*time.Second); turn.State != TurnStateComplete {
		t.Fatalf("turn = %+v, want complete", turn)
	}
}

func TestSend_RetryBackoffHoldsSend(t *testing.T) {
	script := fakeharness.New("claude-code").ComposerBox().Idle().
		RetryBackoff(0, 1, 1).RetryBackoff(250, 2, 2).RetryBackoff(250, 1, 2).
		SettleIdle(250, "settled").AwaitSubmit().Reply(40, "answered", "Baked", "1s").
		StayAliveUntilStopped().Build()
	conv := openFake(t, script)
	awaitScreen(t, conv, "Retrying in")
	sendOneTurn(t, conv, "go")
	if !strings.Contains(conv.screen.Snapshot().Text, "settled") {
		t.Fatal("Send typed into a harness backing off before a retry")
	}
}

// A ctx that ends while the harness is still working is ErrHarnessBusy, with
// nothing typed and no turn recorded.
func TestSend_BusyUntilContextEndsIsErrHarnessBusy(t *testing.T) {
	conv := openFake(t, busyHarness(3*time.Second, func(b *fakeharness.Builder) *fakeharness.Builder {
		return b.AwaitSubmit().Raw(0, "TYPED")
	}))
	awaitScreen(t, conv, "Working…")
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conv.Send(ctx, "go")
	release()
	if !errors.Is(err, ErrHarnessBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send = %v, want ErrHarnessBusy wrapping the deadline", err)
	}
	if turns, _ := conv.store.ListTurns(context.Background(), conv.SessionID()); len(turns) != 0 {
		t.Fatalf("turns recorded: %+v, want none", turns)
	}
	time.Sleep(3500 * time.Millisecond)
	if strings.Contains(conv.screen.Snapshot().Text, "TYPED") {
		t.Fatal("the harness received a submit; Send typed into it")
	}
}

// The sub-agent flicker — footer and spinner gone for one frame mid-work —
// is shorter than the confirmation window, so it never opens the gate.
func TestSend_FlickerDoesNotOpenTheGate(t *testing.T) {
	script := fakeharness.New("claude-code").ComposerBox().Idle().
		Working(0, "Working").MarkerFlicker(60, "Baked", "1s", "exploring").Working(60, "Working").
		Flicker(60, "exploring").Working(60, "Working").SettleIdle(60, "settled").
		AwaitSubmit().Reply(40, "answered", "Brewed", "2s").StayAliveUntilStopped().Build()
	conv := openFake(t, script)
	awaitScreen(t, conv, "Working…")
	sendOneTurn(t, conv, "go")
	if !strings.Contains(conv.screen.Snapshot().Text, "settled") {
		t.Fatal("Send typed during a flicker frame")
	}
}

// A settled reply that quotes the working markers does not hold Send: they sit
// in the conversation, above the status region.
func TestSend_QuotedMarkersDoNotHoldSend(t *testing.T) {
	const quoting = `While it works, claude's footer says "esc to interrupt" and its status line reads "✶ Cerebrating… (57s · ↓ 4.8k tokens)".`
	script := fakeharness.New("claude-code").ComposerBox().Idle().
		AwaitSubmit().Working(30, "Working").Reply(40, quoting, "Baked", "1s").
		AwaitSubmit().Reply(40, "second", "Brewed", "2s").StayAliveUntilStopped().Build()
	conv := openFake(t, script)
	sendOneTurn(t, conv, "explain")
	waitForTerminalTurn(t, conv, 10*time.Second)
	start := time.Now()
	sendOneTurn(t, conv, "again")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the second Send waited %s on a quoted marker", d)
	}
	if turn := waitForTerminalTurn(t, conv, 10*time.Second); turn.State != TurnStateComplete {
		t.Fatalf("second turn = %+v, want complete", turn)
	}
}
