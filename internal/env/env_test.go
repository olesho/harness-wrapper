package env

import (
	"context"
	"errors"
	"testing"
)

// scriptedProvisioner returns a preconfigured workspace and records lifecycle
// events into a shared log.
type scriptedProvisioner struct {
	ws           *fakeWorkspace
	log          *[]string
	preflightErr error
	createErr    error
}

func (p *scriptedProvisioner) Name() string { return "scripted" }
func (p *scriptedProvisioner) Preflight(context.Context) error {
	*p.log = append(*p.log, "provision.preflight")
	return p.preflightErr
}
func (p *scriptedProvisioner) Create(context.Context, WorkspaceSpec) (Workspace, error) {
	*p.log = append(*p.log, "provision.create")
	if p.createErr != nil {
		return nil, p.createErr
	}
	return p.ws, nil
}

type scriptedContainment struct {
	log          *[]string
	preflightErr error
}

func (c *scriptedContainment) Name() string { return "scripted" }
func (c *scriptedContainment) Preflight(context.Context, Workspace) error {
	*c.log = append(*c.log, "contain.preflight")
	return c.preflightErr
}
func (c *scriptedContainment) Layer(PolicySpec) ContainmentLayer {
	*c.log = append(*c.log, "contain.layer")
	return fakeLayer{}
}

type scriptedInjector struct {
	name     string
	log      *[]string
	applyErr error
	redacts  []string
}

func (i *scriptedInjector) Requires() []Capability { return nil }
func (i *scriptedInjector) Redactions() []string   { return i.redacts }
func (i *scriptedInjector) Apply(context.Context, Workspace) error {
	*i.log = append(*i.log, "apply:"+i.name)
	return i.applyErr
}
func (i *scriptedInjector) Cleanup(context.Context, Workspace) error {
	*i.log = append(*i.log, "cleanup:"+i.name)
	return nil
}

type recordingRedactor struct{ secrets []string }

func (r *recordingRedactor) Register(s string) { r.secrets = append(r.secrets, s) }

func TestEnvHappyPathOrder(t *testing.T) {
	var log []string
	inner := &fakeWorkspace{}
	red := &recordingRedactor{}
	env, err := Env(context.Background(), EnvConfig{
		Provision: &scriptedProvisioner{ws: inner, log: &log},
		Contain:   &scriptedContainment{log: &log},
		Spec:      WorkspaceSpec{Name: "n"},
		Injectors: []CredentialInjector{
			&scriptedInjector{name: "a", log: &log, redacts: []string{"s1"}},
			&scriptedInjector{name: "b", log: &log, redacts: []string{"s2"}},
		},
		Redactor: red,
	})
	if err != nil {
		t.Fatalf("Env: %v", err)
	}

	wantAcquire := []string{
		"provision.preflight",
		"provision.create",
		"contain.preflight",
		"contain.layer",
		"apply:a",
		"apply:b",
	}
	assertPrefix(t, log, wantAcquire)

	// Redactions registered before any apply.
	if len(red.secrets) != 2 || red.secrets[0] != "s1" || red.secrets[1] != "s2" {
		t.Fatalf("redactions wrong/out of order: %v", red.secrets)
	}

	// Destroy unwinds injectors in REVERSE, then the workspace.
	if err := env.Destroy(context.Background(), OutcomeSuccess); err != nil {
		t.Fatal(err)
	}
	tail := log[len(log)-2:]
	if tail[0] != "cleanup:b" || tail[1] != "cleanup:a" {
		t.Fatalf("cleanup order not reversed: %v", tail)
	}
	if len(inner.destroyOutcomes) != 1 || inner.destroyOutcomes[0] != OutcomeSuccess {
		t.Fatalf("workspace not destroyed with outcome: %v", inner.destroyOutcomes)
	}
}

func TestEnvUnwindsOnContainPreflightFailure(t *testing.T) {
	var log []string
	inner := &fakeWorkspace{}
	_, err := Env(context.Background(), EnvConfig{
		Provision: &scriptedProvisioner{ws: inner, log: &log},
		Contain:   &scriptedContainment{log: &log, preflightErr: errors.New("no runtime")},
		Spec:      WorkspaceSpec{Name: "n"},
	})
	if err == nil {
		t.Fatal("expected setup failure")
	}
	// The created inner workspace must be torn down as a setup-failure.
	if len(inner.destroyOutcomes) != 1 || inner.destroyOutcomes[0] != OutcomeSetupFailure {
		t.Fatalf("inner not torn down with setup-failure: %v", inner.destroyOutcomes)
	}
}

func TestEnvHalfFailedApplyStillCleansUp(t *testing.T) {
	var log []string
	inner := &fakeWorkspace{}
	_, err := Env(context.Background(), EnvConfig{
		Provision: &scriptedProvisioner{ws: inner, log: &log},
		Contain:   &scriptedContainment{log: &log},
		Spec:      WorkspaceSpec{Name: "n"},
		Injectors: []CredentialInjector{
			&scriptedInjector{name: "a", log: &log},
			&scriptedInjector{name: "b", log: &log, applyErr: errors.New("boom")},
		},
	})
	if err == nil {
		t.Fatal("expected apply failure")
	}
	// Both injectors' cleanup ran (b's cleanup was pushed before its failed
	// apply), in reverse order, plus the inner setup-failure destroy.
	joined := log
	if !contains(joined, "cleanup:b") || !contains(joined, "cleanup:a") {
		t.Fatalf("half-failed apply did not clean up both injectors: %v", joined)
	}
	if len(inner.destroyOutcomes) != 1 || inner.destroyOutcomes[0] != OutcomeSetupFailure {
		t.Fatalf("inner not torn down: %v", inner.destroyOutcomes)
	}
}

func TestEnvSurfacesOriginalCauseFirst(t *testing.T) {
	var log []string
	cause := errors.New("preflight-cause")
	inner := &fakeWorkspace{destroyErr: errors.New("teardown-boom")}
	_, err := Env(context.Background(), EnvConfig{
		Provision: &scriptedProvisioner{ws: inner, log: &log},
		Contain:   &scriptedContainment{log: &log, preflightErr: cause},
		Spec:      WorkspaceSpec{Name: "n"},
	})
	var te *TeardownError
	if !errors.As(err, &te) {
		t.Fatalf("expected TeardownError when unwind also fails, got %v", err)
	}
	if !errors.Is(te, cause) {
		t.Fatal("original cause should be surfaced inside the aggregate")
	}
}

// scriptedAcquirer is a containment with an Acquire hook.
type scriptedAcquirer struct {
	scriptedContainment
	acquireErr error
}

func (a *scriptedAcquirer) Acquire(context.Context, Workspace, PolicySpec) (ContainmentLayer, error) {
	*a.log = append(*a.log, "contain.acquire")
	if a.acquireErr != nil {
		return nil, a.acquireErr
	}
	return fakeLayer{}, nil
}

func TestEnvPrefersAcquireOverLayer(t *testing.T) {
	var log []string
	inner := &fakeWorkspace{}
	_, err := Env(context.Background(), EnvConfig{
		Provision: &scriptedProvisioner{ws: inner, log: &log},
		Contain:   &scriptedAcquirer{scriptedContainment: scriptedContainment{log: &log}},
		Spec:      WorkspaceSpec{Name: "n"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if contains(log, "contain.layer") {
		t.Fatal("Acquire path must not call Layer")
	}
	if !contains(log, "contain.acquire") {
		t.Fatal("Acquire hook was not used")
	}
}

func assertPrefix(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) < len(want) {
		t.Fatalf("log too short: got %v, want prefix %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("log[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
