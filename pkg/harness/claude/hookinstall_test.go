package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

func readSettings(t *testing.T, worktree string) map[string][]harness.SettingsHookMatcher {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(worktree, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	var hooks map[string][]harness.SettingsHookMatcher
	if err := json.Unmarshal(top["hooks"], &hooks); err != nil {
		t.Fatalf("hooks block invalid: %v", err)
	}
	return hooks
}

// loomOwned reports whether a matcher group carries a loom-managed command
// (the test's local replacement for the now-internal harness helper).
func loomOwned(m harness.SettingsHookMatcher) bool {
	for _, e := range m.Hooks {
		if harness.IsManagedHookCommand(e.Command) {
			return true
		}
	}
	return false
}

// scanPreToolUse inspects the PreToolUse matchers, asserting every command is
// owner-marked and reporting whether a Task pre-task hook (retired, ADR-011)
// and the all-matcher yield-guard hook are present.
func scanPreToolUse(t *testing.T, matchers []harness.SettingsHookMatcher) (sawTask, sawYieldAll bool) {
	t.Helper()
	for _, m := range matchers {
		for _, e := range m.Hooks {
			if m.Matcher == "Task" && strings.Contains(e.Command, "pre-task") {
				sawTask = true
			}
			if m.Matcher == "" && strings.Contains(e.Command, "yield-guard") {
				sawYieldAll = true
			}
			if !harness.IsManagedHookCommand(e.Command) {
				t.Errorf("PreToolUse command not owner-marked: %s", e.Command)
			}
		}
	}
	return sawTask, sawYieldAll
}

func TestEnsureConfigFreshInstall(t *testing.T) {
	wt := t.TempDir()
	if err := (hookProvider{}).EnsureConfig(wt, []string{"/abs/loom", "hooks"}); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	hooks := readSettings(t, wt)

	// The lifecycle and subagent events, PreToolUse for the yield guard and
	// PostToolUse for the subagent's transcript.
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd", "SubagentStart", "SubagentStop", "PreToolUse", "PostToolUse"} {
		if len(hooks[ev]) == 0 {
			t.Errorf("event %s missing from settings.json", ev)
		}
	}
	// A subagent's start is its own hook (ADR-011): PreToolUse carries no
	// Task-matched pre-task, only the yield guard.
	sawTask, sawYieldAll := scanPreToolUse(t, hooks["PreToolUse"])
	if sawTask || !sawYieldAll {
		t.Errorf("PreToolUse must carry only the yield-guard(all): task=%v yield=%v", sawTask, sawYieldAll)
	}
	if post := hooks["PostToolUse"]; len(post) != 1 || post[0].Matcher != "Task" || !strings.Contains(post[0].Hooks[0].Command, "post-task") {
		t.Errorf("PostToolUse = %+v, want the Task-matched post-task", post)
	}
	// Commands are shell-guarded.
	if !strings.Contains(hooks["Stop"][0].Hooks[0].Command, "HW_EVENT_SPOOL") {
		t.Errorf("Stop command not shell-guarded: %s", hooks["Stop"][0].Hooks[0].Command)
	}
}

