package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDeadHWSessions pins the reap policy: a session is reaped only when EVERY
// pane it owns is dead, and only when it carries the hw- prefix. This is the
// half of `reap` that decides what gets killed, so it is tested as a pure
// function over canned `tmux list-panes -F` output rather than against a live
// server.
func TestDeadHWSessions(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "all panes dead is reaped",
			in:   "hw-alpha 1\nhw-alpha 1\n",
			want: []string{"alpha"},
		},
		{
			name: "a single live pane spares the session",
			in:   "hw-beta 1\nhw-beta 0\n",
			want: nil,
		},
		{
			name: "live pane first still spares the session",
			in:   "hw-beta 0\nhw-beta 1\n",
			want: nil,
		},
		{
			name: "sessions without the hw- prefix are never touched",
			in:   "someones-work 1\nhw-gamma 1\n",
			want: []string{"gamma"},
		},
		{
			name: "empty input reaps nothing",
			in:   "",
			want: nil,
		},
		{
			name: "malformed lines are skipped, not guessed at",
			in:   "hw-delta\n\n   \nhw-delta 1 extra\nhw-eps 1\n",
			want: []string{"eps"},
		},
		{
			name: "multiple dead sessions come back sorted",
			in:   "hw-zeta 1\nhw-alpha 1\nhw-live 0\n",
			want: []string{"alpha", "zeta"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deadHWSessions(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("deadHWSessions(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestTmuxReapLeavesLiveSessionAlone runs the real `reap` subcommand against a
// live hw- session and asserts it is a no-op. The dangerous failure mode for a
// janitor is over-reach, so it is the one worth an integration test.
func TestTmuxReapLeavesLiveSessionAlone(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available; skipping integration test")
	}

	socket := privateTmuxSocket(t)
	hwBin := buildHarnessWrapper(t)
	mockBin := buildMockHarness(t)

	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "claude")
	if err := exec.Command("ln", "-s", mockBin, shimPath).Run(); err != nil {
		t.Fatalf("symlink mock as claude: %v", err)
	}
	envPATH := shimDir + ":" + getenvDefault("PATH", "/usr/bin:/bin")
	env := []string{"PATH=" + envPATH, envTmuxSocket + "=" + socket}

	tracePath := filepath.Join(t.TempDir(), "reap.ndjson")
	sessionName := "hwreap"

	spawn := exec.Command(hwBin, "--tmux-session", sessionName, "--trace-file", tracePath, "claude", "--", "--mode", "stuck")
	spawn.Env = appendEnv(env, "HOME="+t.TempDir())
	if out, err := spawn.CombinedOutput(); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}

	reap := exec.Command(hwBin, "reap")
	reap.Env = appendEnv(nil, env...)
	out, err := reap.CombinedOutput()
	if err != nil {
		t.Fatalf("reap: %v\n%s", err, out)
	}
	if strings.Contains(string(out), sessionName) {
		t.Errorf("reap touched the live session %q: %s", sessionName, out)
	}

	if !tmuxSessionExistsOn(t, socket, tmuxSessionPrefix+sessionName) {
		t.Errorf("live session %q was killed by reap", sessionName)
	}
}

// privateTmuxSocket returns a per-test tmux socket name and arranges for the
// whole server on it to be killed at test end.
//
// Every tmux-touching test in this package uses one. It is the mistake that
// caused PUPPET-346 in the first place, seen from the other side: a test that
// mutates the SHARED default server leaves its damage behind whenever cleanup
// does not run (SIGKILL, a reaped test binary, a `go test` timeout). A private
// server can only ever strand itself.
func privateTmuxSocket(t *testing.T) string {
	t.Helper()
	socket := "hwtest-" + strings.ReplaceAll(t.Name(), "/", "-") + "-" + strconv.Itoa(os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	return socket
}

func tmuxSessionExistsOn(t *testing.T, socket, tmuxName string) bool {
	t.Helper()
	return exec.Command("tmux", "-L", socket, "has-session", "-t", tmuxName).Run() == nil
}

func waitForSessionGone(t *testing.T, socket, tmuxName string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !tmuxSessionExistsOn(t, socket, tmuxName) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !tmuxSessionExistsOn(t, socket, tmuxName)
}
