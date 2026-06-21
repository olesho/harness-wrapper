package harness_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/harness"
	_ "github.com/olesho/harness-wrapper/pkg/harness/all" // register built-in profiles (claude)
	"github.com/olesho/harness-wrapper/pkg/transcript"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// mockBin is the freshly-built fake-harness binary, shared across the package's
// tests (set up by TestMain, mirroring pkg/wrapper's harness build).
var mockBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "harness-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	mockBin = filepath.Join(tmp, "mock")
	build := exec.Command("go", "build", "-o", mockBin, "github.com/olesho/harness-wrapper/test/fakeharness/mock")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build mock: %v\n%s", err, out)
		os.Exit(1)
	}
	// run_turn_test.go drives the scriptable fake harness (built lazily via
	// fakeharness.BuildOnce); clean its binary up alongside the mock. os.Exit
	// skips defers, so remove explicitly before exiting.
	code := m.Run()
	fakeharness.Cleanup()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// streamFixture is a scripted Claude stream-json transcript: a system/init
// (session id), an assistant text + tool_use, a user tool_result, and a final
// result line. The mock replays it verbatim; the PTY applies ONLCR (\n→\r\n),
// which the durable tap tolerates.
const streamFixture = `{"type":"system","subtype":"init","session_id":"e2e-sess-9","cwd":"/x"}
{"type":"assistant","session_id":"e2e-sess-9","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"running ls"},{"type":"tool_use","id":"toolu_9","name":"Bash","input":{"command":"ls"}}]}}
{"type":"user","session_id":"e2e-sess-9","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_9","content":[{"type":"text","text":"file.go"}]}]}}
{"type":"result","subtype":"success","session_id":"e2e-sess-9"}
`

// TestRunStreamParseEndToEnd drives the full P3a+P3b chain through a real PTY:
// harness.Run → wrapper.Run → durable OnLine tap → claude.ParseStreamLine →
// authority filter → OnEvent. It is also the StreamParse half of the transport
// spike (review #9): stream-json must survive the PTY parseable.
func TestRunStreamParseEndToEnd(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []transcript.EventEnvelope
	cfg := harness.Config{
		Wrapper: wrapper.Config{
			BinaryPath: mockBin,
			Args:       []string{"--mode", "emit", "--emit-file", fixture},
			Harness:    "claude",
			Stdout:     io.Discard,
		},
		RunID:          "run-e2e",
		TranscriptMode: harness.TranscriptStreamParse,
		OnEvent: func(e transcript.EventEnvelope) error {
			got = append(got, e)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := harness.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("harness.Run: %v", err)
	}
	if res.TranscriptStrategy != "stream" {
		t.Errorf("TranscriptStrategy = %q, want stream", res.TranscriptStrategy)
	}
	if res.HarnessSessionID != "e2e-sess-9" {
		t.Errorf("HarnessSessionID = %q, want e2e-sess-9 (captured from system:init)", res.HarnessSessionID)
	}

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (text + tool_use + tool_result):\n%+v", len(got), got)
	}
	// All envelopes carry the run/harness/session stamps.
	for i, e := range got {
		if e.RunID != "run-e2e" || e.Harness != "claude" || e.HarnessSessionID != "e2e-sess-9" {
			t.Errorf("env %d stamps wrong: %+v", i, e)
		}
		if e.Event.Source != transcript.SourceLive {
			t.Errorf("env %d Source = %q, want live", i, e.Event.Source)
		}
	}
	if got[0].Event.Type != transcript.EventText || got[0].Event.Text != "running ls" {
		t.Errorf("event 0: %+v, want text 'running ls'", got[0].Event)
	}
	if got[1].Event.Type != transcript.EventToolUse || got[1].Event.ToolName != "Bash" || got[1].Event.ToolUseID != "toolu_9" {
		t.Errorf("event 1: %+v, want tool_use Bash/toolu_9", got[1].Event)
	}
	if got[2].Event.Type != transcript.EventToolResult || got[2].Event.ToolUseID != "toolu_9" || got[2].Event.Output == "" {
		t.Errorf("event 2: %+v, want tool_result for toolu_9 with output", got[2].Event)
	}
}

// TestRunHooksEnsuresConfigAndFallsBackToStream drives the Hooks strategy end
// to end through a real PTY. The mock doesn't fire Claude hooks, so the spool
// stays empty and the orchestrator falls back to the buffered live stream —
// proving (a) EnsureConfig wrote the per-worktree settings.json, (b) the spool
// env was set, and (c) the post-exit drain → shadow fallback never loses the
// transcript when hooks don't fire.
func TestRunHooksEnsuresConfigAndFallsBackToStream(t *testing.T) {
	worktree := t.TempDir()
	dir := t.TempDir()
	fixture := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	var got []transcript.EventEnvelope
	cfg := harness.Config{
		Wrapper: wrapper.Config{
			BinaryPath: mockBin,
			Args:       []string{"--mode", "emit", "--emit-file", fixture},
			Harness:    "claude",
			WorkingDir: worktree,
			Stdout:     io.Discard,
		},
		RunID:          "run-hooks",
		TranscriptMode: harness.TranscriptHooks,
		HookCommand:    []string{"/abs/loom", "hooks"},
		OnEvent:        func(e transcript.EventEnvelope) error { got = append(got, e); return nil },
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := harness.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("harness.Run: %v", err)
	}
	// EnsureConfig wrote the per-worktree settings.json with loom's hooks.
	settings, rerr := os.ReadFile(filepath.Join(worktree, ".claude", "settings.json"))
	if rerr != nil {
		t.Fatalf("settings.json not written by EnsureConfig: %v", rerr)
	}
	if !strings.Contains(string(settings), "/abs/loom") || !strings.Contains(string(settings), "HW_EVENT_SPOOL") {
		t.Errorf("settings.json missing loom hook command:\n%s", settings)
	}
	// No hook fired (the mock isn't claude) → fall back to the buffered stream.
	if res.TranscriptStrategy != "stream" {
		t.Errorf("TranscriptStrategy = %q, want stream (hooks fallback)", res.TranscriptStrategy)
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3 (buffered stream flushed on fallback)", len(got))
	}
	if res.HarnessSessionID != "e2e-sess-9" {
		t.Errorf("HarnessSessionID = %q, want e2e-sess-9", res.HarnessSessionID)
	}
}

