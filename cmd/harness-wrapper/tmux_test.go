package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func appendEnv(env []string, kvs ...string) []string {
	out := slices.Clone(env)
	out = append(out, kvs...)
	return out
}

func TestValidSessionName(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"foo", true},
		{"foo-bar_baz", true},
		{"abc123", true},
		{"", false},
		{"foo bar", false},
		{"foo:bar", false},
		{"foo/bar", false},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := validSessionName(tc.in); got != tc.want {
				t.Errorf("validSessionName(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseHarnessWrapperArgs_TmuxSession(t *testing.T) {
	parsed, err := parseHarnessWrapperArgs([]string{
		"--tmux-session", "myrun",
		"--trace-file", "/tmp/t.ndjson",
		"claude", "--", "--print", "hi",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed.TmuxSession != "myrun" {
		t.Errorf("TmuxSession = %q, want %q", parsed.TmuxSession, "myrun")
	}
	if parsed.TmuxChild != "" {
		t.Errorf("TmuxChild should be empty when only --tmux-session is set")
	}
	if parsed.HarnessName != "claude" {
		t.Errorf("HarnessName = %q, want claude", parsed.HarnessName)
	}
	if len(parsed.HarnessArgs) != 2 || parsed.HarnessArgs[0] != "--print" {
		t.Errorf("HarnessArgs = %v, want [--print hi]", parsed.HarnessArgs)
	}
}

func TestParseHarnessWrapperArgs_TmuxSessionAndChildMutuallyExclusive(t *testing.T) {
	_, err := parseHarnessWrapperArgs([]string{
		"--tmux-session", "a", "--tmux-child", "b",
		"claude", "--",
	})
	if err == nil {
		t.Fatal("expected error when both --tmux-session and --tmux-child are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion, got: %v", err)
	}
}

func TestResolveTracePath_DefaultLocation(t *testing.T) {
	got, err := resolveTracePath("", "myrun")
	if err != nil {
		t.Fatalf("resolveTracePath: %v", err)
	}
	want := ".harness-wrapper/sessions/myrun.trace.ndjson"
	if !strings.HasSuffix(got, want) {
		t.Errorf("trace path %q does not end in %q", got, want)
	}
}

func TestResolveTracePath_ExplicitOverride(t *testing.T) {
	got, err := resolveTracePath("/tmp/explicit.ndjson", "ignored")
	if err != nil {
		t.Fatalf("resolveTracePath: %v", err)
	}
	if got != "/tmp/explicit.ndjson" {
		t.Errorf("trace path = %q, want /tmp/explicit.ndjson", got)
	}
}

// TestTmuxRoundTrip exercises spawn -> list -> status -> kill against
// the real tmux binary. The mock harness binary is shimmed onto a
// temporary PATH as "claude" so resolveHarness finds it. Skipped if
// tmux is not available.
func TestTmuxRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available; skipping integration test")
	}

	hwBin := buildHarnessWrapper(t)
	mockBin := buildMockHarness(t)

	// Shim the mock as `claude` on a temp PATH so resolveHarness picks it up.
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "claude")
	if err := exec.Command("ln", "-s", mockBin, shimPath).Run(); err != nil {
		t.Fatalf("symlink mock as claude: %v", err)
	}
	envPATH := shimDir + ":" + getenvDefault("PATH", "/usr/bin:/bin")

	tracePath := filepath.Join(t.TempDir(), "t.ndjson")
	sessionName := "hwtest"
	t.Cleanup(func() {
		cmd := exec.Command(hwBin, "kill", sessionName)
		cmd.Env = appendEnv(cmd.Env, "PATH="+envPATH)
		_ = cmd.Run()
	})

	// Spawn the parent. The harness ("claude") is the mock running in
	// stuck mode so the session stays alive long enough to observe.
	spawn := exec.Command(hwBin,
		"--tmux-session", sessionName,
		"--trace-file", tracePath,
		"claude", "--", "--mode", "stuck",
	)
	spawn.Env = appendEnv(nil, "PATH="+envPATH, "HOME="+t.TempDir())
	out, err := spawn.CombinedOutput()
	if err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "session: "+sessionName) {
		t.Errorf("spawn output missing session line: %s", out)
	}

	// list should include our session.
	listCmd := exec.Command(hwBin, "list")
	listCmd.Env = appendEnv(nil, "PATH="+envPATH)
	listOut, err := listCmd.Output()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(string(listOut), sessionName) {
		t.Errorf("list output %q missing session %q", string(listOut), sessionName)
	}

	// status (text) should mention the session.
	statusCmd := exec.Command(hwBin, "status", sessionName)
	statusCmd.Env = appendEnv(nil, "PATH="+envPATH)
	statusOut, err := statusCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statusOut)
	}
	if !strings.Contains(string(statusOut), sessionName) {
		t.Errorf("status missing session name: %s", statusOut)
	}

	// kill should remove the session.
	killCmd := exec.Command(hwBin, "kill", sessionName)
	killCmd.Env = appendEnv(nil, "PATH="+envPATH)
	if killOut, err := killCmd.CombinedOutput(); err != nil {
		t.Fatalf("kill: %v\n%s", err, killOut)
	}
	listCmd2 := exec.Command(hwBin, "list")
	listCmd2.Env = appendEnv(nil, "PATH="+envPATH)
	listOut2, _ := listCmd2.Output()
	if strings.Contains(string(listOut2), sessionName) {
		t.Errorf("session %q still listed after kill: %s", sessionName, listOut2)
	}
}

// buildHarnessWrapper compiles the cmd/harness-wrapper binary into a
// temp dir and returns the path. Mirrors how testbuild_test.go builds
// the mock harness in pkg/wrapper.
func buildHarnessWrapper(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "harness-wrapper")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = "."
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build harness-wrapper: %v\n%s", err, outBytes)
	}
	return out
}

// buildMockHarness compiles test/fakeharness/mock from the wrapper repo
// root into a temp dir.
func buildMockHarness(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "mock-harness")
	cmd := exec.Command("go", "build", "-o", out, "../../test/fakeharness/mock")
	cmd.Dir = "."
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mock harness: %v\n%s", err, outBytes)
	}
	return out
}
