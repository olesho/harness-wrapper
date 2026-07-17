package env

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ienv "github.com/olesho/harness-wrapper/internal/env"
	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
)

// TestRunStructuredTurn_RoundTripLocal is the B1-gated host/guest round-trip: it
// drives RunStructuredTurn over B1's LOCAL (non-container) Workspace
// (internal/env.Local, the analog of MH env.Local), pointing the guest runner at
// a freshly built harness-wrapper binary and the INNER turn engine at
// internal/fakeharness via the HARNESS_BINARY_CLAUDE override. It asserts the
// parsed StructuredTurnResult flows all the way back: a COMPLETED turn whose
// reply echoes the uploaded prompt, exit-code-parity with the guest table, and
// the tolerated empty-transcript path (the PTY-replay fake writes no JSONL).
func TestRunStructuredTurn_RoundTripLocal(t *testing.T) {
	wrapperBin := buildHarnessWrapper(t)
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}

	const prompt = "ship the turn API"

	// Scripted fake claude turn: idle → await submit → work → reply echoing the
	// prompt → stay alive until the wrapper stops it (mirrors the guest golden).
	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
		Reply(40, "assistant reply: "+fakeharness.PromptRef(), "Baked", "1s").
		StayAliveUntilStopped().
		Build()
	scriptData, err := json.Marshal(script)
	if err != nil {
		t.Fatalf("marshal script: %v", err)
	}
	scriptPath := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(scriptPath, scriptData, 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}

	// A hermetic Local workspace under a temp root (host == guest).
	prov := ienv.Local(t.TempDir())
	ctx := context.Background()
	ws, err := prov.Create(ctx, ienv.WorkspaceSpec{Name: "structured-turn-roundtrip"})
	if err != nil {
		t.Fatalf("create local workspace: %v", err)
	}
	t.Cleanup(func() { _ = ws.Destroy(context.Background(), ienv.OutcomeSuccess) })

	res, err := RunStructuredTurn(ctx, ws, StructuredTurnConfig{
		Runner:      []string{wrapperBin, "structured-run"},
		Harness:     "claude",
		HarnessArgs: nil,
		Prompt:      prompt,
		Env: map[string]string{
			"HARNESS_BINARY_CLAUDE":       fakeBin,
			fakeharness.EnvVar:            scriptPath,
			"HARNESS_WRAPPER_RUN_TIMEOUT": "30s",
		},
	})
	if err != nil {
		t.Fatalf("RunStructuredTurn: %v", err)
	}

	if res.Status != turnproto.StatusCompleted {
		t.Errorf("status = %q, want %q", res.Status, turnproto.StatusCompleted)
	}
	if got, want := expectedExitCode(res.Status), turnproto.ExitOK; got != want {
		t.Errorf("expectedExitCode(%q) = %d, want %d", res.Status, got, want)
	}
	if !strings.Contains(res.Reply, "assistant reply: "+prompt) {
		t.Errorf("reply = %q, want it to contain %q", res.Reply, "assistant reply: "+prompt)
	}
	// The PTY-replay fake writes no claude-layout JSONL, so the in-guest
	// transcript read errors on the absent session — the tolerated path.
	if len(res.TranscriptEntries) != 0 {
		t.Errorf("transcript_entries = %v, want empty (fake writes no JSONL)", res.TranscriptEntries)
	}
	if res.TranscriptError == "" {
		t.Errorf("transcript_error is empty; want the tolerated absent-session read error recorded")
	}
	if res.WorkingDir == "" {
		t.Errorf("working_dir is empty")
	}
}

// TestRunStructuredTurn_EmptyHarness rejects a config with no harness name before
// touching the workspace.
func TestRunStructuredTurn_EmptyHarness(t *testing.T) {
	if _, err := RunStructuredTurn(context.Background(), nil, StructuredTurnConfig{}); err == nil {
		t.Fatal("want error for empty harness name, got nil")
	}
}

// TestBuildRunnerArgv covers the guest argv shape the structured-run subcommand
// parses back: prompt-file before the wrapper flags, exactly one "--" separator,
// harness args passed through verbatim.
func TestBuildRunnerArgv(t *testing.T) {
	got := buildRunnerArgv(StructuredTurnConfig{
		Runner:      []string{"harness-wrapper", "structured-run"},
		Harness:     "claude",
		HarnessArgs: []string{"--dangerously-skip-permissions"},
		Effort:      "high",
		Model:       "opus",
	}, "/guest/tmp/prompt.txt")

	want := []string{
		"harness-wrapper", "structured-run",
		"--prompt-file", "/guest/tmp/prompt.txt",
		"--effort", "high",
		"--model", "opus",
		"claude", "--",
		"--dangerously-skip-permissions",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("argv =\n  %v\nwant\n  %v", got, want)
	}
}

var (
	wrapperOnce sync.Once
	wrapperPath string
	wrapperErr  error
)

// buildHarnessWrapper compiles cmd/harness-wrapper once per test process — the
// GUEST RUNNER binary RunStructuredTurn execs. Distinct from
// fakeharness.BuildOnce (which builds cmd/fakeharness, the inner turn engine).
func buildHarnessWrapper(t *testing.T) string {
	t.Helper()
	wrapperOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			wrapperErr = err
			return
		}
		dir, err := os.MkdirTemp("", "harness-wrapper-bin")
		if err != nil {
			wrapperErr = err
			return
		}
		wrapperPath = filepath.Join(dir, "harness-wrapper")
		cmd := exec.Command(goBin, "build", "-o", wrapperPath,
			"github.com/olesho/harness-wrapper/cmd/harness-wrapper")
		if out, berr := cmd.CombinedOutput(); berr != nil {
			wrapperErr = &buildError{err: berr, out: out}
		}
	})
	if wrapperErr != nil {
		t.Skipf("harness-wrapper build unavailable: %v", wrapperErr)
	}
	return wrapperPath
}

type buildError struct {
	err error
	out []byte
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + string(e.out) }
