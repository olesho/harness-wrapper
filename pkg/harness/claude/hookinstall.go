// Claude Code settings.json hook installer. The two-level merge itself is
// generic (shared by any harness using the same settings.json format) in
// pkg/harness; this is the thin Claude-specific entry that supplies the
// config path + harness name.
package claude

import (
	"path/filepath"

	"github.com/olesho/harness-wrapper/pkg/harness"
)

// EnsureConfig installs loom's hooks into <worktree>/.claude/settings.json via
// the shared settings.json merge: idempotent (same loom path →
// identical output), self-healing (a changed loom path replaces the stale
// managed entry), preserving the user's own hooks and other settings keys.
func (h hookProvider) EnsureConfig(worktreePath string, loomArgv []string) error {
	spec := h.HookSpec()
	settingsPath := filepath.Join(worktreePath, spec.ConfigPath)
	return harness.EnsureSettingsJSONHooks(settingsPath, spec, loomArgv, "claude")
}
