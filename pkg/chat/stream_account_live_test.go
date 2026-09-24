package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/harnessenv"
)

// TestStreamAccountLive runs the stream-json transport against the real
// Anthropic API with a real account: one conversation that answers, runs a
// Bash tool with hooks firing, calls a local MCP server, is interrupted, is
// quit and reopened, and remembers its first answer. It spends a handful of
// short turns on a small model:
//
//	HW_LIVE_ACCOUNT=1 HW_LIVE_TOKEN_FILE=<file holding a `claude setup-token` token> \
//	  go test ./pkg/chat -run StreamAccountLive -v
//
// The token is read from the file and passed only as CLAUDE_CODE_OAUTH_TOKEN
// in claude's environment; it is never logged. HW_LIVE_MODEL overrides the
// model (default haiku).
func TestStreamAccountLive(t *testing.T) {
	if os.Getenv("HW_LIVE_ACCOUNT") != "1" {
		t.Skip("uses a real account: set HW_LIVE_ACCOUNT=1 and HW_LIVE_TOKEN_FILE (spends a few short turns)")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary not on PATH: %v", err)
	}
	tok, err := os.ReadFile(os.Getenv("HW_LIVE_TOKEN_FILE"))
	if err != nil || len(strings.TrimSpace(string(tok))) == 0 {
		t.Fatalf("HW_LIVE_TOKEN_FILE must name a file holding the token: %v", err)
	}
	model := os.Getenv("HW_LIVE_MODEL")
	if model == "" {
		model = "haiku"
	}
	nonce := sessionid.NewUUID()[:8]

	root := t.TempDir()
	wd, cfg := filepath.Join(root, "work"), filepath.Join(root, "cfg")
	for _, d := range []string{wd, cfg} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realWD, _ := filepath.EvalSymlinks(wd)
	hookLog, mcpLog := filepath.Join(root, "hooks.log"), filepath.Join(root, "mcp.log")
	writeJSON := func(path string, v any) {
		data, _ := json.Marshal(v)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(filepath.Join(cfg, ".claude.json"), map[string]any{
		"hasCompletedOnboarding": true, "bypassPermissionsModeAccepted": true,
		"projects": map[string]any{realWD: map[string]any{"hasTrustDialogAccepted": true}},
	})
	hook := []map[string]any{{"matcher": "*", "hooks": []map[string]any{{"type": "command", "command": "cat >> " + hookLog + "; echo >> " + hookLog}}}}
	writeJSON(filepath.Join(cfg, "settings.json"), map[string]any{
		"skipDangerousModePermissionPrompt": true,
		"hooks":                             map[string]any{"PreToolUse": hook, "PostToolUse": hook},
	})
	self, err := os.Executable() // absolute: claude cannot spawn a relative MCP command (ENOENT)
	if err != nil {
		t.Fatal(err)
	}
	mcpConfig := filepath.Join(root, "mcp.json")
	writeJSON(mcpConfig, map[string]any{"mcpServers": map[string]any{"probe": map[string]any{
		"type": "stdio", "command": self, "args": []string{},
		"env": map[string]string{mcpFakeEnv: mcpLog},
	}}})

	env := append(harnessenv.Cleaned(),
		"CLAUDE_CONFIG_DIR="+cfg,
		"CLAUDE_CODE_OAUTH_TOKEN="+strings.TrimSpace(string(tok)),
		"DISABLE_AUTOUPDATER=1")
	args := []string{"--mcp-config", mcpConfig, "--strict-mcp-config"}
	store := newFakeStore()

	open := func(reopen string) *streamLive {
		t.Helper()
		r := &streamRig{t: t, news: make(chan struct{}, 1)}
		onEvent := func(ev ConversationEvent) {
			r.mu.Lock()
			r.evs = append(r.evs, ev)
			r.mu.Unlock()
			select {
			case r.news <- struct{}{}:
			default:
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var conv *Conversation
		var err error
		if reopen == "" {
			conv, err = Open(ctx, Options{
				Harness: chatClaudeCode, BinaryPath: bin, WorkingDir: wd, Env: env, Args: args, Model: model,
				Transport: TransportStreamJSON, PermissionMode: "bypass", Store: store, OnEvent: onEvent,
			})
		} else {
			conv, err = Reopen(ctx, ReopenOptions{
				SessionID: reopen, Transport: TransportStreamJSON, BinaryPath: bin, Env: env, Args: args, Model: model,
				PermissionMode: "bypass", Store: store, OnEvent: onEvent,
			})
		}
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		r.conv = conv
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = conv.Close(ctx)
		})
		return &streamLive{rig: r, conv: conv}
	}
	report := func(step string, turn Turn) {
		t.Logf("%-10s %-11s http=%d code=%q text=%q reason=%q", step, turn.State, turn.HTTPCode, turn.Code,
			oneLineCapped(turn.Text, 120), oneLineCapped(turn.Reason, 160))
	}
	expect := func(step string, turn Turn, want string) {
		t.Helper()
		report(step, turn)
		if turn.State != TurnStateComplete || !strings.Contains(turn.Text, want) {
			t.Errorf("%s: turn %s %q, want complete containing %q", step, turn.State, turn.Text, want)
		}
	}

	lv := open("")
	expect("reply", lv.turn("Reply with exactly the word PONG"+nonce+" and nothing else."), "PONG"+nonce)
	expect("bash", lv.turn("Use the Bash tool to run exactly this command: echo hw-bash-"+nonce+
		" — then reply with the command's output and nothing else."), "hw-bash-"+nonce)
	expect("mcp", lv.turn("Call the echo tool of the probe MCP server with the text hw-mcp-"+nonce+
		", then reply with the tool's result and nothing else."), "hw-mcp-"+nonce)

	id := lv.sendOnly("Write a 700-word story about a lighthouse keeper. Do not use any tools.")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && lv.conv.State().LastOutputAt.Before(time.Now().Add(-500*time.Millisecond)) {
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	res, err := lv.conv.Interrupt(ctx)
	cancel()
	turn := lv.rig.ended(id)
	report("interrupt", turn)
	t.Logf("interrupt  result=%q err=%v partial=%d bytes", res, err, len(turn.Text))
	if err != nil || (res != InterruptStopped && res != InterruptTooLate) {
		t.Errorf("Interrupt = %q, %v; want stopped (or too_late on a very fast reply)", res, err)
	}
	expect("after", lv.turn("Reply with exactly the word AFTER"+nonce+" and nothing else."), "AFTER"+nonce)

	st := lv.conv.State()
	t.Logf("state      alive=%v busy=%v status=%q session=%v", st.Alive, st.Busy, st.Status, st.HarnessSessionID != "")
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := lv.conv.Quit(ctx); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	<-lv.conv.Done()
	t.Logf("exit       %+v", *lv.conv.State().Exit)

	re := open(lv.conv.SessionID())
	expect("resumed", re.turn("What exact word did you reply with in your very first answer in this conversation? Reply with that word only."), "PONG"+nonce)

	hooks, _ := os.ReadFile(hookLog)
	calls, _ := os.ReadFile(mcpLog)
	t.Logf("hooks      PreToolUse=%d PostToolUse=%d", strings.Count(string(hooks), `"hook_event_name":"PreToolUse"`),
		strings.Count(string(hooks), `"hook_event_name":"PostToolUse"`))
	t.Logf("mcp        %s", strings.Join(strings.Fields(string(calls)), " "))
	if !strings.Contains(string(hooks), `"PreToolUse"`) || !strings.Contains(string(hooks), `"PostToolUse"`) {
		t.Errorf("the tool hooks did not fire:\n%s", hooks)
	}
	if !strings.Contains(string(calls), "call") {
		t.Errorf("the MCP server was not called: %q", calls)
	}
	hist, src, err := re.conv.HistoryWithSource(ctx)
	t.Logf("history    %d turns from %s (%v)", len(hist), src, err)
}

// mcpFakeEnv, set, makes the test binary a one-tool stdio MCP server
// ("echo") that appends "start" and "call <text>" lines to the file it names.
const mcpFakeEnv = "HW_MCP_FAKE"

func runMCPFake(logPath string) int {
	note := func(s string) {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(s + "\n")
			_ = f.Close()
		}
	}
	note(fmt.Sprintf("start %d", os.Getpid()))
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(nil, 1<<20)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
				Arguments       struct {
					Text string `json:"text"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || req.ID == nil {
			continue // a notification
		}
		var res any
		switch req.Method {
		case "initialize":
			pv := req.Params.ProtocolVersion
			if pv == "" {
				pv = "2025-06-18"
			}
			res = map[string]any{
				"protocolVersion": pv, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "probe", "version": "0.1"},
			}
		case "tools/list":
			res = map[string]any{"tools": []map[string]any{{
				"name": "echo", "description": "Echo text back.",
				"inputSchema": map[string]any{
					"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}},
					"required": []string{"text"},
				},
			}}}
		case "tools/call":
			note("call " + req.Params.Arguments.Text)
			res = map[string]any{"content": []map[string]any{{"type": "text", "text": req.Params.Arguments.Text}}}
		default:
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			continue
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
	}
	return 0
}
