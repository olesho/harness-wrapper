package codex

import (
	"os/exec"
	"strings"
	"testing"
)

// A runtime whose harness list names Codex links the shared adapter and
// this profile: neither links hw's chat or screen machinery, or another
// harness's code.
func TestLinks_CodexOnly(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	const mod = "github.com/olesho/harness-wrapper/"
	// Packages linked by exact path, and each other harness's by suffix.
	exact := []string{"pkg/chat", "internal/chatcore", "pkg/wrapper", "internal/wrapcore", "pkg/harness", "pkg/screen", "pkg/turns"}
	suffixes := []string{
		"adapter/claudecode", "harness/claude", "transcript/claudecode", "harness/codex",
		"harness/pi", "transcript/pi", "harness/opencode", "harness/cursor",
	}
	out, err := exec.Command(goTool, "list", "-deps", mod+"pkg/adapter/codex").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		rel, ok := strings.CutPrefix(dep, mod)
		if !ok {
			continue
		}
		for _, f := range exact {
			if rel == f {
				t.Errorf("the Codex profile links %s", dep)
			}
		}
		for _, f := range suffixes {
			if strings.HasSuffix(rel, "/"+f) {
				t.Errorf("the Codex profile links %s, another harness's", dep)
			}
		}
	}
}
