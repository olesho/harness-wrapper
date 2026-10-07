package fakeadapter

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
)

// mcpServer is an http connector as the fake harness's configuration names
// it: a header's secret value stays in its file, or its environment variable,
// and is read when the harness connects.
type mcpServer struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers,omitempty"`
	HeadersFile map[string]string `json:"headers_file,omitempty"`
	HeadersEnv  map[string]string `json:"headers_env,omitempty"`
}

// mcpServers are the spec's http connectors.
func mcpServers(spec contract.AgentSpec) []mcpServer {
	var out []mcpServer
	for _, c := range spec.Connectors {
		if h := c.HTTP; h != nil {
			out = append(out, mcpServer{Name: c.Name, URL: h.URL, Headers: h.Headers, HeadersFile: h.HeadersFile, HeadersEnv: h.HeadersEnv})
		}
	}
	return out
}

// connectMCP is the fake harness connecting to its http connectors' MCP
// servers as its Session opens: one initialize request each, with the
// connector's headers. A server that cannot be reached is left out, as a
// harness leaves out a connector that fails; the Session works on.
func (s *session) connectMCP() {
	if len(s.cfg.MCP) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.exited:
			cancel()
		case <-ctx.Done():
		}
	}()
	client := &http.Client{Timeout: 5 * time.Second}
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fake-harness","version":"` + Version + `"}}}`)
	for _, m := range s.cfg.MCP {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.URL, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		for k, v := range m.Headers {
			req.Header.Set(k, v)
		}
		for k, env := range m.HeadersEnv {
			if v, ok := os.LookupEnv(env); ok {
				req.Header.Set(k, v)
			}
		}
		if !s.adapter.breaks("headers-file-unread") {
			// HeadersFile supersedes HeadersEnv; the file is read now, so a
			// host may rewrite it between launches.
			for k, file := range m.HeadersFile {
				if b, err := os.ReadFile(file); err == nil {
					req.Header.Set(k, strings.TrimSpace(string(b)))
				}
			}
		}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
}
