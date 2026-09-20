package harness_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// fakeBin builds the scriptable fake harness (cmd/fakeharness) once per process,
// skipping when the Go toolchain is unavailable. scriptEnv marshals a script to
// a temp file and returns the env that points the fake at it — passed to RunTurn
// via TurnConfig.Env so the one-shot driver spawns the fake over a real PTY and
// submits with CSI 13u (no newline coupling).
func fakeBin(t *testing.T) string {
	t.Helper()
	p, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}
	return p
}

func scriptEnv(t *testing.T, s fakeharness.Script) []string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	p := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return append(os.Environ(), fakeharness.EnvVar+"="+p)
}

func TestRunTurn_ClaudeStyleTurnStopsAfterCompletion(t *testing.T) {
	const sessionID = "123e4567-e89b-12d3-a456-426614174000"
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Session(sessionID).
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
		StayAliveUntilStopped().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    bin,
		Env:           env,
		Prompt:        "ship the turn API",
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	if res.Turn.State != chat.TurnStateComplete {
		t.Fatalf("Turn.State = %q, want complete", res.Turn.State)
	}
	if res.Session.HarnessSessionID != sessionID {
		t.Fatalf("HarnessSessionID = %q", res.Session.HarnessSessionID)
	}
	if len(res.History) < 2 {
		t.Fatalf("History length = %d, want at least user + assistant turns", len(res.History))
	}
	if res.Conversation != nil {
		t.Fatal("Conversation should be nil when ExitAfterTurn is true")
	}
	if !res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = false, want true")
	}
	if res.WrapperResult.Status != wrapper.StatusInterrupted {
		t.Fatalf("raw WrapperResult.Status = %q, want interrupted after intentional stop", res.WrapperResult.Status)
	}
	if !strings.Contains(out.String(), "assistant reply: ship the turn API") {
		t.Fatalf("Output missing assistant reply:\n%s", out.String())
	}
}

func TestRunTurn_ExitAfterTurn_HarnessQuitsCleanly(t *testing.T) {
	const sessionID = "123e4567-e89b-12d3-a456-426614174000"
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Session(sessionID).
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
		QuitsOnQuit().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    bin,
		Env:           env,
		Prompt:        "ship the turn API",
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}

	if res.WrapperResult.Status != wrapper.StatusIdle {
		t.Fatalf("raw WrapperResult.Status = %q, want idle after graceful quit", res.WrapperResult.Status)
	}
	if res.WrapperResult.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.WrapperResult.ExitCode)
	}
	if !res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = false, want true")
	}
	if res.Turn.State != chat.TurnStateComplete {
		t.Fatalf("Turn.State = %q, want complete", res.Turn.State)
	}
}

func TestRunTurn_CanKeepConversationAlive(t *testing.T) {
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply one", "Baked", "1s").
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply two", "Baked", "2s").
		StayAliveUntilStopped().
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    bin,
		Env:           env,
		Prompt:        "first turn",
		ExitAfterTurn: false,
	})
	if err != nil {
		t.Fatalf("RunTurn: %v", err)
	}
	if res.Conversation == nil {
		t.Fatal("Conversation is nil when ExitAfterTurn is false")
	}
	if res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = true, want false for kept conversation")
	}
	defer func() { _ = res.Conversation.Close(context.Background()) }()

	release, err := res.Conversation.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl second turn: %v", err)
	}
	defer release()
	turnID, err := res.Conversation.Send(ctx, "second turn")
	if err != nil {
		t.Fatalf("Send second turn: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case ev := <-res.Conversation.Events():
			if ev.Turn.ID == turnID && ev.Turn.State == chat.TurnStateComplete {
				return
			}
		}
	}
}

