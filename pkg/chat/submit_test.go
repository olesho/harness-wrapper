package chat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
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
	if err := c.awaitComposerEcho(context.Background(), "ship the turn API", pre, false); err != nil {
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
	if err := c.awaitComposerEcho(context.Background(), text, pre, false); err != nil {
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
	if err := c.awaitComposerEcho(context.Background(), "never echoed", pre, false); err != nil {
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

	if err := c.awaitComposerEcho(ctx, "anything", pre, false); err == nil {
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
	if err := c.awaitComposerEcho(context.Background(), "\nbody", pre, false); err != nil {
		t.Fatalf("awaitComposerEcho: %v", err)
	}
	if strings.TrimSpace(c.screen.Snapshot().Text) == "" {
		t.Fatal("screen never changed; the test did not exercise the fallback")
	}
}

// ── bracketed paste (PUPPET-194) ──────────────────────────────────────────────
//
// The second defect of the same write. Where the tests above lock "the submit
// key is a SEPARATE, LATER write", these lock "a large payload is declared as a
// PASTE, in ONE write" — measured 2026-08-27 on claude-code 2.1.247, where an
// unframed 2627-byte prompt reached the model as its last read chunk only in 5
// of 10 runs and a framed one arrived whole in 10 of 10.

// writeRecorder records each write SEPARATELY, which is the whole point: the
// order and the boundaries between writes are the contract under test, and a
// single concatenated buffer cannot tell "one write" from "two".
type writeRecorder struct{ writes [][]byte }

func (w *writeRecorder) write(p []byte) (int, error) {
	w.writes = append(w.writes, append([]byte(nil), p...))
	return len(p), nil
}

// newPasteConversation builds a Conversation whose screen changes on the first
// poll, so awaitComposerEcho's screen-change arm returns immediately and the
// test measures write SHAPE, not timing.
func newPasteConversation(t *testing.T, harness string, rec *writeRecorder) *Conversation {
	t.Helper()
	c := newEchoConversation(t, harness, 4*time.Second)
	c.writeStdin = rec.write
	return c
}

func largeText(n int) string {
	line := "the quick brown fox jumps over the lazy dog and keeps on running\n"
	var b strings.Builder
	for b.Len() < n {
		b.WriteString(line)
	}
	return b.String()
}

// The core contract: markers + text in ONE write, submit key in a LATER one.
func TestWriteMessageAndSubmit_FramesALargePayloadAsOnePaste(t *testing.T) {
	rec := &writeRecorder{}
	c := newPasteConversation(t, chatClaudeCode, rec)
	text := largeText(pasteThreshold + 200)

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = c.screen.Write([]byte("❯ [Pasted text #1 +20 lines]\r\n"))
	}()

	if err := c.writeMessageAndSubmit(context.Background(), text, c.screen.Snapshot().Text, []byte(fakeharness.SubmitCSI13u)); err != nil {
		t.Fatalf("writeMessageAndSubmit: %v", err)
	}
	if len(rec.writes) != 2 {
		t.Fatalf("got %d writes, want exactly 2 (framed payload, then submit key): %q", len(rec.writes), rec.writes)
	}
	want := pasteStartCSI200 + text + pasteEndCSI201
	if got := string(rec.writes[0]); got != want {
		t.Fatalf("first write is not the framed payload\n got %d bytes starting %q ending %q\nwant %d bytes",
			len(got), got[:min(12, len(got))], got[max(0, len(got)-12):], len(want))
	}
	if got := string(rec.writes[1]); got != fakeharness.SubmitCSI13u {
		t.Fatalf("second write = %q, want the submit key %q", got, fakeharness.SubmitCSI13u)
	}
}

// The compatibility guarantee for every existing scenario: below the threshold
// nothing changes, byte for byte.
func TestWriteMessageAndSubmit_ShortTextIsNotFramed(t *testing.T) {
	rec := &writeRecorder{}
	c := newPasteConversation(t, chatClaudeCode, rec)
	text := "ship the turn API"

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = c.screen.Write([]byte("❯ ship the turn API\r\n"))
	}()

	if err := c.writeMessageAndSubmit(context.Background(), text, c.screen.Snapshot().Text, []byte(fakeharness.SubmitCSI13u)); err != nil {
		t.Fatalf("writeMessageAndSubmit: %v", err)
	}
	if len(rec.writes) != 2 {
		t.Fatalf("got %d writes, want 2: %q", len(rec.writes), rec.writes)
	}
	if got := string(rec.writes[0]); got != text {
		t.Fatalf("first write = %q, want the raw text %q — short input must not be framed", got, text)
	}
}

// A slash command must still open the command palette, which a PASTE does not.
func TestShouldPaste_NeverFramesASlashCommand(t *testing.T) {
	text := "/compact " + largeText(pasteThreshold)
	if _, _, framed := shouldPaste(chatClaudeCode, text); framed {
		t.Fatal("a slash command was framed as a paste; it would stop opening the command palette")
	}
	if _, _, framed := shouldPaste(chatClaudeCode, "  \n /model "+largeText(pasteThreshold)); framed {
		t.Fatal("leading whitespace defeated the slash guard")
	}
}

// A payload that QUOTES the framing would otherwise end the paste early and
// dump its remainder onto the typed path — the very bug this fixes, re-entered
// through the content.
func TestFramePaste_StripsEmbeddedFraming(t *testing.T) {
	text := "head\x1b[201~middle\x1b[200~tail"
	prefix, suffix := pasteWrapForHarness(chatClaudeCode)
	got := string(framePaste(prefix, suffix, text))
	want := pasteStartCSI200 + "headmiddletail" + pasteEndCSI201
	if got != want {
		t.Fatalf("framePaste = %q, want %q", got, want)
	}
	if strings.Count(got, pasteEndCSI201) != 1 || strings.Count(got, pasteStartCSI200) != 1 {
		t.Fatalf("framePaste left an unbalanced frame: %q", got)
	}
}

// A framed write is rendered as a placeholder, never as the text, so it must
// not wait out bound/2 for a needle that can never match.
func TestAwaitComposerEcho_FramedDoesNotWaitForTheTextNeedle(t *testing.T) {
	c := newEchoConversation(t, chatClaudeCode, 4*time.Second)
	pre := c.screen.Snapshot().Text

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = c.screen.Write([]byte("❯ [Pasted text #1 +42 lines]\r\n"))
	}()

	start := time.Now()
	if err := c.awaitComposerEcho(context.Background(), largeText(pasteThreshold), pre, true); err != nil {
		t.Fatalf("awaitComposerEcho: %v", err)
	}
	if elapsed, half := time.Since(start), c.echoBoundDur()/2; elapsed >= half {
		t.Fatalf("framed echo waited %v (>= bound/2 = %v); the needle was not dropped", elapsed, half)
	}
}

