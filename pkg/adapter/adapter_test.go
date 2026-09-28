package adapter

import (
	"os"
	"path/filepath"
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
		HideBinary: func(t conformance.T) func() {
			if err := os.Rename(bin, bin+".hidden"); err != nil {
				t.Errorf("hiding the binary: %v", err)
			}
			return func() { _ = os.Rename(bin+".hidden", bin) }
		},
		Timeout: 10 * time.Second,
	}
}

// The shared adapter, over a profile that keeps the rules, passes the kit.
func TestFakeProfileConforms(t *testing.T) {
	conformance.Run(conformance.Testing(t), fakeFixture(t))
}
