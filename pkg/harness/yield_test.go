package harness_test

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/harness"
	_ "github.com/olesho/harness-wrapper/pkg/harness/all" // register claude (has a yield hook)
)

func TestYieldControlRequestClear(t *testing.T) {
	y, err := harness.NewYieldControl()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = y.Close() }()

	// Before a request, the guard does not block.
	env := []string{harness.EnvSpool + "=/tmp/spool", harness.EnvYieldFile + "=" + y.FilePath()}
	out, err := harness.HandleHookEvent("claude", "yield-guard", env, nil)
	if err != nil {
		t.Fatalf("yield-guard: %v", err)
	}
	if out.Block {
		t.Fatal("yield-guard blocked before any Request")
	}

	// After Request, the guard blocks with the reason surfaced.
	if err := y.Request("deploy incoming"); err != nil {
		t.Fatal(err)
	}
	out, err = harness.HandleHookEvent("claude", "yield-guard", env, nil)
	if err != nil {
		t.Fatalf("yield-guard: %v", err)
	}
	if !out.Block {
		t.Fatal("yield-guard did not block after Request")
	}
	if !strings.Contains(out.BlockOutput, `"decision":"block"`) {
		t.Errorf("block output missing decision:block: %s", out.BlockOutput)
	}
	if !strings.Contains(out.BlockOutput, "deploy incoming") {
		t.Errorf("block output missing the reason: %s", out.BlockOutput)
	}

	// Clear cancels the yield.
	if err := y.Clear(); err != nil {
		t.Fatal(err)
	}
	out, _ = harness.HandleHookEvent("claude", "yield-guard", env, nil)
	if out.Block {
		t.Fatal("yield-guard still blocking after Clear")
	}
}

func TestYieldGuardInertWithoutSpool(t *testing.T) {
	// Outside a wrapper run (no HW_EVENT_SPOOL), the yield-guard is inert even if
	// a yield file is set — leftover hook entries must never block a non-loom run.
	y, err := harness.NewYieldControl()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = y.Close() }()
	if err := y.Request("x"); err != nil {
		t.Fatal(err)
	}
	out, err := harness.HandleHookEvent("claude", "yield-guard", []string{harness.EnvYieldFile + "=" + y.FilePath()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Block {
		t.Fatal("yield-guard blocked without HW_EVENT_SPOOL (should be inert)")
	}
}

func TestYieldControlCloseIdempotent(t *testing.T) {
	y, err := harness.NewYieldControl()
	if err != nil {
		t.Fatal(err)
	}
	if err := y.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := y.Close(); err != nil {
		t.Fatalf("second Close should be a no-op, got %v", err)
	}
}
