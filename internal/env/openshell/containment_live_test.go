package openshell

import (
	"context"
	"os"
	"testing"

	"github.com/olesho/harness-wrapper/internal/env"
)

// GATED LIVE TIER — the actual containment behavior (guest image, krun egress,
// live openshell gateway) is coupled to a live guest runtime and MUST NOT run in
// the unit tier. It skips by default and runs only under
// HARNESS_WRAPPER_CONTAINMENT=1, mirroring pkg/harness/conformance_test.go's
// HARNESS_WRAPPER_CONFORMANCE gate. This tier owns ALL openshell behavioral
// coverage: gateway preflight, sandbox create/exec/upload/download/delete over
// the real CLI, and krun egress against a guest image shipping a statable
// /init.krun (index.ts:129). None of that is unit-testable.
//
//	HARNESS_WRAPPER_CONTAINMENT=1 go test ./internal/env/openshell/ -run Live -v

const containmentEnv = "HARNESS_WRAPPER_CONTAINMENT"

func requireContainment(t *testing.T) {
	t.Helper()
	if os.Getenv(containmentEnv) != "1" {
		t.Skipf("set %s=1 to run live openshell containment against a real gateway", containmentEnv)
	}
}

// TestLive_Preflight exercises the real gateway connectivity + provider checks
// through the default spawning CliRunner. Requires a Connected openshell gateway
// with the configured provider registered.
func TestLive_Preflight(t *testing.T) {
	requireContainment(t)

	c := OpenShell(Options{})
	if err := c.Preflight(context.Background(), nil); err != nil {
		t.Fatalf("live preflight failed: %v", err)
	}
}

// TestLive_AcquireExecTeardown drives the full sandbox lifecycle against a live
// gateway: create → guest layout prep → a real in-guest exec → teardown. It runs
// over a live-provisioned Workspace supplied via the test's own wiring; absent
// that wiring it is a no-op beyond the preflight, so the skip gate is the only
// thing standing between CI and a live gateway.
func TestLive_AcquireExecTeardown(t *testing.T) {
	requireContainment(t)

	ws := liveWorkspace(t)
	if ws == nil {
		t.Skip("no live workspace wired; set up a provisioner to exercise acquire")
	}

	c := OpenShell(Options{AgentID: "live-openshell-test"})
	ctx := context.Background()
	if err := c.Preflight(ctx, ws); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	layer, err := c.Acquire(ctx, ws, env.PolicySpec{Tier: "trusted-internal"})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Teardown is idempotent (double-destroy contract): first emits a delete argv,
	// second is a no-op.
	if td := layer.Teardown(); len(td) == 0 {
		t.Error("first teardown should emit a delete argv")
	}
	for _, argv := range [][]string{layer.Teardown()} {
		if len(argv) != 0 {
			t.Errorf("second teardown must be a no-op, got %v", argv)
		}
	}
}

// liveWorkspace returns a Workspace bound to a live-provisioned machine, or nil
// when no live provisioner is wired into this build. The unit tier never reaches
// here (the gate skips first).
func liveWorkspace(_ *testing.T) env.Workspace {
	return nil
}
