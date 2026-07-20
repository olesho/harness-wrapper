package chat

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
)

func loadCorpusScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(authCorpusRoot, name, "screen.txt"))
	if err != nil {
		t.Fatalf("read corpus screen %s: %v", name, err)
	}
	return string(b)
}

// Fix A: a logged-out claude turn completes via the "✻ … for 0s" end-of-turn
// marker (NOT an error), so authRelabel must convert that would-be success into
// ReasonAuthRequired when no real reply was extracted — killing the false-success
// where the raw banner screen is persisted as the assistant "reply".
func TestAuthRelabel_ClaudeFalseSuccess(t *testing.T) {
	for _, fx := range []string{
		"claude-code/not-logged-in-brewed",
		"claude-code/not-logged-in-churned",
	} {
		t.Run(fx, func(t *testing.T) {
			snap := screen.Snapshot{Text: loadCorpusScreen(t, fx)}
			c := &Conversation{opts: Options{Harness: chatClaudeCode}, adapter: claudecode.New()}
			turn := &Turn{
				State:  TurnStateComplete,
				Reason: "claude-code: end-of-turn marker confirmed at a settled prompt",
				Text:   snap.Text, // the whole-screen fallback the bug would persist
			}
			if !c.authRelabel(turn, snap) {
				t.Fatalf("authRelabel did not fire on a logged-out claude screen")
			}
			if turn.State != TurnStateErrored {
				t.Errorf("State = %q, want %q", turn.State, TurnStateErrored)
			}
			if turn.Reason != ReasonAuthRequired {
				t.Errorf("Reason = %q, want ReasonAuthRequired", turn.Reason)
			}
			if turn.Text != "" {
				t.Errorf("Text = %q, want empty (banner must not be persisted as a reply)", turn.Text)
			}
		})
	}
}

// Fix A negative: a genuine reply (a "⏺" bullet) that merely MENTIONS "/login" is
// never relabeled, even though authRequired matches the screen — the empty-clean-
// extraction gate protects it.
func TestAuthRelabel_GenuineReplyUntouched(t *testing.T) {
	snap := screen.Snapshot{Text: "⏺ To fix this, run /login in your terminal.\n\n✻ Brewed for 2s\n\n❯ \n"}
	c := &Conversation{opts: Options{Harness: chatClaudeCode}, adapter: claudecode.New()}
	if !authRequired(chatClaudeCode, snap.Text) {
		t.Fatalf("precondition: screen should match authRequired (mentions run /login)")
	}
	turn := &Turn{State: TurnStateComplete, Text: "To fix this, run /login in your terminal."}
	if c.authRelabel(turn, snap) {
		t.Errorf("authRelabel wrongly fired on a genuine reply mentioning /login")
	}
	if turn.State != TurnStateComplete {
		t.Errorf("State = %q, want unchanged %q", turn.State, TurnStateComplete)
	}
}

// Fix B: an onboarding wall (never-signed-in codex menu, fresh-claude wizard) is
// not-ready and never becomes ready, so waitReadyForSend short-circuits with
// ErrAuthRequired within authGateStabilizeGap instead of hanging to the deadline.
func TestWaitReadyForSend_OnboardingShortCircuit(t *testing.T) {
	for _, tc := range []struct{ harness, fx string }{
		{"codex", "codex/onboarding"},
		{chatClaudeCode, "claude-code/theme-picker"},
	} {
		t.Run(tc.fx, func(t *testing.T) {
			scr := screen.New(160, 60)
			if _, err := scr.Write([]byte(loadCorpusScreen(t, tc.fx))); err != nil {
				t.Fatalf("screen.Write: %v", err)
			}
			c := &Conversation{
				opts:         Options{Harness: tc.harness},
				screen:       scr,
				inputStateCh: make(chan struct{}, 1),
				closed:       make(chan struct{}),
			}
			ctx, cancel := context.WithTimeout(context.Background(), authGateStabilizeGap+3*time.Second)
			defer cancel()
			start := time.Now()
			err := c.waitReadyForSend(ctx)
			if !errors.Is(err, ErrAuthRequired) {
				t.Fatalf("waitReadyForSend = %v, want ErrAuthRequired", err)
			}
			if time.Since(start) > authGateStabilizeGap+2*time.Second {
				t.Errorf("short-circuit took %v, expected ~%v (not the full deadline)", time.Since(start), authGateStabilizeGap)
			}
		})
	}
}

// Fix B negative: a normal ready composer is sent through immediately — the auth
// gate must not fire on a healthy composer.
func TestWaitReadyForSend_ReadyComposerSendsThrough(t *testing.T) {
	scr := screen.New(160, 60)
	if _, err := scr.Write([]byte(loadCorpusScreen(t, "codex/normal-composer"))); err != nil {
		t.Fatalf("screen.Write: %v", err)
	}
	c := &Conversation{
		opts:         Options{Harness: "codex"},
		screen:       scr,
		inputStateCh: make(chan struct{}, 1),
		closed:       make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.waitReadyForSend(ctx); err != nil {
		t.Fatalf("waitReadyForSend on a ready composer = %v, want nil", err)
	}
}
