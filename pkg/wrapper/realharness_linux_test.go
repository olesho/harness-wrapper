//go:build linux

package wrapper_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// These smoke tests run a real, pinned harness binary through its built-in
// profile. They use no credentials and prove only that the harness starts,
// and runs a tool, inside its profile's domain; the authenticated conformance
// runs that activate a profile are a separate step. Each is skipped unless its
// variable names the binary.

// realHarnessSetup activates the built-in profiles for one test and isolates
// managed state.
func realHarnessSetup(t *testing.T, env string) string {
	t.Helper()
	bin := os.Getenv(env)
	if bin == "" {
		t.Skipf("set %s to run this smoke test against a real harness", env)
	}
	if _, err := landlock.Probe(); err != nil {
		t.Fatalf("Landlock ABI 9 unavailable: %v", err)
	}
	t.Cleanup(contain.ActivateProfilesForTest())
	// A short state root: claude refuses a private TMPDIR over 79 bytes, and
	// t.TempDir's paths are longer.
	state, err := os.MkdirTemp("", "hw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	t.Setenv("XDG_STATE_HOME", state)
	return bin
}

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b[\]P_^][^\x07\x1b]*(\x07|\x1b\\)|\x1b.`)

// screenText is the harness output with terminal control sequences removed.
func screenText(out *lockedBuffer) string { return ansiSeq.ReplaceAllString(out.String(), " ") }

func waitForText(t *testing.T, out *lockedBuffer, text string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(screenText(out), text) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s := screenText(out)
	t.Fatalf("no %q within %s; output tail:\n%s", text, d, s[max(0, len(s)-3000):])
}

func stopSession(t *testing.T, s *wrapper.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.Stop(ctx)
	waitOrFail(t, s, 30*time.Second)
}

// checkCleanup: under supervision the session's cgroup and private state are
// gone once Wait returns.
func checkCleanup(t *testing.T, s *wrapper.Session) {
	t.Helper()
	a := s.Containment()
	if a.Supervision.Mode != containment.SupervisionCgroup {
		t.Logf("supervision none: cleanup %s", a.Supervision.Cleanup)
		return
	}
	if a.Supervision.Cleanup != "complete" {
		t.Errorf("cleanup: %s", a.Supervision.Cleanup)
	}
	if _, err := os.Stat(a.State.Home); !os.IsNotExist(err) {
		t.Errorf("private state survived: %v", err)
	}
}

// TestRealClaudeContained starts the pinned claude-code (HW_REAL_CLAUDE: the
// 2.1.270 binary whose SHA-256 the profile pins) with every TCP connect denied
// and a placeholder token, so nothing reaches the network. The TUI must reach
// its composer from the seeded private state; claude's `!` bash mode then runs
// a command inside the domain without the model: a write in the working
// directory succeeds and a write outside every grant fails.
func TestRealClaudeContained(t *testing.T) {
	bin := realHarnessSetup(t, "HW_REAL_CLAUDE")
	wd, outside := t.TempDir(), t.TempDir()
	out, rec := &lockedBuffer{}, &traceRecorder{}
	s, err := wrapper.Start(context.Background(), wrapper.Config{
		Harness:    "claude-code",
		BinaryPath: bin,
		WorkingDir: wd,
		Stdout:     out,
		Trace:      rec,
		WaitDelay:  time.Second,
		Env: []string{
			"PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8",
			"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-placeholder-not-a-credential",
		},
		Containment: &wrapper.Containment{Kind: wrapper.ContainmentLandlock, RestrictTCP: true},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := s.Containment()
	t.Logf("profile %s v%d, ABI %d, supervision %s, fingerprint %s", a.Profile, a.ProfileVersion, a.ABI, a.Supervision.Mode, a.Fingerprint)
	if _, err := os.Stat(filepath.Join(a.State.HarnessState, ".claude.json")); err != nil {
		t.Errorf("no seeded .claude.json in the private CLAUDE_CONFIG_DIR: %v", err)
	}

	waitForText(t, out, "for shortcuts", 90*time.Second)
	cmd := "touch inside.txt; touch " + filepath.Join(outside, "outside.txt") + "; echo contained-$?"
	for _, in := range []string{"!", cmd, "\r"} {
		if _, err := s.WriteStdin([]byte(in)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitForText(t, out, "contained-1", 30*time.Second)
	if _, err := os.Stat(filepath.Join(wd, "inside.txt")); err != nil {
		t.Errorf("the bash-mode write inside the working directory failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "outside.txt")); err == nil {
		t.Error("a bash-mode write outside every grant succeeded")
	}
	stopSession(t, s)
	checkCleanup(t, s)
}

// fakeResponses is a minimal OpenAI Responses API: each POST answers with the
// next scripted output item (a tool call, then a message), as server-sent
// events.
func fakeResponses(t *testing.T, items []map[string]any) *httptest.Server {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		i := int(n.Add(1) - 1)
		item := map[string]any{
			"type": "message", "role": "assistant", "id": "msg_done",
			"content": []map[string]any{{"type": "output_text", "text": "done"}},
		}
		if i < len(items) {
			item = items[i]
		}
		id := "resp_" + strconv.Itoa(i)
		usage := map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": id}},
			{"type": "response.output_item.done", "item": item},
			{"type": "response.completed", "response": map[string]any{"id": id, "usage": usage}},
		} {
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
		}
	}))
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// TestRealCodexContained runs the pinned codex (HW_REAL_CODEX: the npm shim
// of an @openai/codex 0.144.5 package, with node on PATH) under its profile at
// the bypass rung, against an in-process fake model provider on a loopback
// port, which is the only TCP connect allowed. The fake asks for one
// shell_command call — the pipe-only tool the seeded config selects — and
// codex runs it inside the domain: a write in the working directory succeeds
// and a write outside every grant fails.
func TestRealCodexContained(t *testing.T) {
	bin := realHarnessSetup(t, "HW_REAL_CODEX")
	wd, outside := t.TempDir(), t.TempDir()
	marker := filepath.Join(wd, "tool-output.txt")
	cmd := "touch inside.txt; touch " + filepath.Join(outside, "outside.txt") + "; echo contained-$? > " + marker
	args, _ := json.Marshal(map[string]string{"command": cmd})
	srv := fakeResponses(t, []map[string]any{
		{"type": "function_call", "id": "fc_0", "call_id": "call_0", "name": "shell_command", "arguments": string(args)},
	})
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	// The shim's `#!/usr/bin/env node` resolves node on this PATH, and the
	// profile grants the node it finds there.
	path := filepath.Dir(bin) + ":/usr/bin:/bin"
	if node, err := exec.LookPath("node"); err == nil {
		path = filepath.Dir(node) + ":" + path
	}
	out, rec := &lockedBuffer{}, &traceRecorder{}
	s, err := wrapper.Start(context.Background(), wrapper.Config{
		Harness:    "codex",
		BinaryPath: bin,
		Args: []string{
			"exec", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check",
			"-c", `model_provider="fake"`,
			"-c", `model_providers.fake={name="fake",base_url="` + srv.URL + `/v1",wire_api="responses",env_key="FAKE_API_KEY",request_max_retries=0,stream_max_retries=0,supports_websockets=false}`,
			"-c", `model="fake-model"`,
			"run the probe",
		},
		WorkingDir: wd,
		Stdout:     out,
		Trace:      rec,
		WaitDelay:  time.Second,
		Env: []string{
			"PATH=" + path, "TERM=xterm-256color", "LANG=C.UTF-8",
			"FAKE_API_KEY=placeholder-not-a-credential",
		},
		Containment: &wrapper.Containment{
			Kind: wrapper.ContainmentLandlock, RestrictTCP: true, ConnectTCP: []uint16{uint16(port)},
			PassEnv: []string{"FAKE_API_KEY"},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	a := s.Containment()
	t.Logf("profile %s v%d, ABI %d, supervision %s, fingerprint %s", a.Profile, a.ProfileVersion, a.ABI, a.Supervision.Mode, a.Fingerprint)
	if _, err := os.Stat(filepath.Join(a.State.HarnessState, "config.toml")); err != nil {
		t.Errorf("no seeded config.toml in the private CODEX_HOME: %v", err)
	}
	res := waitOrFail(t, s, 120*time.Second)
	b, err := os.ReadFile(marker)
	if err != nil {
		s := screenText(out)
		t.Fatalf("the tool call did not run (%v, result %+v); output tail:\n%s", err, res, s[max(0, len(s)-3000):])
	}
	if got := strings.TrimSpace(string(b)); got != "contained-1" {
		t.Errorf("tool command reported %q, want contained-1 (the outside write denied)", got)
	}
	if _, err := os.Stat(filepath.Join(wd, "inside.txt")); err != nil {
		t.Errorf("the tool's write inside the working directory failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "outside.txt")); err == nil {
		t.Error("the tool's write outside every grant succeeded")
	}
	checkCleanup(t, s)
}
