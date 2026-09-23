//go:build linux

package wrapper_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
)

// The authenticated conformance runs that activate the claude-code profile
// (step 3 of the containment plan, G5 in its Validation): what the
// credential-free smoke tests cannot show, because it needs a model and an
// account. Each runs the pinned claude (HW_REAL_CLAUDE) contained with TCP
// restricted to 443, as the login a person stored in HW_REAL_CLAUDE_STATE_DIR
// with contain-login, and uses the account's quota. They run in an ABI 9
// guest, never for pull requests.

// claudeResult is the result object `claude -p --output-format json` prints.
type claudeResult struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	IsError   bool    `json:"is_error"`
	Result    string  `json:"result"`
	SessionID string  `json:"session_id"`
	NumTurns  int     `json:"num_turns"`
	CostUSD   float64 `json:"total_cost_usd"`
}

// signedInRun is one finished contained `claude -p` run.
type signedInRun struct {
	claudeResult
	applied *containment.Applied
	output  string
}

// runSignedIn runs `claude -p --output-format json [args...]` contained in wd
// as the login in stateDir, with env (NAME=value) passed into the domain. It
// runs at the bypass rung, so every tool call the model makes is attempted and
// only the domain can refuse it.
func runSignedIn(t *testing.T, bin, stateDir, wd string, env []string, args ...string) signedInRun {
	t.Helper()
	var names []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	out := &lockedBuffer{}
	s, err := wrapper.Start(context.Background(), wrapper.Config{
		Harness:        "claude",
		BinaryPath:     bin,
		Args:           append([]string{"-p", "--output-format", "json", "--model", "sonnet"}, args...),
		PermissionMode: "bypass",
		WorkingDir:     wd,
		Stdout:         out,
		WaitDelay:      time.Second,
		Env:            append([]string{"PATH=/usr/bin:/bin", "TERM=xterm-256color", "LANG=C.UTF-8"}, env...),
		Containment: &wrapper.Containment{
			Kind: wrapper.ContainmentLandlock, RestrictTCP: true, ConnectTCP: []uint16{443}, StateDir: stateDir,
			PassEnv: names,
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := waitOrFail(t, s, 5*time.Minute)
	run := signedInRun{applied: s.Containment(), output: screenText(out)}
	var ok bool
	if run.claudeResult, ok = resultOf(run.output); !ok {
		t.Fatalf("no result from claude (%s, exit %d); output: %s", res.Status, res.ExitCode, lastOutput(run.output, 3000))
	}
	t.Logf("session %s: %d turns, $%.4f: %s", run.SessionID, run.NumTurns, run.CostUSD, lastOutput(run.Result, 400))
	if res.ExitCode != 0 || run.IsError {
		t.Fatalf("claude failed (%s, exit %d): %s", res.Status, res.ExitCode, lastOutput(run.output, 3000))
	}
	return run
}

// resultOf finds the result object in a run's output.
func resultOf(out string) (claudeResult, bool) {
	for _, l := range strings.Split(out, "\n") {
		var r claudeResult
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "{") && json.Unmarshal([]byte(l), &r) == nil && r.Type == "result" {
			return r, true
		}
	}
	return claudeResult{}, false
}

// toolCall is one tool_use in a claude transcript and the tool_result that
// answered it.
type toolCall struct {
	Name, Input, Result string
}

// transcriptCalls reads the tool calls of one session from its transcript in
// the config directory the domain granted: the wrapper's view of the session's
// files, never the caller's own ~/.claude.
func transcriptCalls(t *testing.T, configDir, sessionID string) []toolCall {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if len(files) != 1 {
		logTree(t, configDir)
		t.Fatalf("transcripts for session %s under %s: %v", sessionID, configDir, files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	type block struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
	}
	var calls []toolCall
	index := map[string]int{}
	for _, line := range strings.Split(string(b), "\n") {
		var entry struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		var blocks []block
		if json.Unmarshal([]byte(line), &entry) != nil || json.Unmarshal(entry.Message.Content, &blocks) != nil {
			continue
		}
		for _, bl := range blocks {
			switch bl.Type {
			case "tool_use":
				index[bl.ID] = len(calls)
				calls = append(calls, toolCall{Name: bl.Name, Input: string(bl.Input)})
			case "tool_result":
				if i, ok := index[bl.ToolUseID]; ok {
					calls[i].Result = flattenContent(bl.Content)
				}
			}
		}
	}
	return calls
}

// flattenContent is a tool_result's content as text: a string, or the text
// parts of a list.
func flattenContent(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// requireCall returns the first call to one of names whose input contains
// want, failing the test when there is none.
func requireCall(t *testing.T, calls []toolCall, want string, names ...string) toolCall {
	t.Helper()
	for _, c := range calls {
		for _, n := range names {
			if c.Name == n && strings.Contains(c.Input, want) {
				return c
			}
		}
	}
	t.Fatalf("no %v call with %q in the transcript; calls: %+v", names, want, calls)
	return toolCall{}
}

// logTree logs the files beneath dir, for a failure's diagnosis.
func logTree(t *testing.T, dir string) {
	t.Helper()
	var paths []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			if strings.Count(rel, string(filepath.Separator)) > 3 {
				return fs.SkipDir
			}
			paths = append(paths, rel)
		}
		return nil
	})
	t.Logf("%s holds:\n  %s", dir, strings.Join(paths, "\n  "))
}

func nonce(prefix string) string {
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano()%1e9, 36)
}

