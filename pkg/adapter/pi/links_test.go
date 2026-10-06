package pi

import (
	"os/exec"
	"strings"
	"testing"
)

// A runtime whose harness list names Pi links the shared adapter and this
// profile: neither links hw's chat or screen machinery, or another harness's
// code.
func TestLinks_PiOnly(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	const mod = "github.com/olesho/harness-wrapper/"
	// Packages linked by exact path, and each other harness's by suffix.
	exact := []string{"pkg/chat", "internal/chatcore", "pkg/wrapper", "internal/wrapcore", "pkg/harness", "pkg/screen", "pkg/turns"}
	suffixes := []string{
		"adapter/claudecode", "harness/claude", "transcript/claudecode",
		"adapter/codex", "harness/codex", "transcript/codex", "harness/opencode", "harness/cursor",
	}
	out, err := exec.Command(goTool, "list", "-deps", mod+"pkg/adapter/pi").Output()
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
				t.Errorf("the Pi profile links %s", dep)
			}
		}
		for _, f := range suffixes {
			if strings.HasSuffix(rel, "/"+f) {
				t.Errorf("the Pi profile links %s, another harness's", dep)
			}
		}
	}
}
