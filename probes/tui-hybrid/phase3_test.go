// Package tuihybrid is the TUI hybrid's phase-3 probe (FINDINGS.md, sections
// dated 2026-10-09): which hooks claude's interactive TUI fires for a
// subagent, a background command and a tool that needs permission, against
// internal/mockapi, with no account. It ships nothing.
//
// claude runs on a pseudo-terminal as the profile runs it — no -p, a debug
// log — with every hook registered. The hook command is this test binary
// (TestMain), which writes each payload to a file of its own; the
// PermissionRequest hook can block until the probe answers it. The tests skip
// without the binary:
//
//	HW_REAL_CLAUDE=… go test -count=1 -v ./probes/tui-hybrid/
//
// HW_TUI_PROBE_RUNS repeats each scenario (default 1); HW_TUI_PROBE_OUT keeps
// each run's evidence (hooks, debug lines, screen, transcripts) there.
package tuihybrid

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/internal/sessionid"
)

// The hook mode's environment.
const (
	envHookMode = "HW_TUI_PROBE_HOOK" // log, or perm: block for an answer
	envHookDir  = "HW_TUI_PROBE_HOOKDIR"
	envHoldFor  = "HW_TUI_PROBE_HOLD" // how long perm waits for an answer, as a Go duration
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(envHookMode); mode != "" {
		os.Exit(hook(mode))
	}
	os.Exit(m.Run())
}

// hook is the hook command: the payload written to the hook directory and,
// in perm mode, the probe's answer, once it writes one, on stdout.
func hook(mode string) int {
	dir := os.Getenv(envHookDir)
	b, _ := io.ReadAll(os.Stdin)
	name := fmt.Sprintf("%020d-%d.json", time.Now().UnixNano(), os.Getpid())
	_ = os.WriteFile(filepath.Join(dir, "hooks", name), b, 0o600)
	if mode != "perm" {
		return 0
	}
	var p struct {
		Event string `json:"hook_event_name"`
	}
	_ = json.Unmarshal(b, &p)
	if p.Event != "PermissionRequest" {
		return 0
	}
	hold, _ := time.ParseDuration(os.Getenv(envHoldFor))
	answer := filepath.Join(dir, "answer.json")
	for end := time.Now().Add(hold); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		a, err := os.ReadFile(answer) //nolint:gosec // the probe's own directory
		if err == nil {
			_ = os.Remove(answer)
			_, _ = os.Stdout.Write(a)
			_ = os.WriteFile(filepath.Join(dir, "answered"), []byte(time.Now().Format(time.RFC3339Nano)), 0o600)
			return 0
		}
	}
	return 0
}

// events are the hooks registered.
var events = []string{
	"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure",
	"PostToolBatch", "Stop", "StopFailure", "Notification", "PermissionRequest", "PermissionDenied",
	"SubagentStart", "SubagentStop",
}

type opts struct {
	bypass bool
	// hookTimeout is the PermissionRequest hook's timeout, in seconds.
	hookTimeout int
	hold        time.Duration
}

type run struct {
	t      *testing.T
	base   string
	cfg    string
	cwd    string
	sid    string
	cmd    *exec.Cmd
	tty    *os.File
	mu     sync.Mutex
	out    []byte
	t0     time.Time
	exited chan struct{}
}

func realClaude(t *testing.T) string {
	bin := os.Getenv("HW_REAL_CLAUDE")
	if bin == "" {
		t.Skip("HW_REAL_CLAUDE does not name a claude binary")
	}
	return bin
}

