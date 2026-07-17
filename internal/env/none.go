package env

import "context"

// The None containment (design §3 shipped implementations): identity.
//
// It advertises no boundary — its layer's primitives are all degenerate, so
// Compose(inner, None().Layer(...)) is functionally equal to inner:
//   - ExecWrap      → argv/opts unchanged
//   - CrossUpload   → nil (no crossing; Compose uploads straight to the inner path)
//   - CrossDownload → nil
//   - PathMap       → "" (defer to the inner path — shadow nothing)
//   - Teardown      → nil (nothing to tear down)
//
// This makes None the correct baseline for the conformance suite.

type identityLayer struct{}

func (identityLayer) ExecWrap(argv []string, opts ExecOpts) ([]string, ExecOpts) {
	return argv, opts
}

func (identityLayer) CrossUpload(_, _ string) []string { return nil }

func (identityLayer) CrossDownload(_, _ string) []string { return nil }

func (identityLayer) PathMap(_ PathKind) string { return "" }

func (identityLayer) Teardown() []string { return nil }

// No AliasMap: host-URL rewriting defers entirely to the inner workspace.

type noneContainment struct{}

func (noneContainment) Name() string { return "none" }

func (noneContainment) Preflight(_ context.Context, _ Workspace) error {
	// No containment runtime — nothing to check.
	return nil
}

func (noneContainment) Layer(_ PolicySpec) ContainmentLayer { return identityLayer{} }

// None constructs the identity containment.
func None() Containment { return noneContainment{} }
