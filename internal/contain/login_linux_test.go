//go:build linux

package contain

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/containment"
)

// loginHarness registers an inactive test profile with a login flow and a
// credential variable, for the test binary.
func loginHarness(t *testing.T) (self string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	self, _ = filepath.EvalSymlinks(self)
	t.Cleanup(RegisterTestProfile(TestProfile{
		Harness:  "hwlogin",
		ExecDirs: []string{filepath.Dir(self)},
		AuthEnv:  []string{"HW_TEST_TOKEN"},
		Inactive: true,
		Login:    &TestLogin{Args: []string{"login"}, Status: []string{"status"}, URL: `https://\S+`, LoggedIn: `yes`, TCPBind: "a callback"},
	}))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	return self
}

// TestLoginRefusals: a login runs only its profile's two commands, only into
// a caller StateDir, and the exception covers nothing but a login.
func TestLoginRefusals(t *testing.T) {
	self := loginHarness(t)
	stateDir := t.TempDir()
	cases := []struct {
		name  string
		in    Input
		stage string
	}{
		{"no StateDir", Input{Args: []string{"login"}, Login: true}, StageState},
		{"managed state", Input{Args: []string{"login"}, Login: true, State: &State{}}, StageState},
		{"another command", Input{Args: []string{"login", "--then", "rm"}, Login: true}, StageRequest},
		{"no arguments", Input{Login: true}, StageRequest},
		{"a session of an inactive profile", Input{Args: []string{"login"}}, StageProfile},
		{"a listening login under restricted TCP", Input{Args: []string{"login"}, Login: true}, StageProfile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &containment.Request{Kind: containment.KindLandlock}
			if tc.name != "no StateDir" {
				req.StateDir = stateDir
			}
			if strings.Contains(tc.name, "restricted TCP") {
				req.RestrictTCP, req.ConnectTCP = true, []uint16{443}
			}
			in := tc.in
			in.Request, in.Harness, in.BinaryPath, in.WorkingDir = req, "hwlogin", self, stateDir
			l, err := Prepare(in)
			if err == nil {
				l.Release()
				t.Fatal("Prepare accepted a login it must refuse")
			}
			if !isRefusal(err, tc.stage) {
				t.Fatalf("got %v, want a refusal at stage %q", err, tc.stage)
			}
		})
	}
}

// TestLoginDropsCredentialVariables: a login's environment carries none of
// the profile's credential variables, even named in PassEnv, while a session
// of the same profile inherits them.
func TestLoginDropsCredentialVariables(t *testing.T) {
	if _, err := landlockProbe(); err != nil {
		t.Skipf("Landlock ABI 9 unavailable: %v", err)
	}
	self := loginHarness(t)
	stateDir := t.TempDir()
	input := func(login bool, args ...string) Input {
		return Input{
			Request:    &containment.Request{Kind: containment.KindLandlock, StateDir: stateDir, PassEnv: []string{"HW_TEST_TOKEN"}},
			Harness:    "hwlogin",
			BinaryPath: self,
			Args:       args,
			WorkingDir: stateDir,
			Env:        append(os.Environ(), "HW_TEST_TOKEN=secret"),
			Login:      login,
		}
	}
	has := func(l *Launch) bool {
		return slices.Contains(l.applied.Env, "HW_TEST_TOKEN") ||
			slices.ContainsFunc(l.env, func(kv string) bool { return strings.HasPrefix(kv, "HW_TEST_TOKEN=") })
	}
	for _, args := range [][]string{{"login"}, {"status"}} {
		l, err := Prepare(input(true, args...))
		if err != nil {
			t.Fatalf("login %q: %v", args, err)
		}
		if has(l) {
			t.Errorf("login %q passes a credential variable: %v", args, l.applied.Env)
		}
		if l.applied.State.Mode != "caller" || l.applied.State.StateDir == "" {
			t.Errorf("login %q state: %+v", args, l.applied.State)
		}
		l.Release()
	}
	t.Cleanup(ActivateProfilesForTest())
	l, err := Prepare(input(false, "run"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if !has(l) {
		t.Error("a session does not inherit the credential variable, so the login check above proves nothing")
	}
}
