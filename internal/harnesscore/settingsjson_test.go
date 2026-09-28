package harnesscore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// RenderSettingsJSONHooks gives exactly the bytes EnsureSettingsJSONHooks
// writes, for a fresh file and for one holding the user's own settings.
func TestRenderSettingsJSONHooksMatchesEnsure(t *testing.T) {
	spec := &HookSpec{
		ConfigPath: ".claude/settings.json",
		Owner:      "test",
		Events: []HookEntry{
			{NativeEvent: "Stop", Arg: "stop"},
			{NativeEvent: "PreToolUse", Matcher: "*", Arg: HookArgPreToolUse},
		},
		Yield: &HookEntry{NativeEvent: "PreToolUse", Matcher: "Bash", Arg: "yield"},
	}
	argv := []string{"/opt/hw/bin/hook"}
	for _, existing := range [][]byte{
		nil,
		[]byte(`{"model":"x","hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo mine"}]}]}}`),
	} {
		file := filepath.Join(t.TempDir(), "settings.json")
		if existing != nil {
			if err := os.WriteFile(file, existing, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := EnsureSettingsJSONHooks(file, spec, argv, "claude"); err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RenderSettingsJSONHooks(existing, spec, argv, "claude")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("rendered\n%s\nensured\n%s", got, want)
		}
		again, err := RenderSettingsJSONHooks(got, spec, argv, "claude")
		if err != nil || !bytes.Equal(again, got) {
			t.Errorf("rendering over its own result changed it: %v\n%s", err, again)
		}
	}
}
