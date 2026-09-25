package harness_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/harnessenv"
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// hookHandlerEnv, set in claude's environment, makes this test binary the
// hook command's handler: `<test binary> claude <event>` runs HandleHookEvent
// on the hook's stdin, exactly as a consumer's `<binary> hooks claude
// <event>` does.
const hookHandlerEnv = "HW_TEST_HOOK_HANDLER"

func runHookHandler(args []string) int {
	if len(args) != 2 {
		fmt.Fprintf(os.Stderr, "hook handler: want <harness> <event>, got %q\n", args)
		return 1
	}
	stdin, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
	if err != nil {
		fmt.Fprintln(os.Stderr, "hook handler:", err)
		return 1
	}
	if _, err := harness.HandleHookEvent(args[0], args[1], os.Environ(), stdin); err != nil {
		fmt.Fprintln(os.Stderr, "hook handler:", err)
		return 1
	}
	return 0
}

// TestToolHooksLive runs the real claude on a real account with claude's
// per-tool hooks installed the way a consumer installs them — the default
// spec plus ToolHookEntries, rendered to this binary as the hook command —
// and reads the spool back: a start and an end for a tool that succeeded, and
// a start and a failure for one that failed. It spends one short turn on a
// small model:
//
//	HW_LIVE_ACCOUNT=1 HW_LIVE_TOKEN_FILE=<file holding a `claude setup-token` token> \
//	  go test ./pkg/harness -run ToolHooksLive -v
//
// The token is read from the file and passed only as CLAUDE_CODE_OAUTH_TOKEN
// in claude's environment; it is never logged. HW_LIVE_MODEL overrides the
// model (default haiku).
func TestToolHooksLive(t *testing.T) {
	if os.Getenv("HW_LIVE_ACCOUNT") != "1" {
		t.Skip("uses a real account: set HW_LIVE_ACCOUNT=1 and HW_LIVE_TOKEN_FILE (spends one short turn)")
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
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nonce := fmt.Sprintf("%d", time.Now().UnixNano()%1e9)

	root := t.TempDir()
	wd, cfg, home, spool := filepath.Join(root, "work"), filepath.Join(root, "cfg"), filepath.Join(root, "home"), filepath.Join(root, "spool")
	for _, d := range []string{wd, cfg, home, spool} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	realWD, _ := filepath.EvalSymlinks(wd)
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
	settings := filepath.Join(cfg, "settings.json")
	writeJSON(settings, map[string]any{"skipDangerousModePermissionPrompt": true})
	hp := claudeHooks(t)
	spec := *hp.HookSpec()
	spec.Owner = "hw-live"
	spec.Events = append(append([]harness.HookEntry(nil), spec.Events...), hp.(harness.ToolHookProvider).ToolHookEntries()...)
	if err := harness.EnsureSettingsJSONHooks(settings, &spec, []string{self}, "claude"); err != nil {
		t.Fatal(err)
	}

	env := append(
		harnessenv.Cleaned(),
		"CLAUDE_CONFIG_DIR="+cfg,
		"CLAUDE_CODE_OAUTH_TOKEN="+strings.TrimSpace(string(tok)),
		"DISABLE_AUTOUPDATER=1",
		harness.EnvSpool+"="+spool,
		harness.EnvHookCwd+"="+wd,
		harness.EnvHome+"="+home,
		harness.EnvConfigDir+"="+cfg,
		hookHandlerEnv+"=1",
	)
	missing := filepath.Join(wd, "missing-"+nonce+".txt")
	prompt := "Do exactly these two steps, one tool call each, then reply DONE and nothing else. " +
		"Step 1: use the Bash tool to run exactly: echo hw-" + nonce + ". " +
		"Step 2: use the Read tool to read the file " + missing + " (it does not exist; that is expected)."
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-p", "--model", model, "--permission-mode", "bypassPermissions", prompt)
	cmd.Dir, cmd.Env = wd, env
	out, err := cmd.CombinedOutput()
	t.Logf("claude -p: %v; reply %q", err, oneLine(string(out), 200))
	if err != nil {
		t.Fatalf("claude -p failed: %v", err)
	}

	got, err := harness.ReadSpool(spool)
	if err != nil {
		t.Fatalf("ReadSpool: %v", err)
	}
	type call struct {
		pre, post, fail *transcript.Event
	}
	calls := map[string]*call{}
	for _, b := range got.Batches {
		for i := range b.Events {
			e := b.Events[i].Event
			if e.Source != transcript.SourceHook {
				continue // session markers and the Stop hook's transcript
			}
			c := calls[e.ToolName]
			if c == nil {
				c = &call{}
				calls[e.ToolName] = c
			}
			switch {
			case strings.HasPrefix(b.Receipt.Name, harness.HookArgPostToolUseFailure+"-"):
				c.fail = &e
			case strings.HasPrefix(b.Receipt.Name, harness.HookArgPostToolUse+"-"):
				c.post = &e
			case strings.HasPrefix(b.Receipt.Name, harness.HookArgPreToolUse+"-"):
				c.pre = &e
			}
			t.Logf("spool %-42s %-11s %-5s id=%s input=%s output=%q", b.Receipt.Name, e.Type, e.ToolName, e.ToolUseID,
				oneLine(string(e.ToolInput), 80), oneLine(e.Output, 80))
		}
	}
	bash, read := calls["Bash"], calls["Read"]
	if bash == nil || bash.pre == nil || bash.post == nil {
		t.Fatalf("Bash: want a start and an end, got %+v", bash)
	}
	if bash.pre.ToolUseID == "" || bash.pre.ToolUseID != bash.post.ToolUseID || !strings.Contains(string(bash.pre.ToolInput), "hw-"+nonce) {
		t.Errorf("Bash start/end = %+v / %+v: want one tool_use id and the command in the input", bash.pre, bash.post)
	}
	if !strings.Contains(bash.post.Output, "hw-"+nonce) {
		t.Errorf("Bash end output = %q, want the command's output", bash.post.Output)
	}
	if read == nil || read.pre == nil || read.fail == nil || read.post != nil {
		t.Fatalf("Read of a missing file: want a start and a failure, got %+v", read)
	}
	if read.fail.ToolUseID != read.pre.ToolUseID || read.fail.Output == "" {
		t.Errorf("Read failure = %+v, want the same tool_use id and the error", read.fail)
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
