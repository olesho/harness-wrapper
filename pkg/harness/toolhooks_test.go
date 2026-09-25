package harness_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
	_ "github.com/olesho/harness-wrapper/pkg/harness/all" // register claude
	"github.com/olesho/harness-wrapper/pkg/transcript"
)

// renderedArg picks the hook argument out of a rendered command: the token
// after 'claude'.
var renderedArg = regexp.MustCompile(`'\\''claude'\\'' '\\''([a-z-]+)'\\''`)

// claudeHooks returns claude's static hook provider, as a hook subprocess or a
// consumer building its spec obtains it.
func claudeHooks(t *testing.T) harness.HookProvider {
	t.Helper()
	p, ok := harness.For("claude")
	if !ok {
		t.Fatal("no claude profile registered")
	}
	shp, ok := p.(harness.StaticHookProfile)
	if !ok {
		t.Fatal("claude has no static hook provider")
	}
	return shp.StaticHookProvider()
}

// TestToolHooksInstallAlongsideTheSpec: a consumer that adds claude's
// per-tool entries to its spec gets them installed next to the Task-matched
// and yield hooks of the same native events, idempotently, with its own
// hook command — the form agentd uses (`agentd-proxy hooks claude <arg>`).
func TestToolHooksInstallAlongsideTheSpec(t *testing.T) {
	hp := claudeHooks(t)
	th, ok := hp.(harness.ToolHookProvider)
	if !ok {
		t.Fatal("claude's hook provider is not a ToolHookProvider")
	}
	spec := *hp.HookSpec()
	spec.Owner = "agentd"
	spec.Events = append(append([]harness.HookEntry(nil), spec.Events...), th.ToolHookEntries()...)

	settings := filepath.Join(t.TempDir(), "profile", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	user := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo user"}]}]},"model":"x"}`
	if err := os.WriteFile(settings, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := []string{"/usr/local/lib/agentd/agentd-proxy", "hooks"}
	for i := 0; i < 2; i++ { // the second ensure changes nothing
		if err := harness.EnsureSettingsJSONHooks(settings, &spec, argv, "claude"); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var top struct {
		Hooks map[string][]harness.SettingsHookMatcher `json:"hooks"`
		Model string                                   `json:"model"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	if top.Model != "x" {
		t.Error("an unrelated setting was lost")
	}
	type key struct{ native, matcher, arg string }
	got := map[key]int{}
	userHooks := 0
	for native, matchers := range top.Hooks {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if !harness.IsManagedHookCommand(h.Command) {
					userHooks++
					continue
				}
				// The inner command is single-quoted inside sh -c, so each
				// quote around a token reads '\'' in the rendered string.
				sub := renderedArg.FindStringSubmatch(h.Command)
				if sub == nil || !strings.Contains(h.Command, `'\''/usr/local/lib/agentd/agentd-proxy'\'' '\''hooks'\''`) || !strings.HasSuffix(h.Command, "# harness-wrapper-hook:agentd") {
					t.Errorf("command not rendered from the consumer's argv and owner: %s", h.Command)
					continue
				}
				arg := sub[1]
				got[key{native, m.Matcher, arg}]++
			}
		}
	}
	if userHooks != 1 {
		t.Errorf("%d user hooks survived, want 1", userHooks)
	}
	for _, k := range []key{
		{"PreToolUse", "Task", "pre-task"},
		{"PreToolUse", "", harness.HookArgPreToolUse},
		{"PreToolUse", "", "yield-guard"},
		{"PostToolUse", "Task", "post-task"},
		{"PostToolUse", "", harness.HookArgPostToolUse},
		{"PostToolUseFailure", "", harness.HookArgPostToolUseFailure},
	} {
		if got[k] != 1 {
			t.Errorf("%+v installed %d times, want once (all: %v)", k, got[k], got)
		}
	}
}

// TestToolHooksSpoolForReadAndAck runs each per-tool hook through
// HandleHookEvent, as the fired hook command does, and reads the spool back:
// one file per hook, named after its argument, holding the hook-sourced event.
func TestToolHooksSpoolForReadAndAck(t *testing.T) {
	spool := t.TempDir()
	env := []string{harness.EnvSpool + "=" + spool, harness.EnvHome + "=" + t.TempDir(), harness.EnvHookCwd + "=/wt"}
	payload := func(fields map[string]any) []byte {
		p := map[string]any{"session_id": "sess-t", "transcript_path": "/x/sess-t.jsonl", "cwd": "/wt"}
		for k, v := range fields {
			p[k] = v
		}
		b, _ := json.Marshal(p)
		return b
	}
	for _, c := range []struct {
		arg    string
		fields map[string]any
	}{
		{harness.HookArgPreToolUse, map[string]any{"tool_name": "Bash", "tool_use_id": "toolu_a", "tool_input": map[string]any{"command": "ls"}}},
		{harness.HookArgPostToolUse, map[string]any{"tool_name": "Bash", "tool_use_id": "toolu_a", "tool_response": map[string]any{"stdout": "x"}}},
		{harness.HookArgPostToolUseFailure, map[string]any{"tool_name": "Read", "tool_use_id": "toolu_b", "error": "no such file"}},
	} {
		if _, err := harness.HandleHookEvent("claude", c.arg, env, payload(c.fields)); err != nil {
			t.Fatalf("HandleHookEvent(%s): %v", c.arg, err)
		}
	}
	got, err := harness.ReadSpool(spool)
	if err != nil {
		t.Fatalf("ReadSpool: %v", err)
	}
	if len(got.Batches) != 3 {
		t.Fatalf("ReadSpool = %d batches, want 3: %+v", len(got.Batches), got)
	}
	byArg := map[string]transcript.Event{}
	for _, b := range got.Batches {
		if len(b.Events) != 1 {
			t.Fatalf("%s: %d events, want 1", b.Receipt.Name, len(b.Events))
		}
		// The longest argument first: a failure's file also starts with
		// "post-tool-use-".
		for _, arg := range []string{harness.HookArgPostToolUseFailure, harness.HookArgPostToolUse, harness.HookArgPreToolUse} {
			if strings.HasPrefix(b.Receipt.Name, arg+"-") {
				byArg[arg] = b.Events[0].Event
				break
			}
		}
	}
	pre, post, fail := byArg[harness.HookArgPreToolUse], byArg[harness.HookArgPostToolUse], byArg[harness.HookArgPostToolUseFailure]
	if pre.Type != transcript.EventToolUse || pre.ToolUseID != "toolu_a" || string(pre.ToolInput) != `{"command":"ls"}` {
		t.Errorf("pre-tool-use event = %+v", pre)
	}
	if post.Type != transcript.EventToolResult || post.Output != `{"stdout":"x"}` {
		t.Errorf("post-tool-use event = %+v", post)
	}
	if fail.Type != transcript.EventToolResult || fail.ToolName != "Read" || fail.Output != "no such file" {
		t.Errorf("post-tool-use-failure event = %+v", fail)
	}
	for arg, e := range byArg {
		// The durable spool form keeps what the authority filter and dedup key on.
		if e.Source != transcript.SourceHook || !strings.HasPrefix(e.NativeID, "hook:"+arg+":") {
			t.Errorf("%s: source %q, native id %q", arg, e.Source, e.NativeID)
		}
	}
	receipts := make([]harness.SpoolReceipt, 0, len(got.Batches))
	for _, b := range got.Batches {
		receipts = append(receipts, b.Receipt)
	}
	if err := harness.AckSpool(spool, receipts...); err != nil {
		t.Fatalf("AckSpool: %v", err)
	}
	if left, _ := harness.ReadSpool(spool); len(left.Batches) != 0 {
		t.Errorf("%d batches left after AckSpool", len(left.Batches))
	}
}
