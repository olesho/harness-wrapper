// Package pirpc probes pi's RPC mode (pi --mode rpc) for the Harness Adapter's
// Pi profile: what a client learns from pi about each input it sends — its
// receipt, the end and outcome of its run, an interrupt, a retry — and what
// pi's session file holds, after a crash too. It speaks pi's RPC itself, with
// no adapter between, against the mock model API. Each test asserts the
// behaviour the profile is built on (FINDINGS.md), so a pi that behaves
// otherwise fails here first.
package pirpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
)

// Keys the probe gives pi: shaped like the real ones, accepted by the mock.
const (
	anthropicKey = "sk-ant-api03-probe-anthropic-key-000000000000"
	openaiKey    = "sk-proj-probe-openai-key-0000000000000000000"
)

// The models the probe runs: one on each API the mock speaks.
const (
	anthropicModel = "anthropic/claude-haiku-4-5"
	openaiModel    = "openai/gpt-4.1-mini"
)

// binary is the pi the probe runs: HW_REAL_PI, the executable of an unpacked
// release (fetch.sh). The tests skip without it.
func binary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HW_REAL_PI")
	if bin == "" {
		t.Skip("HW_REAL_PI names no pi: see README.md")
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v: %s", bin, err, out)
	}
	t.Logf("pi %s", strings.TrimSpace(string(out)))
	return bin
}

// tagExtension is the extension that writes each input's tag into the
// session: hwtag.ts, beside this file.
func tagExtension(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("hwtag.ts")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// agent is one pi agent's files: its agent dir (PI_CODING_AGENT_DIR), its
// home, its workspace (pi's cwd) and its sessions (--session-dir), and how
// it is launched.
type agent struct {
	t        *testing.T
	bin      string
	dir      string
	home     string
	work     string
	sessions string
	id       string
	model    string
	ext      []string
	args     []string
	env      []string
	mcp      bool
}

// newAgent lays out an agent whose model's provider answers at mock, with a
// key for each provider in auth.json.
func newAgent(t *testing.T, mock *mockapi.Server, model string) *agent {
	t.Helper()
	root := t.TempDir()
	a := &agent{
		t: t, bin: binary(t), dir: filepath.Join(root, "agent"), home: filepath.Join(root, "home"),
		work: filepath.Join(root, "work"), sessions: filepath.Join(root, "agent", "sessions"),
		id: sessionID(t.Name()), model: model,
		ext: []string{tagExtension(t)},
	}
	for _, d := range []string{a.dir, a.home, a.work, a.sessions} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	a.writeJSON("settings.json", map[string]any{
		"retry":                  map[string]any{"enabled": true, "maxRetries": 2, "baseDelayMs": 200},
		"cacheWarming":           "off",
		"enableInstallTelemetry": false,
	})
	if mock != nil {
		a.writeJSON("models.json", map[string]any{"providers": map[string]any{
			"anthropic": map[string]any{"baseUrl": mock.URL()},
			"openai":    map[string]any{"baseUrl": mock.URL() + "/v1"},
		}})
	}
	a.writeJSON("auth.json", map[string]any{
		"anthropic": map[string]any{"type": "api_key", "key": anthropicKey},
		"openai":    map[string]any{"type": "api_key", "key": openaiKey},
	})
	return a
}

// sessionID is a session id pi takes, made from a test's name: letters,
// digits, '-', '_' and '.'.
func sessionID(name string) string {
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 'a' - 'A'
		}
		return '-'
	}, name)
	return "probe-" + strings.Trim(id, "-_.")
}

func (a *agent) writeJSON(name string, v any) {
	a.t.Helper()
	b, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(filepath.Join(a.dir, name), b, 0o600); err != nil {
		a.t.Fatal(err)
	}
}

// argv is how the agent's pi starts: RPC mode, the session it names, the
// model, and the tag extension.
func (a *agent) argv() []string {
	args := []string{"--mode", "rpc", "--session-id", a.id, "--session-dir", a.sessions, "--model", a.model}
	if !a.mcp {
		args = append(args, "--no-mcp")
	}
	for _, e := range a.ext {
		args = append(args, "-e", e)
	}
	return append(args, a.args...)
}

func (a *agent) environ() []string {
	return append([]string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + a.home,
		"TMPDIR=" + os.TempDir(),
		"PI_CODING_AGENT_DIR=" + a.dir,
		"PI_OFFLINE=1",
		"PI_TELEMETRY=0",
	}, a.env...)
}

