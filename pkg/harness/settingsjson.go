package harness

import (
	"encoding/json"
	"fmt"
)

// Claude and Gemini share the SAME settings.json hook format — a two-level
// grouping of matcher → command entries:
//
//	{"hooks": {"<Event>": [{"matcher": "<m>", "hooks": [{"type":"command","command":"..."}]}]}}
//
// so the merge lives here, generic, and each harness's EnsureConfig is a thin
// call that supplies its config path + native event names (the only per-harness
// variation, per the plan). Per-harness payload parsing stays in the harness
// package; only the config FILE shape is shared.

// SettingsHookMatcher is one matcher group in a settings.json hook event.
type SettingsHookMatcher struct {
	Matcher string            `json:"matcher"`
	Hooks   []SettingsHookCmd `json:"hooks"`
}

// SettingsHookCmd is a single command hook within a matcher group.
type SettingsHookCmd struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// EnsureSettingsJSONHooks idempotently + atomically installs spec's hooks into a
// Claude/Gemini-style settings.json at settingsPath, rendering each command from
// loomArgv via RenderHookCommand (harnessName is the token in the command). It
// preserves the user's hooks + unknown keys, marks loom's entries (owner), and
// refreshes the loom path each call (self-healing). flock-guarded + atomic.
func EnsureSettingsJSONHooks(settingsPath string, spec *HookSpec, loomArgv []string, harnessName string) error {
	return WithLockedFile(settingsPath, func(existing []byte) ([]byte, error) {
		settings, hooks, err := loadSettingsJSON(existing)
		if err != nil {
			return nil, err
		}
		// Group managed entries by native event FIRST: one native event can carry
		// several loom matchers (e.g. Claude's PreToolUse has the Task-matched
		// capture hook AND the all-matcher yield-guard), and the upsert removes
		// all loom-owned matchers for an event before re-adding — so they must be
		// added together or the second would delete the first.
		byEvent := map[string][]SettingsHookMatcher{}
		add := func(e HookEntry) {
			cmd := RenderHookCommand(loomArgv, harnessName, e.Arg, spec.Owner)
			byEvent[e.NativeEvent] = append(byEvent[e.NativeEvent], SettingsHookMatcher{
				Matcher: e.Matcher,
				Hooks:   []SettingsHookCmd{{Type: "command", Command: cmd}},
			})
		}
		for _, e := range spec.Events {
			add(e)
		}
		if spec.Yield != nil {
			add(*spec.Yield)
		}
		for nativeEvent, loomMatchers := range byEvent {
			if err := upsertSettingsHooks(hooks, nativeEvent, loomMatchers); err != nil {
				return nil, err
			}
		}
		return marshalSettingsJSON(settings, hooks)
	})
}

// loadSettingsJSON parses the settings file into (top-level map, hooks map),
// tolerating an absent/empty file (fresh maps).
func loadSettingsJSON(data []byte) (settings, hooks map[string]json.RawMessage, err error) {
	settings = map[string]json.RawMessage{}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &settings); err != nil {
			return nil, nil, fmt.Errorf("hooks: parse settings.json: %w", err)
		}
	}
	hooks = map[string]json.RawMessage{}
	if raw, ok := settings["hooks"]; ok {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return nil, nil, fmt.Errorf("hooks: parse hooks block: %w", err)
		}
	}
	return settings, hooks, nil
}

// upsertSettingsHooks replaces all loom-owned matchers for nativeEvent with the
// fresh loomMatchers, preserving every non-loom (user) matcher.
func upsertSettingsHooks(hooks map[string]json.RawMessage, nativeEvent string, loomMatchers []SettingsHookMatcher) error {
	var matchers []SettingsHookMatcher
	if raw, ok := hooks[nativeEvent]; ok {
		if err := json.Unmarshal(raw, &matchers); err != nil {
			return fmt.Errorf("hooks: parse %s entries: %w", nativeEvent, err)
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
		return fmt.Errorf("hooks: marshal %s entries: %w", nativeEvent, err)
	}
	hooks[nativeEvent] = data
	return nil
}

// matcherIsLoomOwned reports whether a matcher group contains a loom-managed
// command (so it is replaced on re-ensure).
func matcherIsLoomOwned(m SettingsHookMatcher) bool {
	for _, e := range m.Hooks {
		if IsManagedHookCommand(e.Command) {
			return true
		}
	}
	return false
}

// marshalSettingsJSON writes hooks back under settings["hooks"] and renders the
// whole object (stable, indented, trailing newline).
func marshalSettingsJSON(settings, hooks map[string]json.RawMessage) ([]byte, error) {
	hooksJSON, err := json.Marshal(hooks)
	if err != nil {
		return nil, fmt.Errorf("hooks: marshal hooks: %w", err)
	}
	settings["hooks"] = hooksJSON
	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("hooks: marshal settings: %w", err)
	}
	return append(out, '\n'), nil
}