// TestRealClaudeToolTurn has the model edit a file that exists, write a new
// one, and then run a Bash command that writes outside every grant. The edits
// land in the working directory, and claude's backup of the edited file in its
// file history, in the login's config directory; the Bash command is attempted
// and the OS refuses it.
func TestRealClaudeToolTurn(t *testing.T) {
	bin := realHarnessSetup(t, "HW_REAL_CLAUDE")
	stateDir := signedInStateDir(t, "HW_REAL_CLAUDE_STATE_DIR")
	wd, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(wd, "notes.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(outside, "escape.txt")
	// -p keeps file history only when asked to; an interactive session keeps
	// it by default.
	run := runSignedIn(t, bin, stateDir, wd, []string{"CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING=true"},
		"This is an automated test of a sandbox. Do exactly these steps, in order, and nothing else. "+
			"1. Use the Edit tool to replace alpha with beta in notes.txt in the current directory. "+
			"2. Use the Write tool to create new.txt in the current directory, containing the single line: gamma. "+
			"3. Use the Bash tool to run exactly this command, once, even though it is expected to fail: touch "+escape+" "+
			"Then reply with the single word finished.")
	config := run.applied.State.HarnessState
	logTree(t, config)

	for name, want := range map[string]string{"notes.txt": "beta", "new.txt": "gamma"} {
		if b, err := os.ReadFile(filepath.Join(wd, name)); err != nil || strings.TrimSpace(string(b)) != want {
			t.Errorf("%s = %q (%v), want %s", name, b, err, want)
		}
	}
	if _, err := os.Stat(escape); err == nil {
		t.Error("the Bash tool wrote outside every grant")
	}
	calls := transcriptCalls(t, config, run.SessionID)
	requireCall(t, calls, "notes.txt", "Edit")
	requireCall(t, calls, "new.txt", "Write")
	denied := requireCall(t, calls, "touch "+escape, "Bash")
	t.Logf("the Bash call's result: %s", denied.Result)
	if !strings.Contains(denied.Result, "Permission denied") {
		t.Errorf("the Bash call's result shows no OS denial: %q", denied.Result)
	}
	// The backup claude took before the Edit holds the file's old content.
	backups, _ := filepath.Glob(filepath.Join(config, "file-history", run.SessionID, "*"))
	found := false
	for _, b := range backups {
		if content, err := os.ReadFile(b); err == nil && strings.TrimSpace(string(content)) == "alpha" {
			found = true
		}
	}
	if !found {
		t.Errorf("no backup of notes.txt in the session's file history: %v", backups)
	}
}

// TestRealClaudeSubagent has the model delegate a command to a Task subagent,
// which runs it in the working directory and leaves its own transcript beside
// the session's.
func TestRealClaudeSubagent(t *testing.T) {
	bin := realHarnessSetup(t, "HW_REAL_CLAUDE")
	stateDir := signedInStateDir(t, "HW_REAL_CLAUDE_STATE_DIR")
	wd := t.TempDir()
	run := runSignedIn(t, bin, stateDir, wd, nil,
		"This is an automated test. Use the Task tool, with subagent_type general-purpose, to have a subagent "+
			"run the Bash command `echo from-subagent > sub.txt` in "+wd+" and report what happened. "+
			"Do not run any command yourself. Then reply with the single word finished.")
	config := run.applied.State.HarnessState
	logTree(t, config)

	if b, err := os.ReadFile(filepath.Join(wd, "sub.txt")); err != nil || strings.TrimSpace(string(b)) != "from-subagent" {
		t.Errorf("sub.txt = %q (%v), want the subagent's from-subagent", b, err)
	}
	requireCall(t, transcriptCalls(t, config, run.SessionID), "", "Task", "Agent")
	if sub, _ := filepath.Glob(filepath.Join(config, "projects", "*", run.SessionID, "subagents", "*.jsonl")); len(sub) == 0 {
		t.Error("no subagent transcript beside the session's")
	}
}

// TestRealClaudeMCPServer has the model call the one tool of a stdio MCP
// server (HW_TEST_MCP_PROBE, a prebuilt test/mcpprobe) that claude starts
// inside its domain. The tool answers with the server's nonce and the outcome
// of reading a file outside every grant, which the domain refuses.
func TestRealClaudeMCPServer(t *testing.T) {
	bin := realHarnessSetup(t, "HW_REAL_CLAUDE")
	stateDir := signedInStateDir(t, "HW_REAL_CLAUDE_STATE_DIR")
	probe := os.Getenv("HW_TEST_MCP_PROBE")
	if probe == "" {
		t.Skip("set HW_TEST_MCP_PROBE to a prebuilt test/mcpprobe")
	}
	wd, outside := t.TempDir(), t.TempDir()
	// The server runs from the working directory: of the grants a caller
	// does not add, only it carries execute.
	server := filepath.Join(wd, "mcpprobe")
	b, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server, b, 0o755); err != nil {
		t.Fatal(err)
	}
	forbidden := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(forbidden, []byte("not for the harness\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	word := nonce("probe")
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"probe": map[string]any{
		"type": "stdio", "command": server, "args": []string{"-nonce", word, "-forbidden", forbidden},
	}}})
	cfgPath := filepath.Join(wd, "mcp.json")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	run := runSignedIn(t, bin, stateDir, wd, nil, "--mcp-config", cfgPath, "--strict-mcp-config",
		"This is an automated test. Call the probe tool of the probe MCP server once, "+
			"then reply with exactly the text it returned.")

	call := requireCall(t, transcriptCalls(t, run.applied.State.HarnessState, run.SessionID), "", "mcp__probe__probe")
	t.Logf("the MCP tool's result: %s", call.Result)
	if !strings.Contains(call.Result, "nonce="+word) {
		t.Errorf("the MCP tool's result has no nonce %s: %q", word, call.Result)
	}
	if !strings.Contains(call.Result, "read-denied") || !strings.Contains(call.Result, "permission denied") {
		t.Errorf("the MCP server read a file outside every grant: %q", call.Result)
	}
}

