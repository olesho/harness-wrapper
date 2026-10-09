package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/contract"
	"github.com/olesho/harness-wrapper/pkg/contract/conformance"
)

// fakeFixture is the kit's fixture for the shared adapter over the fake
// profile.
func fakeFixture(t *testing.T) conformance.Fixture {
	root := t.TempDir()
	bin := fakeBinary(root)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return conformance.Fixture{
		Adapter:     New(newFakeProfile()),
		HarnessRoot: root,
		Spec: contract.AgentSpec{
			Instructions:      contract.Instructions{Persona: "Be brief."},
			PermissionPosture: contract.PostureBypass,
		},
		Kill: func(_ conformance.T, s contract.Session) { s.(*session).t.(*fakeTransport).kill() },
		Heard: func(_ conformance.T, s contract.Session) string {
			t := s.(*session).t.(*fakeTransport)
			t.mu.Lock()
			defer t.mu.Unlock()
			return strings.Join(t.heard, "\n")
		},
		Quiet: 300 * time.Millisecond,
		HideBinary: func(t conformance.T) func() {
			if err := os.Rename(bin, bin+".hidden"); err != nil {
				t.Errorf("hiding the binary: %v", err)
			}
			return func() { _ = os.Rename(bin+".hidden", bin) }
		},
		Timeout: 10 * time.Second,
		NonPin: func(t conformance.T) (string, func()) {
			if err := os.WriteFile(bin+".version", []byte("1.1.0\n"), 0o644); err != nil {
				t.Errorf("making the binary 1.1.0: %v", err)
			}
			return "1.1.0", func() { _ = os.Remove(bin + ".version") }
		},
	}
}

// Only strict refuses, a version other than the pin or none, naming both;
// and a request of a minor before 1.7 carries no policy.
func TestVersionPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy  contract.VersionPolicy
		running string
		refused bool
	}{
		{"", "2.0.0", false},
		{contract.VersionFlexible, "", false},
		{contract.VersionStrict, "1.0.0", false},
		{contract.VersionStrict, "2.0.0", true},
		{contract.VersionStrict, "", true},
	} {
		err := CheckHarnessVersion(tc.policy, "h", "1.0.0", tc.running)
		var e *contract.Error
		switch {
		case !tc.refused && err != nil:
			t.Errorf("%q, %q: %v", tc.policy, tc.running, err)
		case tc.refused && (!errors.As(err, &e) || e.Code != contract.CodeOpenFailed || e.Reason != contract.OpenVersionUnsupported || !strings.Contains(e.Message, "1.0.0")):
			t.Errorf("%q, %q: %v, want open_failed version_unsupported naming the pin", tc.policy, tc.running, err)
		case tc.refused && tc.running != "" && !strings.Contains(e.Message, tc.running):
			t.Errorf("%q, %q: %q names not the version running", tc.policy, tc.running, e.Message)
		}
	}

	f := fakeFixture(t)
	base := t.TempDir()
	l := contract.Layout{
		Home: filepath.Join(base, "home"), Config: filepath.Join(base, "config"), Workspace: filepath.Join(base, "ws"),
		Secrets: filepath.Join(base, "secrets"), Scratch: filepath.Join(base, "scratch"),
	}
	req := contract.ProvisionRequest{Contract: "harness-adapter/1.6", HarnessRoot: f.HarnessRoot, Layout: l, Spec: f.Spec}
	req.Spec.VersionPolicy = contract.VersionStrict
	var e *contract.Error
	if _, err := f.Adapter.Provision(req); !errors.As(err, &e) || e.Code != contract.CodeProtocol || e.Field != "spec.version_policy" {
		t.Errorf("a 1.6 request with a policy: %v, want protocol on spec.version_policy", err)
	}
	req.Contract = contract.Version
	if _, err := f.Adapter.Provision(req); err != nil {
		t.Errorf("a %s request with a policy: %v", contract.Version, err)
	}
}

// The shared adapter, over a profile that keeps the rules, passes the kit.
func TestFakeProfileConforms(t *testing.T) {
	conformance.Run(conformance.Testing(t), fakeFixture(t))
}
