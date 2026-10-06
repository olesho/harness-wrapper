package harnesscore

import (
	"encoding/json"
	"fmt"
)

// Some harnesses share the SAME settings.json hook format — a two-level
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
// settings.json (the shared two-level hook format) at settingsPath, rendering each command from
// loomArgv via RenderHookCommand (harnessName is the token in the command). It
// preserves the user's hooks + unknown keys, marks loom's entries (owner), and
// refreshes the loom path each call (self-healing). flock-guarded + atomic.
func EnsureSettingsJSONHooks(settingsPath string, spec *HookSpec, loomArgv []string, harnessName string) error {
	return WithLockedFile(settingsPath, func(existing []byte) ([]byte, error) {
		return RenderSettingsJSONHooks(existing, spec, loomArgv, harnessName)
	})
}

// RenderSettingsJSONHooks is EnsureSettingsJSONHooks without the file: it
// returns existing — a settings.json's bytes, nil for none — with spec's hooks
// installed, exactly as the ensure would write them. It does no I/O, so a
// renderer that must not touch the filesystem (a Harness Adapter's Provision)
// gets the same bytes.
func RenderSettingsJSONHooks(existing []byte, spec *HookSpec, loomArgv []string, harnessName string) ([]byte, error) {
	settings, hooks, err := loadSettingsJSON(existing)
	if err != nil {
		return nil, err
	}
	// Group managed entries by native event FIRST: one native event can carry
	// several loom matchers (e.g. Claude's PreToolUse carries the yield-guard
	// and, for a consumer that adds them, the per-tool hook), and the upsert
	// removes all loom-owned matchers for an event before re-adding — so they
	// must be added together or the second would delete the first.
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

// upsertSettingsHooks replaces all loom-owned commands for nativeEvent with the
// fresh loomMatchers, preserving every non-loom (user) entry.
//
// Matcher groups are handled as raw JSON, never decoded into
// SettingsHookMatcher: a round-trip through that struct would silently drop
// every field it does not model (timeout, async, statusMessage, …). A group
// with no loom command is kept byte-for-byte; a mixed group loses only its
// loom commands; a group is dropped only when nothing is left in it.
func upsertSettingsHooks(hooks map[string]json.RawMessage, nativeEvent string, loomMatchers []SettingsHookMatcher) error {
	var groups []json.RawMessage
	if raw, ok := hooks[nativeEvent]; ok {
		if err := json.Unmarshal(raw, &groups); err != nil {
			return fmt.Errorf("hooks: parse %s entries: %w", nativeEvent, err)
		}
	}
	kept := make([]json.RawMessage, 0, len(groups)+len(loomMatchers))
	for _, g := range groups {
		ng, keep, err := stripLoomCommands(g)
		if err != nil {
			return fmt.Errorf("hooks: rewrite %s entry: %w", nativeEvent, err)
		}
		if keep {
			kept = append(kept, ng)
		}
	}
	for _, m := range loomMatchers {
		data, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("hooks: marshal %s entries: %w", nativeEvent, err)
		}
		kept = append(kept, data)
	}
	data, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("hooks: marshal %s entries: %w", nativeEvent, err)
	}
	hooks[nativeEvent] = data
	return nil
}

// stripLoomCommands removes the loom-managed commands from one matcher group.
// A group with no loom command comes back unchanged; one left with no command
// at all is dropped (keep=false); otherwise the group is re-encoded with only
// the user's commands and every other field untouched. A shape this code does
// not recognise is kept as-is rather than rejected.
func stripLoomCommands(group json.RawMessage) (out json.RawMessage, keep bool, err error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(group, &fields) != nil {
		return group, true, nil
	}
	var cmds []json.RawMessage
	if raw, ok := fields["hooks"]; !ok || json.Unmarshal(raw, &cmds) != nil {
		return group, true, nil
	}
	userCmds := make([]json.RawMessage, 0, len(cmds))
	for _, c := range cmds {
		var cmd struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(c, &cmd) == nil && IsManagedHookCommand(cmd.Command) {
			continue
		}
		userCmds = append(userCmds, c)
	}
	switch {
	case len(userCmds) == len(cmds):
		return group, true, nil
	case len(userCmds) == 0:
		return nil, false, nil
	}
	if fields["hooks"], err = json.Marshal(userCmds); err != nil {
		return nil, false, err
	}
	if out, err = json.Marshal(fields); err != nil {
		return nil, false, err
	}
	return out, true, nil
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
