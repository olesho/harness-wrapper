package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startTmuxServer starts a tmux server on socket with a keepalive session and
// exactly env as the starting process's environment. A tmux server snapshots
// that environment as its GLOBAL environment, and every pane inherits it — tmux
// copies only PATH from the client that creates a later session. So this is how
// a test makes the server's view of HW_TMUX_SOCKET differ from the spawner's,
// which is the condition under test.
func startTmuxServer(t *testing.T, socket string, env []string) {
	t.Helper()
	cmd := exec.Command("tmux", "-L", socket, "new-session", "-d", "-s", "keepalive", "sleep", "300")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start tmux server %s: %v\n%s", socket, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
}

// dumpTmuxDiagnostics logs what a failed tmux-lifecycle test needs to be
// diagnosable from its log alone: every pane's session, liveness, exit status
// and command; the server's global environment as panes inherit it; the text of
// each hw- pane; and the trace file.
func dumpTmuxDiagnostics(t *testing.T, socket, tracePath string) {
	t.Helper()
	panes, _ := exec.Command("tmux", "-L", socket, "list-panes", "-a", "-F",
		"#{session_name} pane=#{pane_id} dead=#{pane_dead} status=#{pane_dead_status} pid=#{pane_pid} cmd=#{pane_start_command}").CombinedOutput()
	t.Logf("tmux -L %s panes:\n%s", socket, panes)
	genv, _ := exec.Command("tmux", "-L", socket, "show-environment", "-g").CombinedOutput()
	var relevant []string
	for _, line := range strings.Split(string(genv), "\n") {
		if strings.HasPrefix(line, "HW_") || strings.HasPrefix(line, "-HW_") || strings.HasPrefix(line, "TMUX") {
			relevant = append(relevant, line)
		}
	}
	t.Logf("tmux -L %s global env (HW_*/TMUX*): %q", socket, relevant)
	for _, line := range strings.Split(string(panes), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], tmuxSessionPrefix) {
			continue
		}
		text, _ := exec.Command("tmux", "-L", socket, "capture-pane", "-p", "-t", strings.TrimPrefix(fields[1], "pane=")).CombinedOutput()
		t.Logf("pane %s text:\n%s", fields[1], text)
	}
	if b, err := os.ReadFile(tracePath); err == nil {
		t.Logf("trace %s:\n%s", tracePath, b)
	} else {
		t.Logf("trace %s: %v", tracePath, err)
	}
}

// waitForLastTraceKind polls tracePath until its LAST event is kind.
func waitForLastTraceKind(t *testing.T, tracePath, kind string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if last, err := readLastTraceEvent(tracePath); err == nil && last != nil && last["kind"] == kind {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestTmuxChildTeardownTargetsItsOwnServer is the deterministic form of the
// TestTmuxShortRunSurvivesRemainOnExit CI failure (run 34675765868: session
// still alive after 15s).
//
// The in-pane child tears its own session down by socket NAME, read from
// HW_TMUX_SOCKET. That variable reaches a pane only through the server's
// global environment, which is snapshotted when the server starts — the
// spawner's own HW_TMUX_SOCKET never crosses. On a server started without it
// (the hostile test's private server) the child's kill-session goes to the
// default socket and misses, and only the parent's per-session remain-on-exit
// off, set AFTER new-session returns, stands between a short run and a
// retained dead pane. When the child exits first, the session strands.
//
// Here that ordering is fixed rather than raced: the server's environment
// names a DIFFERENT socket (a stale value), the child is started as the pane
// command with no parent at all, so no per-session option is ever set, and
// remain-on-exit is on globally. A decoy server on the stale socket holds a
// session with the same name, to catch a teardown aimed at the wrong server.
func TestTmuxChildTeardownTargetsItsOwnServer(t *testing.T) {
	tmuxBin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not available; skipping integration test")
	}
	const sessionName = "hwowner"
	socket := privateTmuxSocket(t)
	decoy := socket + "-decoy"
	// The child runs tmux by name for its teardown, so its PATH must reach it.
	sysPATH := filepath.Dir(tmuxBin) + ":/usr/bin:/bin"
	base := []string{"PATH=" + sysPATH, "HOME=" + t.TempDir()}

	// Stale server environment: it names the decoy socket.
	startTmuxServer(t, socket, appendEnv(base, envTmuxSocket+"="+decoy))
	if out, err := exec.Command("tmux", "-L", socket, "setw", "-g", "remain-on-exit", "on").CombinedOutput(); err != nil {
		t.Fatalf("set remain-on-exit on: %v\n%s", err, out)
	}
	startTmuxServer(t, decoy, base)
	if out, err := exec.Command("tmux", "-L", decoy, "new-session", "-d", "-s", tmuxSessionPrefix+sessionName, "sleep", "300").CombinedOutput(); err != nil {
		t.Fatalf("decoy session: %v\n%s", err, out)
	}

	hwBin := buildHarnessWrapper(t)
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, "claude"), []byte("#!/bin/sh\necho fake claude\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	tracePath := filepath.Join(t.TempDir(), "owner.trace.ndjson")

	// The child exactly as runTmuxSpawn would start it, but with no parent: the
	// per-session remain-on-exit off never happens.
	argv := tmuxReexecArgv(harnessWrapperArgs{TmuxSession: sessionName, HarnessName: "claude"}, hwBin, tracePath)
	start := exec.Command("tmux", append([]string{"-L", socket, "new-session", "-d", "-s", tmuxSessionPrefix + sessionName}, argv...)...)
	start.Env = appendEnv(base[1:], "PATH="+shimDir+":"+sysPATH) // PATH is the one variable tmux copies from the client
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start child session: %v\n%s", err, out)
	}

	if !waitForLastTraceKind(t, tracePath, "wrapper_cli_exited", 15*time.Second) {
		dumpTmuxDiagnostics(t, socket, tracePath)
		t.Fatal("child never finished its run (no wrapper_cli_exited in the trace)")
	}
	if !waitForSessionGone(t, socket, tmuxSessionPrefix+sessionName, 10*time.Second) {
		dumpTmuxDiagnostics(t, socket, tracePath)
		t.Errorf("session %s outlived its finished child on its own server", tmuxSessionPrefix+sessionName)
	}
	if !tmuxSessionExistsOn(t, decoy, tmuxSessionPrefix+sessionName) {
		t.Errorf("the child killed %s on the DECOY server %q; a teardown must only touch the server it runs on", tmuxSessionPrefix+sessionName, decoy)
	}
	last, err := readLastTraceEvent(tracePath)
	if err != nil || last == nil || last["kind"] != "wrapper_cli_exited" {
		b, _ := json.Marshal(last)
		t.Errorf("final trace event = %s (err %v), want wrapper_cli_exited flushed before the teardown", b, err)
	}
}
