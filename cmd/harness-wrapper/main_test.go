package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunHarnessWrapper_TraceIncludesCLIExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake harness is Unix-only")
	}

	shimDir := t.TempDir()
	claudePath := filepath.Join(shimDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\necho fake claude\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))

	tracePath := filepath.Join(t.TempDir(), "trace.ndjson")
	code := runHarnessWrapper([]string{
		"--trace-file", tracePath,
		"claude", "--",
	})
	if code != 0 {
		t.Fatalf("runHarnessWrapper exit code = %d, want 0", code)
	}

	last := readLastTraceKind(t, tracePath)
	if last["kind"] != "wrapper_cli_exited" {
		t.Fatalf("last trace kind = %v, want wrapper_cli_exited; event=%v", last["kind"], last)
	}
	fields, ok := last["fields"].(map[string]any)
	if !ok {
		t.Fatalf("last trace fields missing/wrong type: %v", last)
	}
	if fields["status"] != "idle" {
		t.Fatalf("status = %v, want idle; fields=%v", fields["status"], fields)
	}
}

func TestRunHarnessWrapper_TraceIncludesSignalAndCLIExitOnSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix signals are required")
	}

	hwBin := buildHarnessWrapper(t)
	shimDir := t.TempDir()
	claudePath := filepath.Join(shimDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\ntrap 'exit 0' TERM\necho fake claude ready\nwhile true; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}

	tracePath := filepath.Join(t.TempDir(), "trace.ndjson")
	cmd := exec.Command(hwBin, "--trace-file", tracePath, "claude", "--")
	cmd.Env = appendEnv(nil, "PATH="+shimDir+":"+os.Getenv("PATH"))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start harness-wrapper: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	waitForTraceKind(t, tracePath, "pty_opened")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal harness-wrapper: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("harness-wrapper did not exit after SIGTERM")
	}

	events := readTraceEvents(t, tracePath)
	if !hasTraceKind(events, "wrapper_cli_signal") {
		t.Fatalf("trace missing wrapper_cli_signal: %v", events)
	}
	last := events[len(events)-1]
	if last["kind"] != "wrapper_cli_exited" {
		t.Fatalf("last trace kind = %v, want wrapper_cli_exited; event=%v", last["kind"], last)
	}
	fields, ok := last["fields"].(map[string]any)
	if !ok {
		t.Fatalf("last trace fields missing/wrong type: %v", last)
	}
	if fields["status"] != "interrupted" {
		t.Fatalf("status = %v, want interrupted; fields=%v", fields["status"], fields)
	}
}

// TestRunHarnessWrapper_RejectsSandboxDefaults freezes the passthrough policy:
// --sandbox-defaults is a run/structured-run toggle, and the default
// passthrough mode rejects it explicitly (exit 2 + a named error) instead of
// silently ignoring it. The check precedes resolveHarness, so the test is
// hermetic — no `claude` on PATH, no HARNESS_BINARY_CLAUDE override — and it
// precedes the tmux branch, so the tmux variant needs no session inspection:
// rejection fires before any session could be spawned.
func TestRunHarnessWrapper_RejectsSandboxDefaults(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"passthrough", []string{"--sandbox-defaults", "claude", "--"}},
		{"tmux passthrough", []string{"--tmux-session", "X", "--sandbox-defaults", "claude", "--"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr, code := captureRunStderr(t, tc.args)
			if code != 2 {
				t.Errorf("exit code = %d, want 2; stderr:\n%s", code, stderr)
			}
			const want = "--sandbox-defaults is only supported by run and structured-run"
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, want)
			}
		})
	}
}