// proc is a running pi, spoken to over its RPC records.
type proc struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	begun time.Time

	mu      sync.Mutex
	next    int
	waiting map[string]chan response
	records []record
	wake    chan struct{}
	stderr  bytes.Buffer
	exited  chan struct{}
}

// record is one record pi wrote that answers no command: an event, an
// extension UI request.
type record struct {
	At   time.Duration
	Type string
	Raw  json.RawMessage
}

// response is pi's answer to one command.
type response struct {
	Command string          `json:"command"`
	Success bool            `json:"success"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

func (a *agent) start() *proc {
	a.t.Helper()
	cmd := exec.Command(a.bin, a.argv()...)
	cmd.Dir, cmd.Env = a.work, a.environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p := &proc{t: a.t, cmd: cmd, begun: time.Now(), waiting: map[string]chan response{}, wake: make(chan struct{}), exited: make(chan struct{})}
	cmd.Stderr = &lockedWriter{mu: &p.mu, w: &p.stderr}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	p.stdin = stdin
	if err := cmd.Start(); err != nil {
		a.t.Fatal(err)
	}
	go p.read(stdout)
	go func() { _ = cmd.Wait(); close(p.exited) }()
	a.t.Cleanup(func() {
		select {
		case <-p.exited:
		default:
			p.kill()
		}
		if s := p.stderrText(); s != "" {
			a.t.Logf("pi's stderr:\n%s", s)
		}
	})
	return p
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(b)
}

func (p *proc) stderrText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stderr.String()
}

func (p *proc) logf(format string, args ...any) {
	p.t.Logf("%7.3f "+format, append([]any{time.Since(p.begun).Seconds()}, args...)...)
}

func short(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

func (p *proc) read(stdout io.Reader) {
	r := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var head struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if jerr := json.Unmarshal(line, &head); jerr != nil {
				p.logf("<< ?? %s", short(line, 300))
			} else if head.Type == "response" {
				var resp response
				_ = json.Unmarshal(line, &resp)
				p.logf("<= %s %s", head.ID, short(line, 600))
				p.mu.Lock()
				ch := p.waiting[head.ID]
				delete(p.waiting, head.ID)
				p.mu.Unlock()
				if ch != nil {
					ch <- resp
				}
			} else {
				if head.Type != "message_update" && head.Type != "tool_execution_update" {
					p.logf("<< %s", short(line, 700))
				}
				p.mu.Lock()
				p.records = append(p.records, record{At: time.Since(p.begun), Type: head.Type, Raw: append(json.RawMessage(nil), bytes.TrimSpace(line)...)})
				close(p.wake)
				p.wake = make(chan struct{})
				p.mu.Unlock()
			}
		}
		if err != nil {
			return
		}
	}
}

// send writes one command and returns its id; await waits for its answer.
func (p *proc) send(cmd map[string]any) string {
	p.mu.Lock()
	p.next++
	id := fmt.Sprintf("c%d", p.next)
	ch := make(chan response, 1)
	p.waiting[id] = ch
	p.mu.Unlock()
	cmd["id"] = id
	b, _ := json.Marshal(cmd)
	p.logf(">> %s", short(b, 400))
	if _, err := p.stdin.Write(append(b, '\n')); err != nil {
		p.logf(">> write: %v", err)
	}
	return id
}

func (p *proc) await(id string, d time.Duration) (response, bool) {
	p.mu.Lock()
	ch := p.waiting[id]
	p.mu.Unlock()
	if ch == nil {
		return response{}, false
	}
	select {
	case r := <-ch:
		return r, true
	case <-time.After(d):
		return response{}, false
	}
}

// call sends a command and waits up to 30 s for its answer.
func (p *proc) call(cmd map[string]any) response {
	p.t.Helper()
	typ := cmd["type"]
	r, ok := p.await(p.send(cmd), 30*time.Second)
	if !ok {
		p.t.Fatalf("%v: no answer in 30s", typ)
	}
	return r
}

// mark is where the records stand now: waitFor looks at records from a mark
// on.
func (p *proc) mark() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.records)
}

// waitFor waits up to d for a record of typ at or after from, and returns it
// with its index.
func (p *proc) waitFor(typ string, from int, d time.Duration) (record, int, bool) {
	deadline := time.After(d)
	for {
		p.mu.Lock()
		for i := from; i < len(p.records); i++ {
			if p.records[i].Type == typ {
				r := p.records[i]
				p.mu.Unlock()
				return r, i, true
			}
		}
		wake := p.wake
		p.mu.Unlock()
		select {
		case <-wake:
		case <-p.exited:
			p.mu.Lock()
			n := len(p.records)
			p.mu.Unlock()
			if n == from {
				return record{}, -1, false
			}
		case <-deadline:
			return record{}, -1, false
		}
	}
}

// mustWait is waitFor that fails the test.
func (p *proc) mustWait(typ string, from int, d time.Duration) (record, int) {
	p.t.Helper()
	r, i, ok := p.waitFor(typ, from, d)
	if !ok {
		p.t.Fatalf("no %s in %s", typ, d)
	}
	return r, i
}

// since are the records from a mark on.
func (p *proc) since(from int) []record {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]record(nil), p.records[from:]...)
}

// kill is a crash: SIGKILL to pi alone, not its process group.
func (p *proc) kill() {
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	<-p.exited
}

// term is SIGTERM to pi, waiting up to 10 s for it to exit.
func (p *proc) term() bool {
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.exited:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

// closeStdin ends pi's input and waits up to 10 s for it to exit.
func (p *proc) closeStdin() bool {
	_ = p.stdin.Close()
	select {
	case <-p.exited:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

// prompt sends text tagged with tag (the tag extension takes it off), and
// returns the command's id.
func (p *proc) prompt(tag, text string) string {
	msg := text
	if tag != "" {
		msg = "<!--hw:" + tag + "-->\n" + text
	}
	return p.send(map[string]any{"type": "prompt", "message": msg})
}

// run prompts, checks the prompt started a run, and waits up to d for the
// run to settle; it returns the mark the run's records start at.
func (p *proc) run(tag, text string, d time.Duration) int {
	p.t.Helper()
	from := p.mark()
	r, ok := p.await(p.prompt(tag, text), 30*time.Second)
	if !ok || !r.Success {
		p.t.Fatalf("prompt %q: %+v (answered: %v)", text, r, ok)
	}
	p.mustWait("agent_settled", from, d)
	return from
}

// entry is one line of a session file.
type entry struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	ParentID   *string         `json:"parentId"`
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data"`
	Message    *message        `json:"message"`
	Raw        json.RawMessage `json:"-"`
}