func TestEnsureConfigIdempotent(t *testing.T) {
	wt := t.TempDir()
	argv := []string{"/abs/loom", "hooks"}
	if err := (hookProvider{}).EnsureConfig(wt, argv); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(wt, ".claude", "settings.json")
	first, _ := os.ReadFile(path)
	if err := (hookProvider{}).EnsureConfig(wt, argv); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("re-ensure not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestEnsureConfigPreservesUserHooks(t *testing.T) {
	wt := t.TempDir()
	dir := filepath.Join(wt, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-seed a user hook under Stop and a user PreToolUse matcher + an unrelated
	// top-level settings key.
	seed := `{
  "model": "claude-sonnet-4-6",
  "hooks": {
    "Stop": [{"matcher":"","hooks":[{"type":"command","command":"my-own-stop-hook"}]}],
    "PreToolUse": [{"matcher":"Bash","hooks":[{"type":"command","command":"my-bash-guard"}]}]
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (hookProvider{}).EnsureConfig(wt, []string{"/abs/loom", "hooks"}); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	s := string(data)
	if !strings.Contains(s, "my-own-stop-hook") {
		t.Error("user Stop hook was dropped")
	}
	if !strings.Contains(s, "my-bash-guard") {
		t.Error("user PreToolUse(Bash) hook was dropped")
	}
	if !strings.Contains(s, `"claude-sonnet-4-6"`) {
		t.Error("unrelated top-level settings key was dropped")
	}
	// And loom's hooks are present alongside.
	hooks := readSettings(t, wt)
	if len(hooks["Stop"]) != 2 { // user + loom
		t.Errorf("Stop should have user + loom matchers, got %d", len(hooks["Stop"]))
	}
}

func TestEnsureConfigRefreshesLoomPath(t *testing.T) {
	wt := t.TempDir()
	if err := (hookProvider{}).EnsureConfig(wt, []string{"/old/loom", "hooks"}); err != nil {
		t.Fatal(err)
	}
	if err := (hookProvider{}).EnsureConfig(wt, []string{"/new/loom", "hooks"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(wt, ".claude", "settings.json"))
	s := string(data)
	if strings.Contains(s, "/old/loom") {
		t.Error("stale loom path not refreshed")
	}
	if !strings.Contains(s, "/new/loom") {
		t.Error("new loom path not written")
	}
	// No duplicate loom matchers: Stop has exactly one loom entry.
	hooks := readSettings(t, wt)
	loomCount := 0
	for _, m := range hooks["Stop"] {
		if loomOwned(m) {
			loomCount++
		}
	}
	if loomCount != 1 {
		t.Errorf("Stop has %d loom matchers after re-ensure, want 1 (refresh, not append)", loomCount)
	}
}

func TestEnsureConfigConcurrent(t *testing.T) {
	wt := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := (hookProvider{}).EnsureConfig(wt, []string{"/abs/loom", "hooks"}); err != nil {
				t.Errorf("concurrent EnsureConfig: %v", err)
			}
		}()
	}
	wg.Wait()

	// Result is one valid file with exactly one loom matcher per simple event.
	hooks := readSettings(t, wt)
	loomCount := 0
	for _, m := range hooks["Stop"] {
		if loomOwned(m) {
			loomCount++
		}
	}
	if loomCount != 1 {
		t.Errorf("Stop has %d loom matchers after %d concurrent ensures, want 1", loomCount, n)
	}
}

// TestEnsureConfigMigratesTaskHooks: a settings.json an earlier
// harness-wrapper wrote — its Task-matched pre-task / post-task entries next
// to the user's own hooks — comes out of one ensure with pre-task gone, the
// native subagent hooks in, post-task refreshed, and the user's hooks
// untouched (ADR-011). pre-task goes because the ensure rewrites every
// managed entry under PreToolUse, where the yield guard lives.
func TestEnsureConfigMigratesTaskHooks(t *testing.T) {
	wt := t.TempDir()
	old := func(arg string) string {
		return harness.RenderHookCommand([]string{"/old/loom", "hooks"}, "claude", arg, hookOwner)
	}
	entry := func(matcher, cmd string) map[string]any {
		return map[string]any{"matcher": matcher, "hooks": []map[string]string{{"type": "command", "command": cmd}}}
	}
	prev := map[string]any{"hooks": map[string]any{
		"PreToolUse":  []any{entry("Task", old("pre-task")), entry("", old("yield-guard")), entry("Bash", "echo user-pre")},
		"PostToolUse": []any{entry("Task", old("post-task")), entry("Edit", "echo user-post")},
		"Stop":        []any{entry("", old("stop"))},
	}}
	if err := os.MkdirAll(filepath.Join(wt, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(prev)
	if err := os.WriteFile(filepath.Join(wt, ".claude", "settings.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (hookProvider{}).EnsureConfig(wt, []string{"/abs/loom", "hooks"}); err != nil {
		t.Fatal(err)
	}
	hooks := readSettings(t, wt)
	for ev, matchers := range hooks {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if strings.Contains(h.Command, "pre-task") || strings.Contains(h.Command, "/old/loom") {
					t.Errorf("%s still runs a retired or stale hook: %s", ev, h.Command)
				}
			}
		}
	}
	for _, ev := range []string{"SubagentStart", "SubagentStop"} {
		if len(hooks[ev]) != 1 {
			t.Errorf("%s = %+v, want the native subagent hook", ev, hooks[ev])
		}
	}
	userCmds := map[string]bool{}
	for _, matchers := range hooks {
		for _, m := range matchers {
			for _, h := range m.Hooks {
				if !harness.IsManagedHookCommand(h.Command) {
					userCmds[m.Matcher+":"+h.Command] = true
				}
			}
		}
	}
	if !userCmds["Bash:echo user-pre"] || !userCmds["Edit:echo user-post"] || len(userCmds) != 2 {
		t.Errorf("user hooks after the migration: %v", userCmds)
	}
	if post := hooks["PostToolUse"]; len(post) != 2 {
		t.Errorf("PostToolUse = %+v, want the user's hook and the refreshed post-task", post)
	}
}
