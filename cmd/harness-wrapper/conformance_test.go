package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/fakeharness"
	"github.com/olesho/harness-wrapper/pkg/turnproto"
)

// Cross-language conformance corpus — guest emit-pairing coverage. The corpus's
// cli/emit_pairing.json freezes status → (exit code, stderr anchor); these tests
// assert the GUEST structured-run runner actually behaves that way for the rows
// not already covered end-to-end by TestStructuredRun_GoldenCompleted
// (completed → 0). See test/conformance/README.md.
//
// The genuinely-new rows: deadline → 124 WITH DeadlineLine on stderr, and
// startup_error → 1 (both driven end-to-end below). The errored → 1 row shares
// the identical emit tail (emitStructured + turnproto.ExitCode(status), with the
// DeadlineLine guard firing only for deadline), pinned at the table level by
// TestConformance_GuestEmitTail against the ONE authority turnproto.ExitCode.

// TestConformance_StartupErrorEmit drives the guest runner into a startup_error
// (empty prompt, no harness spawned — hermetic) and asserts the emit pairing:
// status startup_error, exit 1, and NO DeadlineLine on stderr.
func TestConformance_StartupErrorEmit(t *testing.T) {
	t.Chdir(t.TempDir())
	out, errOut, code := captureStructuredRunStderr(t, "   ", []string{"claude", "--"})

	res, ok := turnproto.ParseLastJSONLine([]byte(out))
	if !ok {
		t.Fatalf("no JSON result line in stdout:\n%s", out)
	}
	if res.Status != turnproto.StatusStartupError {
		t.Errorf("status = %q, want %q", res.Status, turnproto.StatusStartupError)
	}
	if code != turnproto.ExitError {
		t.Errorf("exit code = %d, want %d", code, turnproto.ExitError)
	}
	if strings.Contains(errOut, turnproto.DeadlineLine) {
		t.Errorf("stderr carries DeadlineLine, want it absent for startup_error:\n%s", errOut)
	}
}

// TestConformance_DeadlineEmit drives the guest runner into a deadline (a
// fakeharness that starts a turn but never replies, under a tiny run timeout) and
// asserts the emit pairing: status deadline, exit 124, and DeadlineLine ON
// stderr — the frozen anchor the orchestrator's deadline regex matches.
func TestConformance_DeadlineEmit(t *testing.T) {
	fakeBin, err := fakeharness.BuildOnce()
	if err != nil {
		t.Skipf("fakeharness unavailable: %v", err)
	}

	// A turn that goes Working and stays alive but NEVER replies: the ctx deadline
	// fires and RunTurn returns context.DeadlineExceeded → status deadline.
	script := fakeharness.New("claude-code").
		Idle().
		AwaitSubmit().
		Working(30, "Working").
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

	t.Setenv("HARNESS_BINARY_CLAUDE", fakeBin)
	t.Setenv(fakeharness.EnvVar, scriptPath)
	t.Setenv("HARNESS_WRAPPER_RUN_TIMEOUT", "3s")
	t.Chdir(t.TempDir())

	out, errOut, code := captureStructuredRunStderr(t, "never completes", []string{"claude", "--"})

	res, ok := turnproto.ParseLastJSONLine([]byte(out))
	if !ok {
		t.Fatalf("no JSON result line in stdout:\n%s", out)
	}
	if res.Status != turnproto.StatusDeadline {
		t.Errorf("status = %q, want %q; stdout:\n%s", res.Status, turnproto.StatusDeadline, out)
	}
	if code != turnproto.ExitDeadline {
		t.Errorf("exit code = %d, want %d", code, turnproto.ExitDeadline)
	}
	if !strings.Contains(errOut, turnproto.DeadlineLine) {
		t.Errorf("stderr missing DeadlineLine %q; got:\n%s", turnproto.DeadlineLine, errOut)
	}
}

// TestConformance_GuestEmitTail pins the guest's emit tail — the logic
// runStructuredRun runs for EVERY status — against the corpus emit_pairing.json,
// so the errored row (which is awkward to trigger through the full PTY stack) is
// still asserted to match the ONE authority the guest delegates to. The tail is:
// exit = turnproto.ExitCode(status); DeadlineLine on stderr iff status==deadline.
func TestConformance_GuestEmitTail(t *testing.T) {
	raw, err := os.ReadFile("../../test/conformance/cli/emit_pairing.json")
	if err != nil {
		t.Fatalf("read emit_pairing.json (regenerate with make regen-conformance): %v", err)
	}
	var table map[string]struct {
		ExitCode     int    `json:"exit_code"`
		StderrAnchor string `json:"stderr_anchor"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatalf("unmarshal emit_pairing.json: %v", err)
	}

	for _, s := range []turnproto.TurnStatus{
		turnproto.StatusCompleted,
		turnproto.StatusErrored,
		turnproto.StatusDeadline,
		turnproto.StatusStartupError,
	} {
		row, ok := table[string(s)]
		if !ok {
			t.Errorf("emit_pairing.json missing status %q", s)
			continue
		}
		// The guest's exit for this status IS turnproto.ExitCode(status).
		if got := turnproto.ExitCode(s); got != row.ExitCode {
			t.Errorf("status %q: guest exit turnproto.ExitCode=%d, corpus=%d", s, got, row.ExitCode)
		}
		// The guest prints DeadlineLine iff the status is deadline.
		wantAnchor := ""
		if s == turnproto.StatusDeadline {
			wantAnchor = turnproto.DeadlineLine
		}
		if row.StderrAnchor != wantAnchor {
			t.Errorf("status %q: corpus stderr_anchor=%q, guest emits %q", s, row.StderrAnchor, wantAnchor)
		}
	}
}

// captureStructuredRunStderr is captureStructuredRun's twin that also captures
// stderr — needed to assert the DeadlineLine emit pairing.
func captureStructuredRunStderr(t *testing.T, prompt string, args []string) (stdout, stderr string, code int) {
	t.Helper()

	promptPath := filepath.Join(t.TempDir(), "prompt.txt")
	if err := os.WriteFile(promptPath, []byte(prompt), 0o600); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	stdinFile, err := os.Open(promptPath)
	if err != nil {
		t.Fatalf("open prompt: %v", err)
	}
	defer func() { _ = stdinFile.Close() }()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}

	origStdin, origStdout, origStderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = stdinFile, outW, errW
	defer func() { os.Stdin, os.Stdout, os.Stderr = origStdin, origStdout, origStderr }()

	drain := func(r *os.File, out chan<- string) {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		out <- sb.String()
	}
	outCh, errCh := make(chan string, 1), make(chan string, 1)
	go drain(outR, outCh)
	go drain(errR, errCh)

	code = runStructuredRun(args)

	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = origStdout, origStderr
	stdout = <-outCh
	stderr = <-errCh
	_ = outR.Close()
	_ = errR.Close()
	return stdout, stderr, code
}
