package claudecodetui

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter/claudecode"
	"github.com/olesho/harness-wrapper/pkg/adapter/claudecodetui/live"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// At the gated posture claude runs in its default mode with the
// PermissionRequest hook, whose timeout outlasts any caller; at bypass there
// is no such hook, and claude asks nothing.
func TestProvisionGated(t *testing.T) {
	l := contract.Layout{Home: "/a/home", Config: "/a/config", Workspace: "/a/workspace", Secrets: "/a/secrets", Scratch: "/a/scratch"}
	render := func(posture string) (settings string, args []string) {
		t.Helper()
		res, err := Profile{}.Provision(contract.ProvisionRequest{
			Contract: contract.Version, HarnessRoot: "/opt/claude-code", Layout: l, Spec: contract.AgentSpec{PermissionPosture: posture},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range res.Files {
			if f.Root == contract.RootConfig && f.Path == "settings.json" {
				settings = string(f.Content())
			}
		}
		cfg, err := claudecode.ParseOpenConfig(res.OpenConfig)
		if err != nil {
			t.Fatal(err)
		}
		return settings, cfg.Args
	}
	bypassSettings, bypassArgs := render(contract.PostureBypass)
	if strings.Contains(bypassSettings, live.HookPermission) || !strings.Contains(strings.Join(bypassArgs, " "), "--permission-mode bypassPermissions") {
		t.Errorf("bypass: %v\n%s", bypassArgs, bypassSettings)
	}
	gatedSettings, gatedArgs := render(contract.PostureGated)
	if a := strings.Join(gatedArgs, " "); !strings.Contains(a, "--permission-mode default") || strings.Contains(a, "--permission-prompt-tool") {
		t.Errorf("gated args: %s", a)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(gatedSettings), &s); err != nil {
		t.Fatal(err)
	}
	groups := s.Hooks[live.HookPermission]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 || groups[0].Hooks[0].Timeout != live.PermissionTimeout ||
		!strings.Contains(groups[0].Hooks[0].Command, live.PermissionArg) {
		t.Errorf("gated PermissionRequest hook: %+v", groups)
	}
	if live.PermissionTimeout*1000 > 1<<31-1 {
		t.Error("the hook's timeout overflows claude's timer")
	}
	for name, g := range s.Hooks {
		for _, m := range g {
			for _, h := range m.Hooks {
				if name != live.HookPermission && h.Timeout != 0 {
					t.Errorf("%s has a timeout", name)
				}
			}
		}
	}
}
