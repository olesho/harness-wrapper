package chatcore

import (
	"os/exec"
	"strings"
	"testing"
)

// A binary that imports only this package and one harness's screen adapter
// links no other harness: not its screen adapter, its transcript reader, nor
// its classifier patterns. This is what lets a runtime's harness list decide
// which harnesses a binary contains. `go list -deps` of the packages is exactly
// what such a binary links (the runtime aside).
func TestLinks_OneHarnessAtATime(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	const mod = "github.com/olesho/harness-wrapper/"
	perHarness := map[string][]string{
		"claude-code": {"harness/claudecode", "transcript/claudecode", "wrapcore/harness/claude"},
		"codex":       {"harness/codex", "transcript/codex", "wrapcore/harness/codex"},
		"pi":          {"transcript/pi", "adapter/pi"},
		"opencode":    {"harness/opencode", "wrapcore/harness/opencode"},
		"cursor":      {"wrapcore/harness/cursor"},
	}
	for _, tc := range []struct {
		name    string
		imports []string
		links   string // the one harness whose packages may appear; "" for none
	}{
		{"core alone", []string{mod + "internal/chatcore"}, ""},
		{"memstore", []string{mod + "pkg/chat/memstore"}, ""},
		{"supervisor core", []string{mod + "internal/wrapcore"}, ""},
		{"core with claude-code", []string{mod + "internal/chatcore", mod + "pkg/turns/harness/claudecode"}, "claude-code"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(goTool, append([]string{"list", "-deps"}, tc.imports...)...).Output()
			if err != nil {
				t.Fatalf("go list -deps %v: %v", tc.imports, err)
			}
			deps := strings.Fields(string(out))
			for harness, suffixes := range perHarness {
				if harness == tc.links {
					continue
				}
				for _, dep := range deps {
					for _, suffix := range suffixes {
						if strings.HasSuffix(dep, "/"+suffix) {
							t.Errorf("links %s, a package of %s", dep, harness)
						}
					}
				}
			}
		})
	}
}