// TestRunHarnessWrapper_AllowsPermissionMode freezes the OTHER half of the
// passthrough policy: --permission-mode is NOT rejected the way
// --sandbox-defaults is, bypass rungs included. The divergence is a consent
// argument — --permission-mode bypassPermissions is argv the user could type at
// `claude` themselves (passthrough already forwards --effort / --model on the
// same footing), and without IS_SANDBOX=1 claude still shows the Bypass
// Permissions acceptance screen, which passthrough has no machinery to answer,
// so the human consents in their own terminal. --sandbox-defaults is rejected
// precisely because its env half removes that step and enables root.
//
// Asserted hermetically: with no `claude` on PATH the invocation gets PAST the
// point where the sandbox-defaults rejection would have fired and dies in
// resolveHarness instead. Both exit 2, so the stderr text — not the code — is
// what distinguishes "accepted the flag" from "rejected the flag".
func TestRunHarnessWrapper_AllowsPermissionMode(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HARNESS_BINARY_CLAUDE", "")
	t.Setenv("HARNESS_BINARY", "")
	for _, mode := range []string{"bypass", "bypassPermissions", "manual"} {
		t.Run(mode, func(t *testing.T) {
			stderr, code := captureRunStderr(t, []string{"--permission-mode", mode, "claude", "--"})
			if code != 2 {
				t.Errorf("exit code = %d, want 2 (resolveHarness failure); stderr:\n%s", code, stderr)
			}
			if !strings.Contains(stderr, "not found in PATH") {
				t.Errorf("expected the run to reach resolveHarness (proving the flag was accepted); stderr = %q", stderr)
			}
			if strings.Contains(stderr, "--sandbox-defaults") {
				t.Errorf("--permission-mode must not be rejected by the sandbox-defaults policy; stderr = %q", stderr)
			}
			if strings.Contains(stderr, "--permission-mode") {
				t.Errorf("--permission-mode must not be rejected by passthrough at all; stderr = %q", stderr)
			}
		})
	}
}

// TestRunHarnessWrapper_SandboxDefaultsWithModeReportsComposition pins the
// ORDERING between the two exit-2 rejections. parseHarnessWrapperArgs runs
// first, so `--sandbox-defaults --permission-mode manual claude --` reports the
// COMPOSITION error, not the frozen "only supported by run and structured-run"
// message. Harmless — both are exit 2 and both are accurate — but asserted so
// nobody later "fixes" the ordering and silently changes which diagnostic a
// user sees.
func TestRunHarnessWrapper_SandboxDefaultsWithModeReportsComposition(t *testing.T) {
	stderr, code := captureRunStderr(t, []string{"--sandbox-defaults", "--permission-mode", "manual", "claude", "--"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr:\n%s", code, stderr)
	}
	const want = "--sandbox-defaults is incompatible with --permission-mode manual"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
	if strings.Contains(stderr, "only supported by run and structured-run") {
		t.Errorf("expected the composition error, got the sandbox-defaults-mode one: %q", stderr)
	}
}

// captureRunStderr runs run(args) with os.Stderr captured, returning the
// stderr text and the exit code.
func captureRunStderr(t *testing.T, args []string) (string, int) {
	t.Helper()
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = wp
	defer func() { os.Stderr = orig }()

	code := run(args)

	_ = wp.Close()
	os.Stderr = orig
	data, err := io.ReadAll(rp)
	if err != nil {
		t.Fatalf("read stderr pipe: %v", err)
	}
	_ = rp.Close()
	return string(data), code
}

func readLastTraceKind(t *testing.T, path string) map[string]any {
	t.Helper()
	events := readTraceEvents(t, path)
	if len(events) == 0 {
		t.Fatal("trace is empty")
	}
	return events[len(events)-1]
}

func waitForTraceKind(t *testing.T, path, kind string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if hasTraceKind(readTraceEventsAllowMissing(t, path), kind) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for trace kind %q", kind)
}

func hasTraceKind(events []map[string]any, kind string) bool {
	for _, event := range events {
		if event["kind"] == kind {
			return true
		}
	}
	return false
}

func readTraceEvents(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open trace: %v", err)
	}
	defer func() { _ = f.Close() }()

	var events []map[string]any
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			var event map[string]any
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				t.Fatalf("parse trace event: %v\n%s", err, line)
			}
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan trace: %v", err)
	}
	return events
}

func readTraceEventsAllowMissing(t *testing.T, path string) []map[string]any {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("stat trace: %v", err)
	}
	return readTraceEvents(t, path)
}
