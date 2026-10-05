package conformance

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// mcpServer is a minimal MCP server over streamable HTTP: it answers
// initialize and tools/list with no tools, accepts notifications, and keeps
// the headers each request carried.
type mcpServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newMCPServer() *mcpServer {
	m := &mcpServer{}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	return m
}

func (m *mcpServer) url() string { return m.srv.URL + "/mcp" }

func (m *mcpServer) close() { m.srv.Close() }

func (m *mcpServer) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.seen = append(m.seen, r.Header.Clone())
	m.mu.Unlock()
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "not a JSON-RPC message", http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any = map[string]any{}
	switch req.Method {
	case "initialize":
		version := req.Params.ProtocolVersion
		if version == "" {
			version = "2025-06-18"
		}
		result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "hw-conformance", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// heard reports whether a request carried header with value, and how many
// requests came.
func (m *mcpServer) heard(header, value string) (bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range m.seen {
		if h.Get(header) == value {
			return true, len(m.seen)
		}
	}
	return false, len(m.seen)
}
