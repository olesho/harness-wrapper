package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Provision writes the spec's version policy into the open_config.
func TestProvisionVersionPolicy(t *testing.T) {
	for _, p := range []contract.VersionPolicy{"", contract.VersionStrict, contract.VersionFlexible} {
		s := spec()
		s.VersionPolicy = p
		_, cfg, _ := provision(t, s)
		if cfg.VersionPolicy != p {
			t.Errorf("%q: open_config's policy %q", p, cfg.VersionPolicy)
		}
	}
}

// pi's version is its release's package.json's; under strict, Start refuses a
// pi that is not the pin before it launches anything.
func TestStartVersionPolicy(t *testing.T) {
	root := t.TempDir()
	bin := BinaryPath(root)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil { //nolint:gosec // a test's script
		t.Fatal(err)
	}
	if err := os.WriteFile(ExtensionPath(root), Extension, 0o644); err != nil {
		t.Fatal(err)
	}
	if v := packageVersion(bin); v != "" {
		t.Errorf("no package.json: version %q", v)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(bin), "package.json"), []byte(`{"name":"pi","version":"9.9.9"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if v := packageVersion(bin); v != "9.9.9" {
		t.Errorf("version %q, want 9.9.9", v)
	}
	s := spec()
	s.VersionPolicy = contract.VersionStrict
	res, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: root, Layout: layout(), Spec: s})
	if err != nil {
		t.Fatal(err)
	}
	l := layout()
	_, err = Profile{}.Start(context.Background(), adapter.Start{Mode: contract.OpenFresh, OpenConfig: res.OpenConfig, Layout: l, Report: func(adapter.Event) {}})
	var e *contract.Error
	if !errors.As(err, &e) || e.Reason != contract.OpenVersionUnsupported {
		t.Errorf("pi 9.9.9 under strict: %v, want version_unsupported", err)
	}
}
