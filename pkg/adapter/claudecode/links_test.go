package claudecode

import (
	"os/exec"
	"strings"
	"testing"
)

// A runtime whose harness list names Claude Code links the shared adapter
// and this profile, and its hook helper: none of them links hw's chat or
// screen machinery, or another harness's code.
func TestLinks_ClaudeOnly(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	const mod = "github.com/olesho/harness-wrapper/"
	// Packages linked by exact path, and each other harness's by suffix.
	exact := []string{"pkg/chat", "internal/chatcore", "pkg/wrapper", "internal/wrapcore", "pkg/harness", "pkg/screen", "pkg/turns"}
	suffixes := []string{"harness/codex", "transcript/codex", "transcript/pi", "adapter/pi", "harness/opencode", "harness/cursor"}
	for _, pkgs := range [][]string{
		{mod + "pkg/adapter"},
		{mod + "pkg/adapter/claudecode"},
		{mod + "cmd/claude-code-hook"},
	} {
		out, err := exec.Command(goTool, append([]string{"list", "-deps"}, pkgs...)...).Output()
		if err != nil {
			t.Fatalf("go list -deps %v: %v", pkgs, err)
		}
		for _, dep := range strings.Fields(string(out)) {
			rel, ok := strings.CutPrefix(dep, mod)
			if !ok {
				continue
			}
			for _, f := range exact {
				if rel == f {
					t.Errorf("%v links %s", pkgs, dep)
				}
			}
			for _, f := range suffixes {
				if strings.HasSuffix(rel, "/"+f) {
					t.Errorf("%v links %s, another harness's", pkgs, dep)
				}
			}
		}
	}
}
