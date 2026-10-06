package chatcore

import (
	// The built-in harnesses' classifier patterns, as pkg/wrapper registers
	// them: the conversations these tests drive classify as pkg/chat's do.
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/turns/generic"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/claudecode"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/codex"
	"github.com/olesho/harness-wrapper/pkg/turns/harness/opencode"
	_ "github.com/olesho/harness-wrapper/pkg/wrapper"
)

// The tests here drive every built-in harness, which pkg/chat registers in
// production. Register the same set, under the same names.
func init() {
	RegisterAdapter("codex", func() turns.Adapter { return codex.New() })
	RegisterAdapter("claude-code", func() turns.Adapter { return claudecode.New() })
	RegisterAdapter("opencode", func() turns.Adapter { return opencode.New() })
	RegisterAdapter("generic", func() turns.Adapter { return generic.New() })
	RegisterAdapter("", func() turns.Adapter { return generic.New() })
}

// readyForInput and requiresPromptReadiness answer for a harness by name, the
// way these tests have always asked; production asks the conversation's own
// adapter.
func readyForInput(harness, text string) bool {
	return readyOn(adapterNamed(harness), harness, text)
}

func requiresPromptReadiness(harness string) bool {
	return requiresReadiness(adapterNamed(harness))
}

// testAdapter returns a new adapter for a harness a test names, or nil for a
// name no adapter is registered under — what a hand-assembled Conversation
// needs to read its harness's screens as an opened one does.
func testAdapter(harness string) turns.Adapter {
	a, err := resolveAdapter(harness)
	if err != nil {
		return nil
	}
	return a
}