// TestRunOnDisplayLineReceivesOutput proves the best-effort display callback is
// wired through a real PTY run: every emitted line reaches OnDisplayLine (the
// tiny fixture is well under the queue cap, so nothing is dropped).
func TestRunOnDisplayLineReceivesOutput(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		lines []string
	)
	cfg := harness.Config{
		Wrapper: wrapper.Config{
			BinaryPath: mockBin,
			Args:       []string{"--mode", "emit", "--emit-file", fixture},
			Harness:    "claude",
			Stdout:     io.Discard,
		},
		TranscriptMode: harness.TranscriptOff, // display works regardless of transcript mode
		OnDisplayLine: func(line string) {
			mu.Lock()
			lines = append(lines, line)
			mu.Unlock()
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := harness.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("harness.Run: %v", err)
	}
	if res.DisplayLinesDropped != 0 {
		t.Errorf("DisplayLinesDropped = %d, want 0 for a tiny fixture", res.DisplayLinesDropped)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "e2e-sess-9") || !strings.Contains(joined, "running ls") {
		t.Errorf("OnDisplayLine missing expected output; got %d lines:\n%s", len(lines), joined)
	}
}

// TestRunObservesAPIError confirms harness.Run drains the event stream for the
// out-of-band retry signal a retrying caller needs: the mock emits an API-error
// banner mid-run (then recovers + exits 0), and Run reports SawAPIError.
func TestRunObservesAPIError(t *testing.T) {
	cfg := harness.Config{
		Wrapper: wrapper.Config{
			BinaryPath: mockBin,
			Args:       []string{"--mode", "api-error", "--api-error-recover"},
			Harness:    "claude", // the claude classifier recognizes the API-error banner
			Stdout:     io.Discard,
			// Small idle thresholds so the classifier fires StatusAPIError during
			// the recover pause (the mock goes quiet ~500ms after the banner).
			IdleQuiet:    50 * time.Millisecond,
			IdleClassify: 150 * time.Millisecond,
		},
		TranscriptMode: harness.TranscriptOff,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := harness.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("harness.Run: %v", err)
	}
	if !res.SawAPIError {
		t.Error("SawAPIError = false, want true (mock emitted an API-error banner mid-run)")
	}
}

// TestRunOnActivityFires confirms the activity observer samples Snapshot while
// the harness runs (at least the final on-exit sample fires).
func TestRunOnActivityFires(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		calls int
	)
	cfg := harness.Config{
		Wrapper: wrapper.Config{
			BinaryPath: mockBin,
			Args:       []string{"--mode", "emit", "--emit-file", fixture},
			Harness:    "claude",
			Stdout:     io.Discard,
		},
		TranscriptMode:   harness.TranscriptOff,
		OnActivity:       func(wrapper.Snapshot) { mu.Lock(); calls++; mu.Unlock() },
		ActivityInterval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := harness.Run(ctx, cfg); err != nil {
		t.Fatalf("harness.Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Error("OnActivity never fired (expected at least the final on-exit sample)")
	}
}

// TestRunOffModeEmitsNothing confirms Off mode delivers no events but still
// composes the run (and captures the session id for resume).
func TestRunOffModeEmitsNothing(t *testing.T) {
	dir := t.TempDir()
	fixture := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(fixture, []byte(streamFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	delivered := 0
	cfg := harness.Config{
		Wrapper: wrapper.Config{
			BinaryPath: mockBin,
			Args:       []string{"--mode", "emit", "--emit-file", fixture},
			Harness:    "claude",
			Stdout:     io.Discard,
		},
		TranscriptMode: harness.TranscriptOff,
		OnEvent:        func(transcript.EventEnvelope) error { delivered++; return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := harness.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("harness.Run: %v", err)
	}
	if delivered != 0 {
		t.Errorf("Off mode delivered %d events, want 0", delivered)
	}
	if res.TranscriptStrategy != "none" {
		t.Errorf("TranscriptStrategy = %q, want none", res.TranscriptStrategy)
	}
	// Session id is still captured (resume needs it even with transcript Off).
	if res.HarnessSessionID != "e2e-sess-9" {
		t.Errorf("HarnessSessionID = %q, want e2e-sess-9 (captured even in Off mode)", res.HarnessSessionID)
	}
}