// TestRealClaudeResume resumes a session in a second contained launch, which
// finds the first launch's transcript in the login's config directory.
func TestRealClaudeResume(t *testing.T) {
	bin := realHarnessSetup(t, "HW_REAL_CLAUDE")
	stateDir := signedInStateDir(t, "HW_REAL_CLAUDE_STATE_DIR")
	wd := t.TempDir()
	word := nonce("pelican")
	first := runSignedIn(t, bin, stateDir, wd, nil, "Remember this word for later: "+word+". Reply with only OK.")
	second := runSignedIn(t, bin, stateDir, wd, nil, "--resume", first.SessionID,
		"What word did I ask you to remember? Reply with only that word.")
	if !strings.Contains(second.Result, word) {
		t.Errorf("the resumed session answered %q, want %s", second.Result, word)
	}
	t.Logf("first session %s, resumed as %s", first.SessionID, second.SessionID)
}

// TestRealClaudeTokenRefresh expires the stored login's access token and runs
// a prompt: claude refreshes the token inside the domain and stores the new
// one in the StateDir, where the next session finds it. It rewrites the
// StateDir's login, and a failed refresh can leave it signed out.
func TestRealClaudeTokenRefresh(t *testing.T) {
	bin := realHarnessSetup(t, "HW_REAL_CLAUDE")
	stateDir := signedInStateDir(t, "HW_REAL_CLAUDE_STATE_DIR")
	creds := filepath.Join(stateDir, "home", ".claude", ".credentials.json")
	read := func() (doc, oauth map[string]any) {
		t.Helper()
		b, err := os.ReadFile(creds)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		oauth, ok := doc["claudeAiOauth"].(map[string]any)
		if !ok {
			t.Fatalf("%s holds no claude.ai login", creds)
		}
		return doc, oauth
	}
	doc, oauth := read()
	before := map[string]any{"accessToken": oauth["accessToken"], "refreshToken": oauth["refreshToken"]}
	oauth["expiresAt"] = time.Now().Add(-time.Minute).UnixMilli()
	b, _ := json.Marshal(doc)
	tmp := creds + ".conformance"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, creds); err != nil {
		t.Fatal(err)
	}

	run := runSignedIn(t, bin, stateDir, t.TempDir(), nil, signedInPrompt)
	if !strings.Contains(run.Result, "CONTAINED") {
		t.Errorf("reply %q, want CONTAINED", run.Result)
	}
	_, after := read()
	expires, _ := after["expiresAt"].(float64)
	if after["accessToken"] == before["accessToken"] || time.UnixMilli(int64(expires)).Before(time.Now().Add(time.Hour)) {
		t.Errorf("the login was not refreshed in the StateDir: expiresAt %s, access token changed %v",
			time.UnixMilli(int64(expires)).UTC().Format(time.RFC3339), after["accessToken"] != before["accessToken"])
	}
	t.Logf("refreshed: expires %s, refresh token rotated %v",
		time.UnixMilli(int64(expires)).UTC().Format(time.RFC3339), after["refreshToken"] != before["refreshToken"])
}
