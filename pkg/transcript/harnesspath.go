package transcript

import "path/filepath"

// ResolveHarnessPath makes path absolute the way the harness that owns it
// resolves it: relative to the HARNESS CHILD's working directory, workingDir.
//
// Claude Code takes CLAUDE_CONFIG_DIR verbatim (never made absolute) and Codex
// canonicalizes CODEX_HOME against its own cwd, so a relative config root — and
// every transcript path claude builds from one, including the paths it hands
// its hooks — names a location under the child's cwd. Resolving it against the
// wrapper's own cwd, which is what an unqualified file operation does, reads a
// different directory whenever the two differ.
//
// An empty workingDir means the child inherits this process's cwd, so the
// process cwd is the base. An empty or already-absolute path is returned
// unchanged. Callers that pass a relative workingDir get it resolved against
// the process cwd, exactly as exec resolves a relative Cmd.Dir.
func ResolveHarnessPath(path, workingDir string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	joined := filepath.Join(workingDir, path)
	if abs, err := filepath.Abs(joined); err == nil {
		return abs
	}
	return joined
}
