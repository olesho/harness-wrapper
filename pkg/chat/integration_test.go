package chat

import (
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
)

// These tests drive a real fake-harness process over a real PTY through the
// public Open/Send/Events API. They assert the version-independent contract —
// turns complete, the submitted prompt round-trips into the reply verbatim, and
// a mid-turn marker on a non-busy frame does not truncate — rather than any
// specific glyph. The same scenarios can later be pointed at the real installed
// binaries (Layer 4) by swapping BinaryPath and dropping the glyph-specific
// Reason assertions.

// TestIntegration_SubAgentFlicker_DoesNotTruncate regression-locks 3eda8a8 +
// dfc5aae end to end. The script fires an end-of-turn marker on a frame where
// the busy signal has flickered off (a sub-agent redraw), MID-turn, then does
// more work before settling. The old instant-complete code captured the
// pre-final frame; the fix must wait for the settled prompt and return the real
// reply with the submitted sentinel intact.
func TestIntegration_SubAgentFlicker_DoesNotTruncate(t *testing.T) {
	const sentinel = "READY-7Q3X9"

	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().                                                             // capture the submitted prompt
		Working(30, "Cerebrating").                                                // busy
		MarkerFlicker(30, "Pondered", "3s", "drafting").                           // ✻ marker while !Busy, mid-turn ← the trap
		Working(30, "Exploring").                                                  // spinner returns: work continues
		Reply(40, "Final answer: "+fakeharness.PromptRef(), "Synthesized", "12s"). // settled end-of-turn
		Build()

	conv := openFake(t, script)
	sendOneTurn(t, conv, "Reply with exactly: "+sentinel)

	turn := waitForTerminalTurn(t, conv, 4*time.Second)

	if turn.State != TurnStateComplete {
		t.Fatalf("state = %q (reason %q), want complete", turn.State, turn.Reason)
	}
	// The decisive assertion: the sentinel lives ONLY in the final echoed Reply
	// frame. Truncating at the mid-turn flicker (the old bug) would capture the
	// "Pondered"/"drafting" frame, which has no sentinel.
	if !strings.Contains(turn.Text, sentinel) {
		t.Errorf("reply did not round-trip the sentinel %q (turn truncated mid-flight?)\n--- captured text ---\n%s", sentinel, turn.Text)
	}
	if strings.Contains(turn.Text, "Pondered") || strings.Contains(turn.Text, "drafting") {
		t.Errorf("captured a pre-final frame (contains the mid-turn marker/preamble)\n--- captured text ---\n%s", turn.Text)
	}
	if !strings.Contains(turn.Reason, "marker confirmed") {
		t.Errorf("reason = %q, want marker-confirmed completion", turn.Reason)
	}
}

// TestIntegration_MultiTurn confirms turn boundaries are recognized
// independently across a multi-turn conversation: each Send yields exactly one
// completed assistant turn whose reply carries that turn's own sentinel.
func TestIntegration_MultiTurn(t *testing.T) {
	sentinels := []string{"ALPHA-111", "BRAVO-222"}

	b := fakeharness.New("claude-code").Idle()
	for range sentinels {
		b = b.
			AwaitSubmit().
			Working(30, "Working").
			Reply(40, "Echo: "+fakeharness.PromptRef(), "Synthesized", "5s")
	}
	conv := openFake(t, b.Build())

	for i, s := range sentinels {
		sendOneTurn(t, conv, "Say "+s)
		turn := waitForTerminalTurn(t, conv, 4*time.Second)
		if turn.State != TurnStateComplete {
			t.Fatalf("turn %d: state = %q (reason %q), want complete", i, turn.State, turn.Reason)
		}
		if !strings.Contains(turn.Text, s) {
			t.Errorf("turn %d: reply missing sentinel %q\n--- captured text ---\n%s", i, s, turn.Text)
		}
		// The previous turn's sentinel must not leak into this one's reply.
		if i > 0 && strings.Contains(turn.Text, sentinels[i-1]) {
			t.Errorf("turn %d: reply leaked the previous turn's sentinel %q", i, sentinels[i-1])
		}
	}
}

