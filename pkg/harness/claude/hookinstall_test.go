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

func readSettings(t *testing.T, worktree string) map[string][]claudeHookMatcher {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(worktree, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}
	var hooks map[string][]claudeHookMatcher
	if err := json.Unmarshal(top["hooks"], &hooks); err != nil {
		t.Fatalf("hooks block invalid: %v", err)
	}
	return hooks
}

func TestEnsureConfigFreshInstall(t *testing.T) {
	wt := t.TempDir()
	if err := (hookProvider{}).EnsureConfig(wt, []string{"/abs/loom", "hooks"}); err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	hooks := readSettings(t, wt)

	// All six managed events present.
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd", "PreToolUse", "PostToolUse"} {
		if len(hooks[ev]) == 0 {
			t.Errorf("event %s missing from settings.json", ev)
		}
	}
	// PreToolUse carries BOTH the Task-matched pre-task hook AND the all-matcher
	// yield-guard — the double-matcher case.
	var sawTask, sawYieldAll bool
	for _, m := range hooks["PreToolUse"] {
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
	if !sawTask || !sawYieldAll {
		t.Errorf("PreToolUse must have both pre-task(Task) and yield-guard(all): task=%v yield=%v", sawTask, sawYieldAll)
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
		if matcherIsLoomOwned(m) {
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
		if matcherIsLoomOwned(m) {
			loomCount++
		}
	}
	if loomCount != 1 {
		t.Errorf("Stop has %d loom matchers after %d concurrent ensures, want 1", loomCount, n)
	}
}
