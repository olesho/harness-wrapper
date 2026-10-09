package claudecodetui

import (
	"os/exec"
	"strings"
	"testing"
)

// A runtime whose harness list names the TUI profile links it, the Claude
// Code profile it shares its record with and the shared adapter: none of hw's
// chat or screen machinery — the profile drives claude's terminal itself — nor
// another harness's code. agentd's architecture test forbids the former.
func TestLinks_ClaudeTUIOnly(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	const mod = "github.com/olesho/harness-wrapper/"
	exact := []string{"pkg/chat", "internal/chatcore", "pkg/wrapper", "internal/wrapcore", "pkg/harness", "pkg/screen", "pkg/turns"}
	suffixes := []string{"harness/codex", "transcript/codex", "transcript/pi", "adapter/pi", "adapter/codex", "harness/opencode", "harness/cursor"}
	out, err := exec.Command(goTool, "list", "-deps", mod+"pkg/adapter/claudecodetui").Output()
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
				t.Errorf("the TUI profile links %s", dep)
			}
		}
		for _, f := range suffixes {
			if strings.HasSuffix(rel, "/"+f) {
				t.Errorf("the TUI profile links %s, another harness's", dep)
			}
		}
	}
}