// TestIntegration_NoMarkerFallback completes a turn that NEVER prints an
// end-of-turn marker. Completion must fall back to the idle path, which requires
// prompt-readiness — proving the fallback still works end to end (the safety net
// behind d0d23ba) and is correctly distinguished from the marker path.
func TestIntegration_NoMarkerFallback(t *testing.T) {
	const sentinel = "FALLBACK-42"

	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Thinking").
		Working(30, "Thinking").
		SettleIdle(40, "Done: "+fakeharness.PromptRef()). // no ✻ marker; ready + not busy
		Build()

	conv := openFake(t, script)
	sendOneTurn(t, conv, "Answer with "+sentinel)

	turn := waitForTerminalTurn(t, conv, 4*time.Second)

	if turn.State != TurnStateComplete {
		t.Fatalf("state = %q (reason %q), want complete", turn.State, turn.Reason)
	}
	if !strings.Contains(turn.Text, sentinel) {
		t.Errorf("reply missing sentinel %q\n--- captured text ---\n%s", sentinel, turn.Text)
	}
	if !strings.Contains(turn.Reason, "fallback") {
		t.Errorf("reason = %q, want idle-completion fallback", turn.Reason)
	}
}

// TestIntegration_Codex_TokenUsageCompletesTurn covers the codex completion
// path end to end: a turn completes the instant a fresh "Token usage: …" footer
// appears (no quiescence dance, unlike claude-code), the submitted prompt
// round-trips into the reply, and the session id is scraped from the resume
// hint. It also pins the codex submit contract (CSI 13u) via AwaitSubmit.
func TestIntegration_Codex_TokenUsageCompletesTurn(t *testing.T) {
	const sessionID = "abcdef01-2345-6789-abcd-ef0123456789"
	const sentinel = "CODEX-OK-55"

	script := fakeharness.New("codex").
		Session(sessionID).
		Idle().
		AwaitSubmit().
		CodexWorking(30, "Thinking").
		CodexReply(40, "codex reply: "+fakeharness.PromptRef()).
		Build()

	conv := openFake(t, script)
	sendOneTurn(t, conv, "Reply with "+sentinel)

	turn := waitForTerminalTurn(t, conv, 4*time.Second)

	if turn.State != TurnStateComplete {
		t.Fatalf("state = %q (reason %q), want complete", turn.State, turn.Reason)
	}
	if !strings.Contains(turn.Text, sentinel) {
		t.Errorf("reply did not round-trip the sentinel %q\n--- captured text ---\n%s", sentinel, turn.Text)
	}
	if !strings.Contains(turn.Reason, "Token usage") {
		t.Errorf("reason = %q, want codex Token-usage completion", turn.Reason)
	}
	if conv.SessionID() == "" { // chat-level id always set
		t.Error("conversation session id unexpectedly empty")
	}
}

// TestIntegration_Codex_MultiTurn confirms codex turn boundaries across two
// turns: each turn completes on its OWN Token-usage footer. CodexReply derives
// distinct footers per call, so the second turn's marker is not deduped against
// the first (codex fingerprints by exact footer text).
func TestIntegration_Codex_MultiTurn(t *testing.T) {
	sentinels := []string{"CDX-AA", "CDX-BB"}

	b := fakeharness.New("codex").Idle()
	for range sentinels {
		b = b.AwaitSubmit().CodexWorking(20, "Thinking").CodexReply(30, "echo: "+fakeharness.PromptRef())
	}
	conv := openFake(t, b.Build())

	for i, s := range sentinels {
		sendOneTurn(t, conv, "Say "+s)
		turn := waitForTerminalTurn(t, conv, 4*time.Second)
		if turn.State != TurnStateComplete {
			t.Fatalf("turn %d: state = %q (reason %q), want complete", i, turn.State, turn.Reason)
		}
		if !strings.Contains(turn.Text, s) {
			t.Errorf("turn %d: reply missing sentinel %q\n--- captured text ---\n%s", i, s, turn.Text)
		}
	}
}
