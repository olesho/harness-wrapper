package harnesscore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var stopSpec = &HookSpec{
	Events: []HookEntry{{NativeEvent: "Stop", Arg: "stop"}},
	Owner:  "loom",
}

// User hook entries survive an ensure: fields the wrapper does not model
// (timeout, async, statusMessage) are kept, and a user command sharing a
// matcher group with a loom command is kept while only the loom command goes.
func TestEnsureSettingsJSONHooksPreservesUserEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	stale := RenderHookCommand([]string{"/old/loom", "hooks"}, "claude", "stop", "loom")
	in := map[string]any{
		"model": "opus",
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{"matcher": "", "hooks": []any{
					map[string]any{"type": "command", "command": "notify-send done", "timeout": 30, "async": true},
				}},
				map[string]any{"matcher": "", "statusMessage": "mixed", "hooks": []any{
					map[string]any{"type": "command", "command": "user-cleanup"},
					map[string]any{"type": "command", "command": stale},
				}},
				map[string]any{"matcher": "", "hooks": []any{
					map[string]any{"type": "command", "command": stale},
				}},
			},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSettingsJSONHooks(path, stopSpec, []string{"/new/loom", "hooks"}, "claude"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	out, err := os.ReadFile(path) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model string `json:"model"`
		Hooks struct {
			Stop []struct {
				StatusMessage string `json:"statusMessage"`
				Hooks         []struct {
					Command string `json:"command"`
					Timeout int    `json:"timeout"`
					Async   bool   `json:"async"`
				} `json:"hooks"`
			} `json:"Stop"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse result: %v\n%s", err, out)
	}
	if got.Model != "opus" {
		t.Errorf("top-level key lost: model = %q", got.Model)
	}
	stop := got.Hooks.Stop
	if len(stop) != 3 {
		t.Fatalf("Stop groups = %d, want 3 (user, mixed-minus-loom, fresh loom):\n%s", len(stop), out)
	}
	if h := stop[0].Hooks[0]; h.Command != "notify-send done" || h.Timeout != 30 || !h.Async {
		t.Errorf("user hook fields lost: %+v", h)
	}
	if stop[1].StatusMessage != "mixed" || len(stop[1].Hooks) != 1 || stop[1].Hooks[0].Command != "user-cleanup" {
		t.Errorf("mixed group = %+v, want only user-cleanup with statusMessage kept", stop[1])
	}
	if !strings.Contains(stop[2].Hooks[0].Command, "/new/loom") {
		t.Errorf("fresh loom command = %q, want /new/loom", stop[2].Hooks[0].Command)
	}
	if strings.Contains(string(out), "/old/loom") {
		t.Errorf("stale loom command not removed:\n%s", out)
	}
}

// Rewriting a user's settings.json keeps its permission bits and writes
// through a symlink instead of replacing the link with a regular file.
func TestEnsureSettingsJSONHooksKeepsModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil { //nolint:gosec // the mode under test
		t.Fatal(err)
	}
	link := filepath.Join(dir, "worktree", ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSettingsJSONHooks(link, stopSpec, []string{"/abs/loom", "hooks"}, "claude"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("settings.json is no longer a symlink (err=%v)", err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %o, want 644", fi.Mode().Perm())
	}
	data, err := os.ReadFile(target) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "/abs/loom") {
		t.Errorf("hooks not written through the symlink:\n%s", data)
	}
	leftovers, err := filepath.Glob(target + ".tmp-*")
	if err != nil || len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v (err=%v)", leftovers, err)
	}
}

// The temp file is never written through something already at its name.
func TestWriteTempSyncedRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.Symlink(victim, tmp); err != nil {
		t.Fatal(err)
	}
	if err := writeTempSynced(tmp, []byte("evil"), 0o600); err == nil {
		t.Fatal("writeTempSynced wrote through a symlink")
	}
	if data, _ := os.ReadFile(victim); string(data) != "keep" { //nolint:gosec // test temp path
		t.Errorf("victim = %q, want keep", data)
	}
}

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
