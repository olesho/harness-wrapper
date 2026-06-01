// Claude Code settings.json hook installer. Ports tysonthomas9's proven
// idempotent two-level merge (preserves user hooks, unknown top-level keys, and
// unknown hook types) and adds the wrapper's hardening (reviews #5/#10): the
// rendered command is shell-guarded + owner-marked, the loom path is refreshed
// each call, and the write is flock-guarded + atomic.
package claude

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

// claudeHookMatcher is Claude's two-level hook grouping:
// [{"matcher":"","hooks":[{"type":"command","command":"..."}]}].
type claudeHookMatcher struct {
	Matcher string            `json:"matcher"`
	Hooks   []claudeHookEntry `json:"hooks"`
}

type claudeHookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// EnsureConfig installs loom's hooks into <worktree>/.claude/settings.json. It
// is idempotent (same loom path → identical output) and self-healing (a changed
// loom path replaces the stale managed entry), and never disturbs the user's
// own hooks or other settings keys.
func (h hookProvider) EnsureConfig(worktreePath string, loomArgv []string) error {
	spec := h.HookSpec()
	settingsPath := filepath.Join(worktreePath, spec.ConfigPath)

	return harness.WithLockedFile(settingsPath, func(existing []byte) ([]byte, error) {
		settings, hooks, err := loadClaudeSettings(existing)
		if err != nil {
			return nil, err
		}
		// Group managed entries by native event FIRST: a single native event can
		// carry several loom matchers (PreToolUse has the Task-matched pre-task
		// AND the all-matcher yield-guard), and the upsert removes all loom-owned
		// matchers for an event before re-adding — so they must be added together
		// or the second would delete the first.
		byEvent := map[string][]claudeHookMatcher{}
		add := func(e harness.HookEntry) {
			cmd := harness.RenderHookCommand(loomArgv, "claude", e.Arg, spec.Owner)
			byEvent[e.NativeEvent] = append(byEvent[e.NativeEvent], claudeHookMatcher{
				Matcher: e.Matcher,
				Hooks:   []claudeHookEntry{{Type: "command", Command: cmd}},
			})
		}
		for _, e := range spec.Events {
			add(e)
		}
		if spec.Yield != nil {
			add(*spec.Yield)
		}
		for nativeEvent, loomMatchers := range byEvent {
			if err := upsertClaudeHooks(hooks, nativeEvent, loomMatchers); err != nil {
				return nil, err
			}
		}
		return marshalClaudeSettings(settings, hooks)
	})
}

// loadClaudeSettings parses the settings file into (top-level map, hooks map),
// tolerating an absent/empty file (fresh maps).
func loadClaudeSettings(data []byte) (settings, hooks map[string]json.RawMessage, err error) {
	settings = map[string]json.RawMessage{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return nil, nil, fmt.Errorf("claude hooks: parse settings.json: %w", err)
		}
	}
	hooks = map[string]json.RawMessage{}
	if raw, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, nil, fmt.Errorf("claude hooks: parse hooks block: %w", err)
		}
	}
	return settings, hooks, nil
}

// upsertClaudeHooks replaces all loom-owned matchers for nativeEvent with the
// given fresh loomMatchers, preserving every non-loom (user) matcher. Removing
// then re-adding refreshes the loom path and keeps the operation idempotent.
func upsertClaudeHooks(hooks map[string]json.RawMessage, nativeEvent string, loomMatchers []claudeHookMatcher) error {
	var matchers []claudeHookMatcher
	if raw, ok := hooks[nativeEvent]; ok {
		if err := json.Unmarshal(raw, &matchers); err != nil {
			return fmt.Errorf("claude hooks: parse %s entries: %w", nativeEvent, err)
		}
	}
	kept := matchers[:0]
	for _, m := range matchers {
		if !matcherIsLoomOwned(m) {
			kept = append(kept, m)
		}
	}
	kept = append(kept, loomMatchers...)
	data, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("claude hooks: marshal %s entries: %w", nativeEvent, err)
	}
	hooks[nativeEvent] = data
	return nil
}

// matcherIsLoomOwned reports whether a matcher group contains a loom-managed
// command (so it should be replaced on re-ensure).
func matcherIsLoomOwned(m claudeHookMatcher) bool {
	for _, e := range m.Hooks {
		if harness.IsManagedHookCommand(e.Command) {
			return true
		}
	}
	return false
}

// marshalClaudeSettings writes hooks back under settings["hooks"] and renders
// the whole settings object (stable, indented, trailing newline).
func marshalClaudeSettings(settings, hooks map[string]json.RawMessage) ([]byte, error) {
	hooksJSON, err := json.Marshal(hooks)
	if err != nil {
		return nil, fmt.Errorf("claude hooks: marshal hooks: %w", err)
	}
	settings["hooks"] = hooksJSON
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("claude hooks: marshal settings: %w", err)
	}
	return append(out, '\n'), nil
}
