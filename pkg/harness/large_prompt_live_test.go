package harness_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
)

// Live regression test for the large-prompt submit failure.
//
// Field report (2026-08-12, loom's daemon): a planning agent whose prompt was
// 6304 bytes / 157 lines produced ZERO assistant turns and ZERO tokens on eight
// consecutive runs, exiting 0 in ~45s each time. A sibling agent in the same
// container, same binary, same harness, whose prompt was 81 bytes, worked
// normally (26-40 turns, ~5k tokens). The agent's terminal log showed the
// composer holding
//
//	❯ [Pasted text #1 +157 lines]
//
// i.e. the prompt arrived but was never submitted: Claude Code sees a large
// single-write burst as a bracketed paste, and the submit key appended to that
// same write is absorbed as paste CONTENT rather than acted on as a keypress.
//
// The turn then "completes" with nothing in it, which is indistinguishable from
// a successful empty turn to every caller.
//
// The prompt below is deliberately over the collapse threshold. It costs one
// cheap-model turn to run.
func TestRunTurn_RealClaude_LargePromptIsSubmitted(t *testing.T) {
	if os.Getenv("HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN") != "1" {
		t.Skip("set HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN=1 to run against real Claude Code")
	}
	claudePath := requireRealClaude(t)

	const sentinel = "HARNESS_WRAPPER_LARGE_PROMPT_OK"
	prompt := buildLargePrompt(sentinel, 180)
	if lines := strings.Count(prompt, "\n") + 1; lines < 150 {
		t.Fatalf("test prompt is only %d lines; it must exceed the paste-collapse threshold", lines)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:    "claude",
		BinaryPath: claudePath,
		Args:       []string{"--dangerously-skip-permissions"},
		// The cheapest model available: this asserts a submit contract, not
		// reasoning quality, so the model only has to echo one token.
		Model:         cheapClaudeModel(),
		Prompt:        prompt,
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err != nil {
		t.Fatalf("RunTurn with a large prompt: %v\noutput:\n%s", err, tail(out.String(), 2000))
	}
	if res.Turn.State != chat.TurnStateComplete {
		t.Fatalf("Turn.State = %q, want complete\nreason: %s\noutput:\n%s",
			res.Turn.State, res.Turn.Reason, tail(out.String(), 2000))
	}

	// The failure this test exists for is a turn that completes EMPTY: the
	// prompt sat in the composer, the model never ran. Assert on content, not
	// on the turn's own bookkeeping.
	if strings.TrimSpace(res.Turn.Text) == "" && !strings.Contains(out.String(), sentinel) {
		t.Fatalf("large prompt produced an empty turn — it was never submitted (composer still holding it?)\noutput:\n%s",
			tail(out.String(), 2000))
	}
	if !strings.Contains(res.Turn.Text, sentinel) && !strings.Contains(out.String(), sentinel) {
		t.Fatalf("large prompt turn is missing the sentinel\nturn text:\n%s\noutput:\n%s",
			res.Turn.Text, tail(out.String(), 2000))
	}
	// NOTE: do NOT assert on "[Pasted text #" in Output. Output is the whole
	// accumulated terminal stream, and the placeholder legitimately appears
	// there the moment the paste renders — including on runs that then submit
	// correctly. Only the CURRENT screen can distinguish "still holding it"
	// from "held it briefly, then sent it", which is why the confirmation lives
	// in chat.awaitComposerReleased against the live snapshot rather than here.
}

// cheapClaudeModel names the model these live tests run on. They assert
// transport behaviour — did the prompt reach the model at all — so the cheapest
// tier is the correct one, and it keeps the suite runnable in CI.
func cheapClaudeModel() string {
	if m := strings.TrimSpace(os.Getenv("HARNESS_WRAPPER_TEST_MODEL")); m != "" {
		return m
	}
	return "haiku"
}

// buildLargePrompt returns a prompt with at least `lines` lines whose ONLY
// instruction is to echo the sentinel. The bulk is inert filler: the point is
// the size of the write, not the difficulty of the request, so a cheap model
// answers it in one short turn.
func buildLargePrompt(sentinel string, lines int) string {
	var b strings.Builder
	b.WriteString("Reply with exactly this and nothing else: " + sentinel + "\n\n")
	b.WriteString("Ignore everything below; it is padding that makes this prompt large enough\n")
	b.WriteString("to be treated as a paste by the terminal UI.\n\n")
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&b, "padding line %03d: lorem ipsum dolor sit amet, consectetur adipiscing elit\n", i)
	}
	b.WriteString("\nRemember: reply with exactly " + sentinel + "\n")
	return b.String()
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