func runs() int {
	n, err := strconv.Atoi(os.Getenv("HW_TUI_PROBE_RUNS"))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func launch(t *testing.T, mock *mockapi.Server, o opts) *run {
	t.Helper()
	bin := realClaude(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	r := &run{t: t, base: base, cfg: filepath.Join(base, "cfg"), sid: sessionid.NewUUID(), exited: make(chan struct{})}
	r.cwd = filepath.Join(base, "cwd")
	for _, d := range []string{r.cfg, r.cwd, filepath.Join(base, "hooks")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.cwd, _ = filepath.EvalSymlinks(r.cwd)
	hooks := map[string]any{}
	for _, e := range events {
		h := map[string]any{"type": "command", "command": self}
		if e == "PermissionRequest" && o.hookTimeout > 0 {
			h["timeout"] = o.hookTimeout
		}
		entry := map[string]any{"hooks": []any{h}}
		if strings.HasPrefix(e, "P") && e != "PostToolBatch" {
			entry["matcher"] = "*"
		}
		hooks[e] = []any{entry}
	}
	write := func(name string, v any) {
		b, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(r.cfg, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("settings.json", map[string]any{"hooks": hooks, "skipDangerousModePermissionPrompt": true})
	write(".claude.json", map[string]any{
		"hasCompletedOnboarding": true, "bypassPermissionsModeAccepted": true,
		"projects": map[string]any{r.cwd: map[string]any{"hasTrustDialogAccepted": true}},
	})
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLAUDE") && !strings.HasPrefix(kv, "ANTHROPIC") && !strings.HasPrefix(kv, "HW_") {
			env = append(env, kv)
		}
	}
	mode := "log"
	if !o.bypass {
		mode = "perm"
	}
	env = append(env, "CLAUDE_CONFIG_DIR="+r.cfg, "ANTHROPIC_BASE_URL="+mock.URL(), "ANTHROPIC_AUTH_TOKEN=probe-placeholder",
		"DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "TERM=xterm-256color",
		"CLAUDE_CODE_DISABLE_FILE_CHECKPOINTING=1",
		envHookMode+"="+mode, envHookDir+"="+base, envHoldFor+"="+o.hold.String())
	args := []string{"--session-id", r.sid, "--debug-file", filepath.Join(base, "debug.log"), "--model", "claude-haiku-4-5"}
	if o.bypass {
		args = append(args, "--permission-mode", "bypassPermissions")
	}
	cmd := exec.Command(bin, args...) //nolint:gosec // the probe's claude
	cmd.Dir, cmd.Env = r.cwd, env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	tty, err := pty.StartWithAttrs(cmd, &pty.Winsize{Rows: 50, Cols: 200}, cmd.SysProcAttr)
	if err != nil {
		t.Fatal(err)
	}
	r.cmd, r.tty, r.t0 = cmd, tty, time.Now()
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := tty.Read(buf)
			r.mu.Lock()
			r.out = append(r.out, buf[:n]...)
			r.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { _ = cmd.Wait(); close(r.exited) }()
	t.Cleanup(r.close)
	if _, ok := r.await("SessionStart", 60*time.Second, nil); !ok {
		t.Fatalf("claude never started: %s", r.screen())
	}
	time.Sleep(time.Second)
	return r
}

func (r *run) close() {
	select {
	case <-r.exited:
	default:
		for i := 0; i < 3; i++ {
			_, _ = r.tty.WriteString("\x03")
			select {
			case <-r.exited:
			case <-time.After(500 * time.Millisecond):
			}
		}
		_ = syscall.Kill(-r.cmd.Process.Pid, syscall.SIGKILL)
		<-r.exited
	}
	r.keep()
}

func (r *run) typeLine(text string) {
	_, _ = r.tty.WriteString(text)
	time.Sleep(150 * time.Millisecond)
	_, _ = r.tty.WriteString("\r")
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[@-_]|[\x00-\x08\x0b-\x1f\x7f]`)

// screen is the terminal's output since from, as text.
func (r *run) screenFrom(from int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if from > len(r.out) {
		from = len(r.out)
	}
	return ansi.ReplaceAllString(string(r.out[from:]), "")
}

func (r *run) screen() string { return r.screenFrom(0) }

func (r *run) mark() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.out)
}

// hk is one hook fired.
type hk struct {
	At      time.Duration
	Event   string
	Payload map[string]any
}

func (r *run) hooks() []hk {
	d := filepath.Join(r.base, "hooks")
	ents, _ := os.ReadDir(d)
	var out []hk
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(d, e.Name())) //nolint:gosec // the probe's own directory
		if err != nil {
			continue
		}
		var p map[string]any
		if json.Unmarshal(b, &p) != nil {
			continue
		}
		ns, _ := strconv.ParseInt(strings.SplitN(e.Name(), "-", 2)[0], 10, 64)
		ev, _ := p["hook_event_name"].(string)
		out = append(out, hk{At: time.Unix(0, ns).Sub(r.t0), Event: ev, Payload: p})
	}
	return out
}

// await waits for the nth hook event matching pred.
func (r *run) await(event string, d time.Duration, pred func(map[string]any) bool) (hk, bool) {
	for end := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		for _, h := range r.hooks() {
			if h.Event == event && (pred == nil || pred(h.Payload)) {
				return h, true
			}
		}
		if !time.Now().Before(end) {
			return hk{}, false
		}
	}
}

func (r *run) count(event string) int {
	n := 0
	for _, h := range r.hooks() {
		if h.Event == event {
			n++
		}
	}
	return n
}

var keepDebug = regexp.MustCompile(`\[engine\]|\[onCancel\]|\[Stall\]|agent_|ermission|[Tt]ask[^s]|ackground|Hook [A-Z]|hook.*(fail|timed|cancel)`)

func (r *run) debugLines() []string {
	b, _ := os.ReadFile(filepath.Join(r.base, "debug.log"))
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if keepDebug.MatchString(l) {
			out = append(out, l)
		}
	}
	return out
}

// transcripts lists the session's transcript files, the subagents' too.
func (r *run) transcripts() []string {
	var out []string
	_ = filepath.Walk(filepath.Join(r.cfg, "projects"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".jsonl") {
			rel, _ := filepath.Rel(r.cfg, p)
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// keep writes the run's evidence under HW_TUI_PROBE_OUT.
func (r *run) keep() {
	root := os.Getenv("HW_TUI_PROBE_OUT")
	if root == "" {
		return
	}
	dir := filepath.Join(root, strings.ReplaceAll(r.t.Name(), "/", "_"))
	_ = os.MkdirAll(dir, 0o700)
	var lines []string
	for _, h := range r.hooks() {
		b, _ := json.Marshal(h.Payload)
		lines = append(lines, fmt.Sprintf("%8.3fs %s %s", h.At.Seconds(), h.Event, b))
	}
	_ = os.WriteFile(filepath.Join(dir, "hooks.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "debug.txt"), []byte(strings.Join(r.debugLines(), "\n")+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "screen.txt"), []byte(r.screen()), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "transcripts.txt"), []byte(strings.Join(r.transcripts(), "\n")+"\n"), 0o600)
	_ = exec.Command("cp", "-R", filepath.Join(r.cfg, "projects"), filepath.Join(dir, "projects")).Run() //nolint:gosec // the probe's own paths
}

func (r *run) summary() string {
	var b strings.Builder
	for _, h := range r.hooks() {
		fmt.Fprintf(&b, "%7.3fs %-18s", h.At.Seconds(), h.Event)
		for _, k := range []string{"prompt_id", "agent_id", "agent_type", "tool_name", "tool_use_id", "source", "error"} {
			if v, ok := h.Payload[k]; ok {
				fmt.Fprintf(&b, " %s=%v", k, v)
			}
		}
		if p, ok := h.Payload["prompt"].(string); ok {
			fmt.Fprintf(&b, " prompt=%.60q", p)
		}
		if bt, ok := h.Payload["background_tasks"]; ok {
			j, _ := json.Marshal(bt)
			fmt.Fprintf(&b, " background_tasks=%s", j)
		}
		if tr, ok := h.Payload["tool_response"]; ok {
			j, _ := json.Marshal(tr)
			fmt.Fprintf(&b, " tool_response=%.300s", j)
		}
		b.WriteString("\n")
	}
	return b.String()
}
