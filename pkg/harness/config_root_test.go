package harness

import "testing"

// TestHookConfigRootFromInheritedEnv: with Wrapper.Env == nil the harness child
// INHERITS this process's environment, including a per-agent config root, so the
// hook subprocess it fires must be handed the same root. Before the fix the
// inherited root was dropped: harnessConfigDir read the nil slice literally and
// HW_HARNESS_CONFIG_DIR stayed unset, while the child itself ran on the root.
func TestHookConfigRootFromInheritedEnv(t *testing.T) {
	t.Setenv("FAKE_CONFIG_DIR", "/review/agent")
	// Match Run's composition for Wrapper.Env == nil, where the child
	// inherits the environment and the hook must resolve the same root.
	root := harnessConfigDir(configDirProfile{}, nil)
	env := hookEnv(nil, "/spool", "/wt", nil, "", root)
	if got := EnvLookup(env, EnvConfigDir); got != "/review/agent" {
		t.Fatalf("child inherits FAKE_CONFIG_DIR=%q but hook config root = %q", EnvLookup(env, "FAKE_CONFIG_DIR"), got)
	}
}
