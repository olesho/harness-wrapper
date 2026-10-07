// Package wrapper supervises an external CLI agent harness running under
// a pseudoterminal. It runs the harness, observes its output and
// lifecycle, and returns a normalized status when the harness exits or
// is terminated.
//
// It began with only terminal states (idle, failed, interrupted,
// unknown) and now also recognizes a small set of actionable,
// non-terminal harness states from recent output. The wrapper does not
// persist state; callers own persistence.
//
// Termination is process-GROUP scoped on unix: the harness and every tool
// subprocess it spawned are signalled together. This is sound only because
// sessions start under pty.Start, which sets SysProcAttr.Setsid — the harness
// is a session leader whose PGID equals its PID, and its descendants inherit
// that group. Signalling the harness PID alone left those descendants running,
// reparented to PID 1, long after the run was declared over. The escalation to
// SIGKILL is owned by the session through to the group being empty, so a
// harness that exits on SIGTERM cannot strand a child that ignores it; Wait
// returns after that cleanup. A descendant that calls setsid() itself still
// escapes; supervising that is the caller's job.
//
// Concurrency: the package is safe for multiple concurrent Run calls
// only in headless mode (non-TTY stdin/stdout). Concurrent foreground
// Run calls produce undefined behavior because they would compete for
// terminal control.
//
// The implementation lives in internal/wrapcore, which names no harness; every
// exported identifier here is the identical one there. This package adds the
// built-in harnesses' classifier patterns, so callers of pkg/wrapper classify
// exactly as they always have.
package wrapper

//go:generate go run ../../internal/facadegen -core ../../internal/wrapcore -import github.com/olesho/harness-wrapper/internal/wrapcore -alias wrapcore -pkg wrapper -skip RegisterPatterns -o forward.go

import (
	"github.com/olesho/harness-wrapper/internal/wrapcore"
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/claude"
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/codex"
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/cursor"
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/opencode"
	"github.com/olesho/harness-wrapper/pkg/harnessname"
)

// The built-in harnesses' classifier patterns, under the names Config.Harness
// has always accepted for them.
func init() {
	wrapcore.RegisterPatterns(claude.Patterns, harnessname.Spellings(harnessname.ClaudeCode)...)
	wrapcore.RegisterPatterns(codex.Patterns, harnessname.Codex)
	wrapcore.RegisterPatterns(cursor.Patterns, harnessname.Cursor)
	wrapcore.RegisterPatterns(opencode.Patterns, harnessname.OpenCode)
}
