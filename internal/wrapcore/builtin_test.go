package wrapcore

import (
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/claude"
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/codex"
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/cursor"
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/opencode"
	"github.com/olesho/harness-wrapper/internal/wrapcore/harness/pi"
)

// The tests here exercise the built-in classifiers, which pkg/wrapper
// registers in production. Register the same set, the same way.
func init() {
	RegisterPatterns(claude.Patterns, "claude", "claude-code")
	RegisterPatterns(codex.Patterns, "codex")
	RegisterPatterns(cursor.Patterns, "cursor")
	RegisterPatterns(opencode.Patterns, "opencode")
	RegisterPatterns(pi.Patterns, "pi")
}