type message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	ToolCallID   string          `json:"toolCallId"`
	IsError      bool            `json:"isError"`
}

// text is a message's text: its string content, or its text blocks joined.
func (m *message) text() string {
	if m == nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(m.Content, &blocks)
	var out []string
	for _, b := range blocks {
		if b.Type == "text" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "")
}

// sessionFiles are the agent's session files under its session dir.
func (a *agent) sessionFiles() []string {
	var found []string
	_ = filepath.WalkDir(a.sessions, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, "_"+a.id+".jsonl") {
			found = append(found, p)
		}
		return nil
	})
	return found
}

// sessionFile is the agent's one session file.
func (a *agent) sessionFile() string {
	a.t.Helper()
	found := a.sessionFiles()
	if len(found) != 1 {
		a.t.Fatalf("session files for %s under %s: %v", a.id, a.sessions, found)
	}
	return found[0]
}

// entries reads a session file: its header, and its entries in order.
func entries(t *testing.T, path string) (map[string]any, []entry) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	var out []entry
	for i, line := range bytes.Split(bytes.TrimRight(b, "\n"), []byte("\n")) {
		if i == 0 {
			if err := json.Unmarshal(line, &header); err != nil {
				t.Fatalf("header: %v: %s", err, line)
			}
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Logf("line %d does not parse: %v: %s", i+1, err, short(line, 200))
			continue
		}
		e.Raw = append(json.RawMessage(nil), line...)
		out = append(out, e)
	}
	return header, out
}

// describe is a session's entries, one line each, for the log.
func describe(es []entry) string {
	var b strings.Builder
	for _, e := range es {
		switch {
		case e.Message != nil:
			fmt.Fprintf(&b, "  %s %s %s stop=%q err=%q %q\n", e.ID, e.Type, e.Message.Role, e.Message.StopReason, e.Message.ErrorMessage, short([]byte(e.Message.text()), 60))
		case e.Type == "custom":
			fmt.Fprintf(&b, "  %s custom %s %s\n", e.ID, e.CustomType, e.Data)
		default:
			fmt.Fprintf(&b, "  %s %s\n", e.ID, e.Type)
		}
	}
	return b.String()
}