func TestRunTurn_ReturnsErrTurnErrored(t *testing.T) {
	bin := fakeBin(t)
	env := scriptEnv(t, fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Exit(2). // crash mid-turn, like a harness that dies after the prompt
		Build())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:    "claude",
		BinaryPath: bin,
		Env:        env,
		Prompt:     "fail this turn",
	})
	if !errors.Is(err, harness.ErrTurnErrored) {
		t.Fatalf("RunTurn err = %v, want ErrTurnErrored", err)
	}
	if res.Turn.State != chat.TurnStateErrored {
		t.Fatalf("Turn.State = %q, want errored", res.Turn.State)
	}
}

// ─── the live (paid) real-Claude tests ───────────────────────────────────────

// realClaudeEnv returns a launch env with Claude Code's nesting markers
// stripped. The dogfood runs from a cron that is itself a Claude Code session;
// inheriting the markers disables session persistence in the spawned claude,
// which removes the swallowed-prompt transcript rescue and turns a lagged
// repaint into a hard ErrTurnErrored. (PUPPET-671)
//
// Call it AFTER any t.Setenv the test needs carried through: it materializes
// the process environment at call time. CLAUDE_CONFIG_DIR is not a nesting
// marker and survives.
//
// This supersedes PUPPET-670's scrubbedRealClaudeEnv in pkg/harness/env_test.go,
// a test-local hand-copy of the policy whose doc comment justified itself with
// "that function is unexported in package main". It no longer is: the policy is
// pkg/harnessenv and cmd/harness-wrapper delegates to it, so the mirror (and the
// divergence risk two hand-kept tables carry) is gone. Its table rows live in
// pkg/harnessenv/harnessenv_test.go.
func realClaudeEnv(t *testing.T) []string {
	t.Helper()
	env := harnessenv.Cleaned()
	assertNoNestingMarkers(t, env)
	return env
}

// assertNoNestingMarkers fails the test if the launch env still carries a
// nesting marker, naming the leak instead of letting it surface 12s later as a
// swallowed-prompt error on a screen nobody kept.
func assertNoNestingMarkers(t *testing.T, env []string) {
	t.Helper()
	for _, kv := range env {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if harnessenv.IsNestingKey(k) {
			t.Fatalf("launch env still carries the Claude Code nesting marker %q: the spawned claude will disable session persistence and the turn can fail as a swallowed prompt", k)
		}
	}
}

// TestRealClaudeEnvIsScrubbed is the hermetic (unpaid, always-run) companion to
// the live tests below: it proves the env they launch with is scrubbed even on
// a machine where they skip, from a process that is itself marked as nested.
func TestRealClaudeEnvIsScrubbed(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-test")

	env := realClaudeEnv(t) // fails here if any marker survives

	if !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test") {
		t.Error("the live launch env lost CLAUDE_CODE_OAUTH_TOKEN; the spawned claude would start unauthenticated (PUPPET-317)")
	}
}

// reportRealClaudeFailure prints the fields that name WHICH failure arm fired.
// The 2026-09-18 release-check artefact could not be diagnosed because the old
// message dropped Turn.Reason and dumped raw escape bytes: the log was a wall
// of ESC[ sequences and the cause had to be reconstructed by reading source.
// (PUPPET-671)
func reportRealClaudeFailure(t *testing.T, what string, err error, res harness.TurnResult, out *bytes.Buffer) {
	t.Helper()
	cause := "(no error)"
	if err != nil {
		cause = err.Error()
	}
	t.Fatalf("%s: %s\n"+
		"turn.State:        %s\n"+
		"turn.Reason:       %s\n"+
		"turn.Code:         %s\n"+
		"harness sessionID: %q\n"+
		"historySource:     %s (%d turn(s))\n"+
		"rendered screen:\n%s\n"+
		"raw output:\n%s",
		what, cause,
		res.Turn.State, res.Turn.Reason, res.Turn.Code,
		res.Session.HarnessSessionID,
		res.HistorySource, len(res.History),
		renderPTY(out.Bytes()),
		out.String())
}

// renderPTY replays the captured PTY bytes through the same terminal emulator
// the adapter reads, at chat.Open's default geometry, so a failure shows the
// SCREEN the verdict was made on. Falls back to the raw bytes if the emulator
// rejects them.
func renderPTY(raw []byte) string {
	const cols, rows = 120, 40 // chat.Open defaults
	scr := screen.New(cols, rows)
	if _, err := scr.Write(raw); err != nil {
		return string(raw)
	}
	return scr.Snapshot().Text
}

