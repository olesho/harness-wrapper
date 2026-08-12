package chat

import (
	"strings"
	"testing"
)

// The confirmation is a screen predicate, so it is unit-testable without a PTY.
// These lock the two mistakes that made the field failure invisible: reading
// the accumulated transcript instead of the live screen, and treating "the
// harness accepted the bytes" as "the harness ran the turn".

func TestComposerHoldsPaste_DetectsTheCollapsedEntry(t *testing.T) {
	// What the composer looked like on eight consecutive silent runs.
	screen := strings.Join([]string{
		"────────────────────────────────────────────",
		"❯ [Pasted text #1 +157 lines]",
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)",
	}, "\n")

	if !composerHoldsPaste(screen) {
		t.Fatal("composerHoldsPaste = false for a screen whose composer is holding a collapsed paste")
	}
}

func TestComposerHoldsPaste_EmptyComposerIsReleased(t *testing.T) {
	// After a successful submit the placeholder is gone from the composer and
	// the reply is on screen.
	screen := strings.Join([]string{
		"⏺ HARNESS_WRAPPER_LARGE_PROMPT_OK",
		"✻ Worked for 4s",
		"❯ ",
	}, "\n")

	if composerHoldsPaste(screen) {
		t.Fatal("composerHoldsPaste = true for a released composer")
	}
}

// A small prompt is never collapsed, so confirmation must be a no-op for it —
// otherwise every ordinary send would pay the retry budget.
func TestComposerHoldsPaste_PlainTypedPromptIsNotAPaste(t *testing.T) {
	screen := "Claude Code\n❯ Reply with exactly: OK\n"

	if composerHoldsPaste(screen) {
		t.Fatal("composerHoldsPaste = true for a normally typed prompt; only a COLLAPSED paste blocks submission")
	}
}

// screenTail is only used to make a failure legible; keep it honest about
// short input so the error message is not misleading.
func TestScreenTail_ShortScreenIsReturnedWhole(t *testing.T) {
	if got := screenTail("  ❯ [Pasted text #1 +2 lines]  ", 200); got != "❯ [Pasted text #1 +2 lines]" {
		t.Fatalf("screenTail = %q, want the trimmed whole string", got)
	}
}

func TestScreenTail_LongScreenKeepsTheEnd(t *testing.T) {
	long := strings.Repeat("x", 50) + "THE-END"
	got := screenTail(long, 10)
	if len(got) != 10 || !strings.HasSuffix(got, "THE-END") {
		t.Fatalf("screenTail = %q, want the last 10 bytes ending in THE-END", got)
	}
}