// Unmeasured harnesses keep today's behaviour. That is the point of a
// per-harness table, and it mirrors how submitKeyForHarness treats the unknown.
func TestPasteWrapForHarness_OnlyMeasuredHarnesses(t *testing.T) {
	for _, h := range []string{"claude", chatClaudeCode, "codex"} {
		prefix, suffix := pasteWrapForHarness(h)
		if string(prefix) != pasteStartCSI200 || string(suffix) != pasteEndCSI201 {
			t.Fatalf("pasteWrapForHarness(%q) = (%q, %q), want the CSI 200/201 pair", h, prefix, suffix)
		}
	}
	for _, h := range []string{"pi", "opencode", "generic", ""} {
		if prefix, suffix := pasteWrapForHarness(h); prefix != nil || suffix != nil {
			t.Fatalf("pasteWrapForHarness(%q) = (%q, %q), want nil — unmeasured harnesses keep today's write", h, prefix, suffix)
		}
	}
	// And an unwrapped harness is never framed, however large the payload.
	if _, _, framed := shouldPaste("pi", largeText(4*pasteThreshold)); framed {
		t.Fatal("pi was framed; its composer has never been measured against a large prompt")
	}
}

// The fake must drive the wrapper with exactly the bytes production writes, or
// hermetic scenarios stop proving anything about production.
func TestPasteWrapMatchesFakeharness(t *testing.T) {
	if pasteStartCSI200 != fakeharness.PasteStart || pasteEndCSI201 != fakeharness.PasteEnd {
		t.Fatalf("paste framing drifted: chat has (%q, %q), fakeharness has (%q, %q)",
			pasteStartCSI200, pasteEndCSI201, fakeharness.PasteStart, fakeharness.PasteEnd)
	}
}
