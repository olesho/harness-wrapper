// Package codexturns probes what codex's app-server does with turns a Session
// does not start: the turns codex starts by itself for a thread's goal, and
// an input sent while one runs. It speaks JSON-RPC to the pinned codex
// itself, with no adapter between, against the mock model API; each test
// asserts the behaviour the Harness Adapter's Codex profile is built on
// (FINDINGS.md), so a codex that behaves otherwise fails here first.
package codexturns

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/adapter/codex"
	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// server is a codex app-server the probe speaks to itself.
type server struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	start time.Time

	mu    sync.Mutex
	next  int
	calls map[int]chan json.RawMessage
	notes []note
	wake  chan struct{}
}

type note struct {
	at     time.Duration
	method string
	params json.RawMessage
}

func (s *server) logf(format string, args ...any) {
	s.t.Logf("%7.3f "+format, append([]any{time.Since(s.start).Seconds()}, args...)...)
}

func short(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

func startServer(t *testing.T, bin, dir string, env []string) *server {
	cmd := exec.Command(bin, "app-server")
	cmd.Dir, cmd.Env = dir, env
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	s := &server{t: t, cmd: cmd, stdin: stdin, start: time.Now(), calls: map[int]chan json.RawMessage{}, wake: make(chan struct{})}
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				var m struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
					Result json.RawMessage `json:"result"`
					Error  json.RawMessage `json:"error"`
				}
				if json.Unmarshal(line, &m) == nil {
					switch {
					case m.Method != "" && len(m.ID) > 0:
						s.logf("<< REQUEST %s %s", m.Method, short(m.Params, 300))
						_ = s.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "no"}})
					case m.Method != "":
						if !strings.Contains(m.Method, "delta") && m.Method != "account/rateLimits/updated" && m.Method != "thread/tokenUsage/updated" {
							s.logf("<< %s %s", m.Method, short(m.Params, 700))
						}
						s.mu.Lock()
						s.notes = append(s.notes, note{time.Since(s.start), m.Method, m.Params})
						close(s.wake)
						s.wake = make(chan struct{})
						s.mu.Unlock()
					default:
						var id int
						_ = json.Unmarshal(m.ID, &id)
						s.mu.Lock()
						ch := s.calls[id]
						delete(s.calls, id)
						s.mu.Unlock()
						out := m.Result
						if len(m.Error) > 0 {
							out = m.Error
						}
						if ch != nil {
							ch <- out
						}
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *server) write(v any) error {
	b, _ := json.Marshal(v)
	_, err := s.stdin.Write(append(b, '\n'))
	return err
}

// call sends a request; it returns the result (or error object) raw.
func (s *server) call(method string, params any) json.RawMessage {
	ch := s.callAsync(method, params)
	select {
	case r := <-ch:
		s.logf("<= %s: %s", method, short(r, 600))
		return r
	case <-time.After(30 * time.Second):
		s.logf("<= %s: NO ANSWER in 30s", method)
		return nil
	}
}

func (s *server) callAsync(method string, params any) chan json.RawMessage {
	s.mu.Lock()
	s.next++
	id := s.next
	ch := make(chan json.RawMessage, 1)
	s.calls[id] = ch
	s.mu.Unlock()
	pb, _ := json.Marshal(params)
	s.logf("=> %s %s", method, short(pb, 300))
	_ = s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return ch
}

// await waits for a notification of method after index from.
func (s *server) await(method string, from int, d time.Duration) (note, int, bool) {
	deadline := time.After(d)
	for {
		s.mu.Lock()
		for i := from; i < len(s.notes); i++ {
			if s.notes[i].method == method {
				n := s.notes[i]
				s.mu.Unlock()
				return n, i + 1, true
			}
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-deadline:
			return note{}, from, false
		}
	}
}

func (s *server) mark() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.notes)
}

// setup provisions an agent's roots with the Codex profile, pointed at a mock
// model API, and starts codex's app-server in them, initialized.
func setup(t *testing.T) (*mockapi.Server, *server, string, contract.Layout) {
	bin := os.Getenv("HW_REAL_CODEX")
	if bin == "" {
		t.Skip("HW_REAL_CODEX does not name a codex binary")
	}
	mock := mockapi.Start()
	mock.KeepBodies = true
	mock.ChunkDelay = 150 * time.Millisecond
	t.Cleanup(mock.Close)
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(base, "dist")
	_ = os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	_ = os.Symlink(bin, codex.BinaryPath(root))
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"), Workspace: filepath.Join(base, "workspace"),
		Secrets: filepath.Join(base, "secrets"), Scratch: filepath.Join(base, "scratch"),
	}
	for _, d := range []string{l.Home, l.Config, l.Workspace, l.Secrets, l.Scratch} {
		_ = os.MkdirAll(d, 0o700)
	}
	a := adapter.New(codex.Profile{})
	res, err := a.Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: root, Layout: l, Spec: contract.AgentSpec{
		Model: "mock", Instructions: contract.Instructions{Persona: "probe", Workspace: "# Workspace\n"}, PermissionPosture: contract.PostureBypass,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i, f := range res.Files {
		if f.Root == contract.RootConfig && f.Path == "config.toml" {
			provider := fmt.Sprintf("\n[model_providers.mock]\nname = \"mock\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\nrequest_max_retries = 0\nstream_max_retries = 2\nstream_idle_timeout_ms = 120000\n", mock.URL()+"/v1")
			res.Files[i] = contract.TextFile(f.Root, f.Path, f.Mode, "model_provider = \"mock\"\n"+string(f.Content())+provider)
		}
	}
	if err := conformance.Apply(l, res); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Binary     string   `json:"binary"`
		Env        []string `json:"env"`
		WorkingDir string   `json:"working_dir"`
	}
	_ = json.Unmarshal(res.OpenConfig, &cfg)
	s := startServer(t, cfg.Binary, cfg.WorkingDir, append(adapter.HostEnv(), cfg.Env...))
	t.Cleanup(func() {
		_ = s.stdin.Close()
		done := make(chan struct{})
		go func() { _ = s.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = s.cmd.Process.Kill()
		}
	})
	s.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "probe", "title": "probe", "version": "0"}})
	_ = s.write(map[string]any{"jsonrpc": "2.0", "method": "initialized"})
	return mock, s, base, l
}

func threadID(raw json.RawMessage) string {
	var r struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.Thread.ID
}
