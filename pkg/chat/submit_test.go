package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// The contract these lock: the submit key is written only AFTER the composer
// has shown the text (or the bound expires). A harness that collapses a large
// paste absorbs a submit key riding in the same burst, so "text and key in one
// write" is the bug — and "submit, then retry until it looks sent" is the wrong
// fix, because it can double-submit a prompt that did land.

// echoHarness drives a Conversation's screen from the test.
func newEchoConversation(t *testing.T, harness string, idleGap time.Duration) *Conversation {
	t.Helper()
	c := &Conversation{
		opts:   Options{Harness: harness, idleGap: idleGap},
		screen: screen.New(120, 40),
		closed: make(chan struct{}),
	}
	return c
}

func TestAwaitComposerEcho_ReturnsWhenTheTextAppears(t *testing.T) {
	c := newEchoConversation(t, chatClaudeCode, 4*time.Second)
	pre := c.screen.Snapshot().Text

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = c.screen.Write([]byte("❯ ship the turn API\r\n"))
	}()

	start := time.Now()
	if err := c.awaitComposerEcho(context.Background(), "ship the turn API", pre); err != nil {
		t.Fatalf("awaitComposerEcho: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= c.echoBoundDur() {
		t.Fatalf("waited %v — it fell through to the bound instead of seeing the echo", elapsed)
	}
}

// A collapsed paste never echoes the text: the composer shows
// "[Pasted text #1 +157 lines]" instead. The screen still CHANGES, and that is
// the fallback this depends on — without it a large prompt would always pay the
// full bound.
func TestAwaitComposerEcho_CollapsedPasteCountsAsEcho(t *testing.T) {
	c := newEchoConversation(t, chatClaudeCode, 4*time.Second)
	pre := c.screen.Snapshot().Text

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = c.screen.Write([]byte("❯ [Pasted text #1 +157 lines]\r\n"))
	}()

	start := time.Now()
	text := "## WORKFLOW: Planning Task\nline two\nline three\n"
	if err := c.awaitComposerEcho(context.Background(), text, pre); err != nil {
		t.Fatalf("awaitComposerEcho: %v", err)
	}
	// The needle never appears, so this must come back via the halfway
	// screen-change arm — after bound/2, well before the full bound.
	elapsed := time.Since(start)
	if elapsed >= c.echoBoundDur() {
		t.Fatalf("waited the full bound (%v); the screen-change fallback did not fire", elapsed)
	}
}

// Degrade, never hang: if the composer never shows anything, the submit is
// still written once the bound expires.
func TestAwaitComposerEcho_DegradesOnTheBound(t *testing.T) {
	c := newEchoConversation(t, chatClaudeCode, 200*time.Millisecond)
	pre := c.screen.Snapshot().Text

	start := time.Now()
	if err := c.awaitComposerEcho(context.Background(), "never echoed", pre); err != nil {
		t.Fatalf("awaitComposerEcho must degrade, not fail: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %v — the bound is not being honoured", elapsed)
	}
}

// A cancelled run is different from a missed echo: the whole run is over, so it
// propagates rather than writing a submit into a dead session.
func TestAwaitComposerEcho_PropagatesRunCancellation(t *testing.T) {
	c := newEchoConversation(t, chatClaudeCode, 10*time.Second)
	pre := c.screen.Snapshot().Text

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.awaitComposerEcho(ctx, "anything", pre); err == nil {
		t.Fatal("awaitComposerEcho returned nil for a cancelled run; it must not submit into it")
	}
}

// The echo wait must finish well inside the idle-completion window, or the
// swallowed-prompt check could judge the turn before the submit is even
// written.
func TestEchoBound_StaysInsideTheIdleWindow(t *testing.T) {
	c := newEchoConversation(t, chatClaudeCode, 600*time.Millisecond)
	if got, max := c.echoBoundDur(), 300*time.Millisecond; got > max {
		t.Fatalf("echoBoundDur = %v, want <= idleGap/2 (%v)", got, max)
	}

	wide := newEchoConversation(t, chatClaudeCode, time.Hour)
	if got := wide.echoBoundDur(); got != submitEchoGap {
		t.Fatalf("echoBoundDur = %v, want the configured gap %v when idleGap is generous", got, submitEchoGap)
	}
}

// Harnesses with no composer to echo into keep the single combined write; the
// echo path must not be imposed on them.
func TestRequiresPromptReadiness_GatesTheEchoPath(t *testing.T) {
	for _, h := range []string{chatClaudeCode, "codex", "pi"} {
		if !requiresPromptReadiness(h) {
			t.Fatalf("requiresPromptReadiness(%q) = false, want true", h)
		}
	}
	if requiresPromptReadiness("generic") {
		t.Fatal(`requiresPromptReadiness("generic") = true; a harness with no prompt gate must keep the single write`)
	}
}

func TestAwaitComposerEcho_EmptyNeedleUsesScreenChange(t *testing.T) {
	c := newEchoConversation(t, chatClaudeCode, 4*time.Second)
	pre := c.screen.Snapshot().Text

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = c.screen.Write([]byte("anything at all\r\n"))
	}()

	// Leading newline: the first line is empty, so there is no needle to match.
	if err := c.awaitComposerEcho(context.Background(), "\nbody", pre); err != nil {
		t.Fatalf("awaitComposerEcho: %v", err)
	}
	if strings.TrimSpace(c.screen.Snapshot().Text) == "" {
		t.Fatal("screen never changed; the test did not exercise the fallback")
	}
}
