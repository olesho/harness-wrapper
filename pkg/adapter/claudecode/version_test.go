package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/sessionid"
	"github.com/olesho/harness-wrapper/pkg/adapter"
	"github.com/olesho/harness-wrapper/pkg/contract"
)

// Provision writes the spec's version policy into the open_config, and
// nothing for none.
func TestProvisionVersionPolicy(t *testing.T) {
	for _, p := range []contract.VersionPolicy{"", contract.VersionStrict, contract.VersionFlexible} {
		req := goldenRequest(t, loadGolden(t))
		req.Spec.VersionPolicy = p
		res, err := Profile{}.Provision(req)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		cfg, err := parseOpenConfig(res.OpenConfig)
		if err != nil || cfg.VersionPolicy != p {
			t.Errorf("%q: open_config's policy %q, %v", p, cfg.VersionPolicy, err)
		}
		if p == "" && strings.Contains(string(res.OpenConfig), "version_policy") {
			t.Errorf("no policy, and the open_config names one: %s", res.OpenConfig)
		}
	}
}

// Under flexible a claude other than the pin opens, reporting its version;
// under strict it is refused with version_unsupported naming both versions.
// A claude that cannot say its version opens under flexible, reporting none.
func TestStartVersionPolicy(t *testing.T) {
	old := versionOf
	t.Cleanup(func() { versionOf = old })
	start := func(policy contract.VersionPolicy) (adapter.Transport, error) {
		dir := t.TempDir()
		bin := filepath.Join(dir, "claude")
		if err := os.WriteFile(bin, []byte(scriptedClaude), 0o755); err != nil { //nolint:gosec // a test's script
			t.Fatal(err)
		}
		id := sessionid.NewUUID()
		oc, _ := json.Marshal(openConfig{Binary: bin, Env: []string{"INIT_ID=" + id}, WorkingDir: dir, Spool: filepath.Join(dir, "scratch"), VersionPolicy: policy})
		return Profile{}.Start(context.Background(), adapter.Start{Mode: contract.OpenFresh, SessionID: id, OpenConfig: oc, Report: func(adapter.Event) {}})
	}
	versionOf = func(context.Context, string, []string) (string, error) { return "9.9.9", nil }
	for _, p := range []contract.VersionPolicy{"", contract.VersionFlexible} {
		tr, err := start(p)
		if err != nil {
			t.Fatalf("claude 9.9.9 under %q: %v", p, err)
		}
		if v := tr.(adapter.Versioned).HarnessVersion(); v != "9.9.9" {
			t.Errorf("claude 9.9.9 under %q reports %q", p, v)
		}
		tr.Stop(context.Background(), 0)
	}
	_, err := start(contract.VersionStrict)
	var e *contract.Error
	if !errors.As(err, &e) || e.Reason != contract.OpenVersionUnsupported || !strings.Contains(e.Message, "9.9.9") || !strings.Contains(e.Message, Pinned()) {
		t.Errorf("claude 9.9.9 under strict: %v, want version_unsupported naming 9.9.9 and %s", err, Pinned())
	}
	versionOf = func(context.Context, string, []string) (string, error) { return Pinned(), nil }
	tr, err := start(contract.VersionStrict)
	if err != nil {
		t.Fatalf("the pin under strict: %v", err)
	}
	tr.Stop(context.Background(), 0)

	versionOf = func(context.Context, string, []string) (string, error) { return "", errors.New("no version") }
	tr, err = start(contract.VersionFlexible)
	if err != nil {
		t.Fatalf("a claude of no version under flexible: %v", err)
	}
	if v := tr.(adapter.Versioned).HarnessVersion(); v != "" {
		t.Errorf("a claude of no version reports %q", v)
	}
	tr.Stop(context.Background(), 0)
	if _, err := start(contract.VersionStrict); !errors.As(err, &e) || e.Reason != contract.OpenVersionUnsupported {
		t.Errorf("a claude of no version under strict: %v, want version_unsupported", err)
	}
}
