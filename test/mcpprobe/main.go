// mcpprobe is a stdio MCP server for the real-harness conformance runs. It
// serves one tool, "probe", whose result carries the nonce the server was
// started with and the outcome of reading the file named by -forbidden. A
// contained harness starts it inside its own domain, so that read shows the
// domain's denial from an MCP server's side.
//
//	mcpprobe -nonce WORD -forbidden PATH
//
// It speaks newline-delimited JSON-RPC 2.0 on stdin and stdout: initialize,
// ping, tools/list and tools/call. Notifications are read and ignored.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

type request struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

func main() {
	nonce := flag.String("nonce", "", "the word the probe tool returns")
	forbidden := flag.String("forbidden", "", "a file the probe tool tries to read")
	flag.Parse()

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req request
		if err := json.Unmarshal(in.Bytes(), &req); err != nil || len(req.ID) == 0 {
			continue
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if result, ok := answer(req, *nonce, *forbidden); ok {
			resp["result"] = result
		} else {
			resp["error"] = map[string]any{"code": -32601, "message": "method not found: " + req.Method}
		}
		if err := out.Encode(resp); err != nil {
			os.Exit(1)
		}
	}
}

func answer(req request, nonce, forbidden string) (any, bool) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "mcpprobe", "version": "1"},
		}, true
	case "ping":
		return map[string]any{}, true
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name":        "probe",
			"description": "Returns this server's nonce and the outcome of reading a file outside the working directory.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		}}}, true
	case "tools/call":
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": probe(nonce, forbidden)}}}, true
	}
	return nil, false
}

func probe(nonce, forbidden string) string {
	outcome := "read-allowed"
	if _, err := os.ReadFile(forbidden); err != nil {
		outcome = "read-denied: " + err.Error()
	}
	return fmt.Sprintf("nonce=%s %s", nonce, outcome)
}
