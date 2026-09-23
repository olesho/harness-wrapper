package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harnessenv"
)

// The exhaustive predicate/filter table now lives in pkg/harnessenv, which owns
// the policy (PUPPET-671). What stays here is what is specific to cmd/: that
// the two names the rest of this package calls really are the promoted policy,
// and the PUPPET-317 regression guard — a cleanedEnv that drops
// CLAUDE_CODE_OAUTH_TOKEN starts the spawned harness unauthenticated and the
// run dies at the deadline. That must keep failing loudly HERE, at the call
// site that feeds TurnConfig.Env, and not only in the library.

// TestCleanedEnvDelegatesToHarnessenv pins the delegation itself: a rewrite
// that reintroduced a local copy of the policy would drift from the package
// that documents it.
func TestCleanedEnvDelegatesToHarnessenv(t *testing.T) {
	t.Parallel()

	in := []string{
		"PATH=/usr/bin:/bin",
		"CLAUDECODE=1",
		"CLAUDE_CODE_CHILD_SESSION=1",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test",
	}
	if got, want := filterNestingEnv(in), harnessenv.Filter(in); !reflect.DeepEqual(got, want) {
		t.Errorf("filterNestingEnv() = %q, want harnessenv.Filter() = %q", got, want)
	}
}

// TestCleanedEnv is the PUPPET-317 guard at the cmd/ call site: the credential
// survives and no nesting marker does.
func TestCleanedEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-test")

	got := cleanedEnv()

	if !slices.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-test") {
		t.Error("cleanedEnv() dropped CLAUDE_CODE_OAUTH_TOKEN; the spawned harness would start unauthenticated")
	}
	for _, unwanted := range []string{"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_CHILD_SESSION=1"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("cleanedEnv() kept nesting marker %q", unwanted)
		}
	}
}
