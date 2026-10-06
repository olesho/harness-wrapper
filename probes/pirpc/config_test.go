package pirpc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/mockapi"
)

// mcpServer is a streamable-HTTP MCP server with one tool, echo, that records
// each JSON-RPC call it gets: when, which method, with which headers.
type mcpServer struct {
	srv   *httptest.Server
	begun time.Time

	mu    sync.Mutex
	calls []mcpCall
}

type mcpCall struct {
	At     time.Duration
	Method string
	Header http.Header
}

func startMCP(t *testing.T) *mcpServer {
	t.Helper()
	m := &mcpServer{begun: time.Now()}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mcpServer) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	_ = json.Unmarshal(raw, &req)
	m.mu.Lock()
	m.calls = append(m.calls, mcpCall{At: time.Since(m.begun), Method: req.Method, Header: r.Header.Clone()})
	m.mu.Unlock()
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		v := req.Params.ProtocolVersion
		if v == "" {
			v = "2025-06-18"
		}
		result = map[string]any{
			"protocolVersion": v, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "probe", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name": "echo", "description": "Echo the text back",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		}}}
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func (m *mcpServer) record() []mcpCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mcpCall(nil), m.calls...)
}

// pi connects to the MCP servers in its mcp.json when it starts, before any
// prompt; a header value "!command" is that command's output; a server with
// direct exposure has its tools declared to the model.
func TestMCPAtOpen(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	mock.KeepBodies = true
	mcp := startMCP(t)
	a := newAgent(t, mock, anthropicModel)
	secret := filepath.Join(a.home, "header")
	if err := os.WriteFile(secret, []byte("probe-header-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.writeJSON("mcp.json", map[string]any{"mcpServers": map[string]any{"probe": map[string]any{
		"url": mcp.srv.URL + "/mcp", "exposure": "direct",
		"headers": map[string]any{"X-Probe": "!cat " + secret},
	}}})
	a.mcp = true
	opened := time.Since(mcp.begun)
	p := a.start()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && len(mcp.record()) < 3 {
		time.Sleep(100 * time.Millisecond)
	}
	calls := mcp.record()
	for _, c := range calls {
		t.Logf("%.3fs after start: %s, X-Probe %q", (c.At - opened).Seconds(), c.Method, c.Header.Get("X-Probe"))
	}
	if len(calls) == 0 || calls[0].Method != "initialize" {
		t.Fatalf("no initialize before any prompt: %+v", calls)
	}
	if got := calls[0].Header.Get("X-Probe"); got != "probe-header-value" {
		t.Errorf("X-Probe %q, want the command's output", got)
	}
	p.run("in-1", "PING 1", 60*time.Second)
	reqs := mock.Requests()
	var body struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(reqs[len(reqs)-1].Body, &body)
	var names []string
	for _, tl := range body.Tools {
		names = append(names, tl.Name)
	}
	t.Logf("tools the model was offered: %v", names)
	if !strings.Contains(strings.Join(names, " "), "mcp__probe__echo") {
		t.Errorf("the MCP tool is not declared to the model: %v", names)
	}
}

// A model pi's catalog does not have: without a models.json entry pi refuses
// it; with one under its provider (an id alone), pi runs it.
func TestModelOutsideTheCatalog(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	const model = "anthropic/claude-probe-9"

	a := newAgent(t, mock, model)
	p := a.start()
	r, ok := p.await(p.prompt("in-1", "PING 1"), 15*time.Second)
	st := state(t, p)
	t.Logf("not in the catalog: prompt %+v (answered %v); state model %+v; stderr %q", r, ok, st.Model, p.stderrText())

	b := newAgent(t, mock, model)
	b.writeJSON("models.json", map[string]any{"providers": map[string]any{
		"anthropic": map[string]any{"baseUrl": mock.URL(), "models": []any{map[string]any{"id": "claude-probe-9"}}},
	}})
	q := b.start()
	from := q.run("in-1", "PING 1", 60*time.Second)
	st = state(t, q)
	t.Logf("with a models.json entry: state model %+v; answer %q", st.Model, lastAssistant(q.since(from)).text())
	if st.Model.ID != "claude-probe-9" || lastAssistant(q.since(from)).text() != "PONG 1" {
		t.Error("a model added in models.json does not run")
	}
	reqs := mock.Requests()
	if reqs[len(reqs)-1].Model != "claude-probe-9" {
		t.Errorf("the model got %q", reqs[len(reqs)-1].Model)
	}
}

// What of a release pi needs: the executable alone reports version 0.0.0; with
// the package.json beside it, the release's version, and an RPC run with a
// tool works with nothing else.
func TestDistribution(t *testing.T) {
	mock := mockapi.Start()
	defer mock.Close()
	src := filepath.Dir(binary(t))
	copyFile := func(from, to string) {
		t.Helper()
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(from)
		if err := os.WriteFile(to, b, st.Mode()); err != nil {
			t.Fatal(err)
		}
	}
	version := func(dir string) string {
		out, _ := exec.Command(filepath.Join(dir, "pi"), "--version").CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	alone := t.TempDir()
	copyFile(filepath.Join(src, "pi"), filepath.Join(alone, "pi"))
	with := t.TempDir()
	copyFile(filepath.Join(src, "pi"), filepath.Join(with, "pi"))
	copyFile(filepath.Join(src, "package.json"), filepath.Join(with, "package.json"))
	t.Logf("pi --version: alone %q, with package.json %q", version(alone), version(with))

	a := newAgent(t, mock, anthropicModel)
	a.bin = filepath.Join(with, "pi")
	p := a.start()
	from := p.run("in-1", "TOOL echo minimal", 60*time.Second)
	if got := lastAssistant(p.since(from)).text(); got != "TOOL DONE: minimal" {
		t.Errorf("pi and its package.json alone: %q, want TOOL DONE: minimal", got)
	}
	entries, _ := os.ReadDir(src)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	t.Logf("the release's files: %v", names)
}