func TestRunTurn_RealClaudeDogfood(t *testing.T) {
	if os.Getenv("HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN") != "1" {
		t.Skip("set HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN=1 to run against real Claude Code")
	}
	claudePath := requireRealClaude(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    claudePath,
		Args:          []string{"--dangerously-skip-permissions"},
		Env:           realClaudeEnv(t),
		Prompt:        "Reply with exactly: HARNESS_WRAPPER_RUNTURN_OK",
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err != nil {
		reportRealClaudeFailure(t, "RunTurn real Claude", err, res, &out)
	}
	if res.Turn.State != chat.TurnStateComplete {
		reportRealClaudeFailure(t, "Turn.State = "+string(res.Turn.State)+", want complete", err, res, &out)
	}
	if !strings.Contains(res.Turn.Text, "HARNESS_WRAPPER_RUNTURN_OK") && !strings.Contains(out.String(), "HARNESS_WRAPPER_RUNTURN_OK") {
		reportRealClaudeFailure(t, "real Claude output missing sentinel; turn text:\n"+res.Turn.Text, nil, res, &out)
	}
	if !res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = false, want true")
	}
	// After ExitAfterTurn the process must be stopped and stopped cleanly. WHICH of
	// the two terminal stop statuses appears depends on whether the harness honors
	// the graceful /quit: claude <=2.1.217 ignored it and was SIGTERM'd
	// (interrupted); 2.1.245 exits 0 on it (idle). Both satisfy the contract; only
	// failed/blocked_by_cost/stale/unknown indicate a real problem. Each branch has
	// exact fake-harness coverage above (see TestRunTurn_ClaudeStyleTurnStopsAfterCompletion
	// and TestRunTurn_ExitAfterTurn_HarnessQuitsCleanly).
	switch res.WrapperResult.Status {
	case wrapper.StatusIdle, wrapper.StatusInterrupted:
	default:
		t.Fatalf("raw WrapperResult.Status = %q, want idle or interrupted after intentional stop", res.WrapperResult.Status)
	}
}

func TestRunTurn_RealClaudeDogfoodKeepAlive(t *testing.T) {
	if os.Getenv("HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN") != "1" {
		t.Skip("set HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN=1 to run against real Claude Code")
	}
	claudePath := requireRealClaude(t)

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    claudePath,
		Args:          []string{"--dangerously-skip-permissions"},
		Env:           realClaudeEnv(t),
		Prompt:        "Reply with exactly: HARNESS_WRAPPER_RUNTURN_KEEP_1",
		ExitAfterTurn: false,
		Output:        &out,
	})
	if err != nil {
		reportRealClaudeFailure(t, "RunTurn real Claude keep-alive first turn", err, res, &out)
	}
	if res.Conversation == nil {
		t.Fatal("Conversation is nil when ExitAfterTurn is false")
	}
	defer func() { _ = res.Conversation.Close(context.Background()) }()
	if res.ProcessStoppedAfterTurn {
		t.Fatal("ProcessStoppedAfterTurn = true, want false")
	}

	release, err := res.Conversation.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl second real Claude turn: %v", err)
	}
	defer release()

	turnID, err := res.Conversation.Send(ctx, "Reply with exactly: HARNESS_WRAPPER_RUNTURN_KEEP_2")
	if err != nil {
		reportRealClaudeFailure(t, "Send second real Claude turn", err, res, &out)
	}
	awaitRealClaudeSentinel(ctx, t, res.Conversation, turnID, "HARNESS_WRAPPER_RUNTURN_KEEP_2", &out)
}

