package daytona

import (
	"context"
	"os"
	"testing"

	"github.com/olesho/harness-wrapper/internal/env"
)

// The live tier owns all live-SDK Daytona behavioral coverage. It is gated
// behind HARNESS_WRAPPER_CONTAINMENT=1 (mirroring pkg/harness/conformance_test.go's
// HARNESS_WRAPPER_CONFORMANCE gate) so it never runs in the unit tier, which
// must build and pass without a live Daytona SDK or network.
//
//	HARNESS_WRAPPER_CONTAINMENT=1 go test ./internal/env/daytona/ -run Live -v
//
// Wiring the real SDK is out of scope for this package (no Go @daytonaio/sdk
// dependency): a live runner injects DaytonaConfig.ClientCtor with an adapter
// over the real SDK before invoking these paths.

const containmentEnv = "HARNESS_WRAPPER_CONTAINMENT"

func requireContainment(t *testing.T) {
	t.Helper()
	if os.Getenv(containmentEnv) != "1" {
		t.Skipf("set %s=1 to run live Daytona SDK behavioral coverage", containmentEnv)
	}
}

// liveConfig builds the DaytonaConfig for the live tier. A live runner supplies
// the real SDK adapter here; the default (no ClientCtor) is deliberately unusable
// so an accidentally-ungated invocation fails loudly rather than hitting network.
func liveConfig(t *testing.T) DaytonaConfig {
	t.Helper()
	return DaytonaConfig{
		APIKey:     os.Getenv("DAYTONA_API_KEY"),
		APIURL:     os.Getenv("DAYTONA_API_URL"),
		ClientCtor: nil, // live runner injects the real SDK adapter
	}
}

// TestLive_ProvisionExecDestroy exercises the full create -> exec -> destroy
// lifecycle against a real Daytona sandbox. Skipped unless the gate is set.
func TestLive_ProvisionExecDestroy(t *testing.T) {
	requireContainment(t)

	config := liveConfig(t)
	if config.ClientCtor == nil {
		t.Skip("live tier gated on but no real SDK adapter wired; wire DaytonaConfig.ClientCtor")
	}

	ctx := context.Background()
	p := Daytona(config)
	if err := p.Preflight(ctx); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	ws, err := p.Create(ctx, env.WorkspaceSpec{
		Image:  os.Getenv("DAYTONA_IMAGE"),
		Labels: map[string]string{"harness-wrapper": "live-test"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() {
		if err := ws.Destroy(ctx, env.OutcomeSuccess); err != nil {
			t.Errorf("destroy: %v", err)
		}
	}()

	res, err := ws.Exec(ctx, []string{"echo", "hello"}, nil)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Code != 0 {
		t.Errorf("exec code = %d, want 0 (stderr=%q)", res.Code, res.Stderr)
	}
}

// TestLive_Sweep exercises the reaper against real orphaned sandboxes.
func TestLive_Sweep(t *testing.T) {
	requireContainment(t)
	config := liveConfig(t)
	if config.ClientCtor == nil {
		t.Skip("live tier gated on but no real SDK adapter wired; wire DaytonaConfig.ClientCtor")
	}
	res, err := Sweep(context.Background(), config, SweepOpts{
		Labels: map[string]string{"harness-wrapper": "live-test"},
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	t.Logf("live sweep dry-run matched %d sandboxes", len(res.Kept))
}
