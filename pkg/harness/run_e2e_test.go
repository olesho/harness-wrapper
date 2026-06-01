package harness_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
	defer os.RemoveAll(tmp)

	mockBin = filepath.Join(tmp, "mock")
	build := exec.Command("go", "build", "-o", mockBin, "github.com/olesho/harness-wrapper/test/fakeharness/mock")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build mock: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
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