// awaitRealClaudeSentinel waits for turnID to complete and asserts the sentinel
// round-tripped, failing the test on timeout or early conversation close.
func awaitRealClaudeSentinel(ctx context.Context, t *testing.T, conv *chat.Conversation, turnID, sentinel string, out *bytes.Buffer) {
	t.Helper()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("second real Claude turn timed out: %v\nrendered screen:\n%s\nraw output:\n%s", ctx.Err(), renderPTY(out.Bytes()), out.String())
		case ev, ok := <-conv.Events():
			if !ok {
				t.Fatalf("conversation closed before second real Claude turn completed\nrendered screen:\n%s\nraw output:\n%s", renderPTY(out.Bytes()), out.String())
			}
			if ev.Turn.ID != turnID || ev.Turn.State != chat.TurnStateComplete {
				continue
			}
			if !strings.Contains(ev.Turn.Text, sentinel) && !strings.Contains(out.String(), sentinel) {
				t.Fatalf("second real Claude output missing sentinel\nturn text:\n%s\nturn.Reason: %s\nrendered screen:\n%s\nraw output:\n%s", ev.Turn.Text, ev.Turn.Reason, renderPTY(out.Bytes()), out.String())
			}
			return
		}
	}
}

func requireRealClaude(t *testing.T) string {
	t.Helper()
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude not found on PATH: %v", err)
	}
	return claudePath
}

// TestRunTurn_RealClaudeLargePromptIntact is the LIVE regression for PUPPET-194:
// a >2KB multi-line prompt must reach the model WHOLE.
//
// It is the only test that can prove the fix. The hermetic fake has no paste
// heuristic — it cannot lose the head of a large write — so the defect exists
// only against a real composer, and the assertion has to read what the MODEL
// saw. The payload's first line asks for the three words after a marker; a
// truncated arrival starts past that line and cannot answer it, so the reply
// itself distinguishes the two outcomes with no ambiguity. testdata carries the
// payload so its SIZE is a fact of the repo rather than of a shell heredoc.
//
// Measured 2026-08-27 on claude-code 2.1.247 (macOS), 10 runs per arm:
// unframed 5/10 intact, framed 10/10. Run it in a loop, not once — the defect
// is probabilistic:
//
//	go test ./pkg/harness/ -run RealClaudeLargePromptIntact -count=10 -v
func TestRunTurn_RealClaudeLargePromptIntact(t *testing.T) {
	if os.Getenv("HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN") != "1" {
		t.Skip("set HARNESS_WRAPPER_REAL_CLAUDE_RUNTURN=1 to run against real Claude Code")
	}
	claudePath := requireRealClaude(t)

	raw, err := os.ReadFile(filepath.Join("testdata", "large_prompt.txt"))
	if err != nil {
		t.Fatalf("read large prompt: %v", err)
	}
	prompt := string(raw)
	// Guard the payload itself: shrink it below the measured cliff and the test
	// would pass for the wrong reason.
	if len(prompt) < 2560 || strings.Count(prompt, "\n") < 30 {
		t.Fatalf("payload is %d bytes / %d lines, want >= 2560 bytes and >= 30 lines to clear the truncation cliff",
			len(prompt), strings.Count(prompt, "\n"))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var out bytes.Buffer
	res, err := harness.RunTurn(ctx, harness.TurnConfig{
		Harness:       "claude",
		BinaryPath:    claudePath,
		Args:          []string{"--dangerously-skip-permissions"},
		Env:           realClaudeEnv(t),
		Prompt:        prompt,
		ExitAfterTurn: true,
		Output:        &out,
	})
	if err != nil {
		reportRealClaudeFailure(t, "RunTurn real Claude large prompt", err, res, &out)
	}
	if res.Turn.State != chat.TurnStateComplete {
		reportRealClaudeFailure(t, "Turn.State = "+string(res.Turn.State)+", want complete", err, res, &out)
	}
	got := strings.ToLower(res.Turn.Text + out.String())
	if !strings.Contains(got, "alpha bravo charlie") {
		t.Fatalf("the model did not echo the words after the HEAD sentinel — the prompt arrived TRUNCATED at the front\nturn text:\n%s\nrendered screen:\n%s",
			res.Turn.Text, renderPTY(out.Bytes()))
	}
}
