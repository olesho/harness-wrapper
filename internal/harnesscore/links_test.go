package harnesscore

import (
	"os/exec"
	"strings"
	"testing"
)

// A binary that imports this package, and a harness's hook profile, links no
// chat and no other harness: this is what lets a Harness Adapter's hook helper
// and record reader carry one harness only. `go list -deps` of the packages is
// exactly what such a binary links (the runtime aside).
func TestLinks_NoChatOneHarness(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH")
	}
	const mod = "github.com/olesho/harness-wrapper/"
	perHarness := map[string][]string{
		"claude-code": {"harness/claude", "harness/claudecode", "transcript/claudecode", "wrapcore/harness/claude"},
		"codex":       {"harness/codex", "transcript/codex", "wrapcore/harness/codex"},
		"pi":          {"transcript/pi", "adapter/pi"},
		"opencode":    {"harness/opencode", "wrapcore/harness/opencode"},
		"cursor":      {"wrapcore/harness/cursor"},
	}
	chat := []string{mod + "pkg/chat", mod + "internal/chatcore", mod + "pkg/wrapper", mod + "internal/wrapcore", mod + "pkg/harness"}
	for _, tc := range []struct {
		name    string
		imports []string
		links   string // the one harness whose packages may appear; "" for none
	}{
		{"core alone", []string{mod + "internal/harnesscore"}, ""},
		{"core with claude's hooks", []string{mod + "internal/harnesscore", mod + "pkg/harness/claude"}, "claude-code"},
		{"core with codex's hooks", []string{mod + "internal/harnesscore", mod + "pkg/harness/codex"}, "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(goTool, append([]string{"list", "-deps"}, tc.imports...)...).Output()
			if err != nil {
				t.Fatalf("go list -deps %v: %v", tc.imports, err)
			}
			deps := strings.Fields(string(out))
			for _, dep := range deps {
				for _, c := range chat {
					if dep == c {
						t.Errorf("links %s", dep)
					}
				}
			}
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
