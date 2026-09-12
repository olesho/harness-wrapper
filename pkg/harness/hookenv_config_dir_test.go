package harness

import (
	"strings"
	"testing"
)

// envHas reports whether env contains the exact "K=V" assignment.
func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

// envHasKey reports whether env assigns key at all.
func envHasKey(env []string, key string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			return true
		}
	}
	return false
}

// TestHookEnv_WritesConfigDir is the hook half of PUPPET-494: HW_HARNESS_CONFIG_DIR
// was READ by the hook subprocess (hookrun.go) and never WRITTEN by anything, so
// a fired hook naming a profiled agent's transcript was rejected as "not under
// transcript root".
func TestHookEnv_WritesConfigDir(t *testing.T) {
	base := []string{"PATH=/usr/bin"}
	env := hookEnv(base, "/spool", "/wt", nil, "", "/agents/w3/claude")

	if !envHas(env, EnvConfigDir+"=/agents/w3/claude") {
		t.Fatalf("env missing %s=/agents/w3/claude: %v", EnvConfigDir, env)
	}
	// The pre-existing variables must still be there, and the inherited env
	// must be appended to, never replaced.
	for _, want := range []string{"PATH=/usr/bin", EnvSpool + "=/spool", EnvHookCwd + "=/wt"} {
		if !envHas(env, want) {
			t.Fatalf("env missing %q: %v", want, env)
		}
	}
}

// TestHookEnv_OmitsConfigDirWhenEmpty pins the unprofiled case: no config dir
// resolved ⇒ the variable is ABSENT, so the hook subprocess keeps its
// <HW_HOME>/.claude fallback and behaviour is unchanged.
func TestHookEnv_OmitsConfigDirWhenEmpty(t *testing.T) {
	env := hookEnv([]string{"PATH=/usr/bin"}, "/spool", "/wt", nil, "", "")
	if envHasKey(env, EnvConfigDir) {
		t.Fatalf("env unexpectedly assigns %s: %v", EnvConfigDir, env)
	}
}

// configDirProfile is a Profile that resolves a config root from the launch env.
type configDirProfile struct{ Profile }

func (configDirProfile) Name() string { return "cfgdir" }

func (configDirProfile) Resolve(_ ResolveContext) ResolvedProfile { return ResolvedProfile{} }

func (configDirProfile) HarnessConfigDir(env []string) string {
	return EnvLookup(env, "FAKE_CONFIG_DIR")
}

// plainProfile implements no optional capability.
type plainProfile struct{ Profile }

func (plainProfile) Name() string { return "plain" }

func (plainProfile) Resolve(_ ResolveContext) ResolvedProfile { return ResolvedProfile{} }

func TestHarnessConfigDir(t *testing.T) {
	env := []string{"FAKE_CONFIG_DIR=/root/one", "FAKE_CONFIG_DIR=/root/two"}

	// Last occurrence wins, matching exec semantics.
	if got := harnessConfigDir(configDirProfile{}, env); got != "/root/two" {
		t.Fatalf("harnessConfigDir = %q, want %q", got, "/root/two")
	}
	// A profile without the capability resolves to "" rather than panicking.
	if got := harnessConfigDir(plainProfile{}, env); got != "" {
		t.Fatalf("harnessConfigDir(plain) = %q, want empty", got)
	}
}
