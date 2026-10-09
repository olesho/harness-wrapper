package codex

import (
	"testing"

	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Provision writes the spec's version policy into the open_config.
func TestProvisionVersionPolicy(t *testing.T) {
	for _, p := range []contract.VersionPolicy{"", contract.VersionStrict, contract.VersionFlexible} {
		s := spec()
		s.VersionPolicy = p
		res, err := adapter.New(Profile{}).Provision(contract.ProvisionRequest{Contract: contract.Version, HarnessRoot: "/opt/codex", Layout: layout(), Spec: s})
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		cfg, err := parseOpenConfig(res.OpenConfig)
		if err != nil || cfg.VersionPolicy != p {
			t.Errorf("%q: open_config's policy %q, %v", p, cfg.VersionPolicy, err)
		}
	}
}

// codex's version is the one its initialize answer's user agent carries
// (codex 0.160.0).
func TestVersionOfUserAgent(t *testing.T) {
	for ua, want := range map[string]string{
		"harness-wrapper/0.160.0 (Mac OS 26.5.2; arm64) Apple_Terminal/470.2 (harness-wrapper; harness-wrapper (devel))": "0.160.0",
		"codex_cli_rs/0.161.0 (Ubuntu 24.4.0; x86_64) unknown":                                                           "0.161.0",
		"":                "",
		"no version here": "",
	} {
		if got := versionOfUserAgent(ua); got != want {
			t.Errorf("versionOfUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
}
