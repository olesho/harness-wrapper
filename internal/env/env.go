package env

import "context"

// Core lifecycle engine (design §4).
//
// Env owns the canonical acquisition order and the reverse-order failure
// unwind. Implementations (provisioners, containments, injectors) never sequence
// this themselves — that is what makes the ordering a contract rather than an
// implementation accident.
//
//  1. provisioner.Preflight            host-side, zero resources
//  2. provisioner.Create      → inner Workspace          ── resources exist here
//  3. containment.Preflight(inner)     runtime capability checks, via inner exec
//  4. Compose(inner, layer)   → composed Workspace
//  5. Redactions registered, THEN injector.Apply(composedWs)
//  6. (turns run — out of this module's scope)
//  7. destroy: injector.Cleanup → containment teardown → inner destroy

// noopRedactor drops every secret — the default when a caller wires no log sink.
// Redaction is still SEQUENCED correctly (before apply); it just goes nowhere.
type noopRedactor struct{}

func (noopRedactor) Register(string) {
	// No-op: the default redactor drops every secret (see type doc).
}

// Env acquires an Environment per the canonical lifecycle. Any failure in steps
// 1–5 unwinds all acquired layers in reverse order, best-effort, errors
// aggregated, never short-circuited (§4).
func Env(ctx context.Context, cfg EnvConfig) (*Environment, error) {
	provision := cfg.Provision
	contain := cfg.Contain
	spec := cfg.Spec
	injectors := cfg.Injectors
	redactor := cfg.Redactor
	if redactor == nil {
		redactor = noopRedactor{}
	}
	policy := cfg.Policy

	// Cleanup thunks pushed in ACQUISITION order; unwound in reverse. Each
	// injector cleanup is pushed BEFORE its Apply, so a half-failed apply still
	// cleans up (design §4 step 5). Setup-failure unwind ALWAYS destroys.
	var unwind []func() error
	// teardownWs starts as the inner workspace and is upgraded to the composed
	// workspace once Compose runs, so the single workspace-destroy thunk always
	// tears down the deepest layer acquired.
	var teardownWs Workspace

	fail := func(setupErr error) (*Environment, error) {
		teardownErrs := runAll(reverseThunks(unwind))
		if len(teardownErrs) > 0 {
			// Surface the ORIGINAL cause first, with teardown failures attached —
			// the setup error is why we are unwinding at all.
			all := append([]error{setupErr}, teardownErrs...)
			return nil, &TeardownError{Errors: all, Context: "env: setup failed and unwind hit errors"}
		}
		return nil, setupErr
	}

	// 1. host-side preflight — zero resources.
	if err := provision.Preflight(ctx); err != nil {
		return fail(err)
	}

	// 2. create — resources exist from here; register its teardown immediately.
	inner, err := provision.Create(ctx, spec)
	if err != nil {
		return fail(err)
	}
	teardownWs = inner
	unwind = append(unwind, func() error { return teardownWs.Destroy(ctx, OutcomeSetupFailure) })

	// 3. containment runtime capability checks, via inner exec.
	if err := contain.Preflight(ctx, inner); err != nil {
		return fail(err)
	}

	// 4. compose — the workspace-destroy thunk now tears down containment +
	//    inner. A containment with an Acquire hook creates its resources HERE
	//    (never in preflight — capability checks only) and hands back a layer
	//    closed over them; acquire failure unwinds the inner via the thunk above.
	var layer ContainmentLayer
	if a, ok := contain.(Acquirer); ok {
		layer, err = a.Acquire(ctx, inner, policy)
		if err != nil {
			return fail(err)
		}
	} else {
		layer = contain.Layer(policy)
	}
	composed := Compose(inner, layer)
	teardownWs = composed

	// 5. redactions registered BEFORE any apply (§4): a half-completed Apply can
	//    never emit an unredacted secret. Apply against the COMPOSED workspace so
	//    a file-injected token lands INSIDE the containment boundary.
	for _, inj := range injectors {
		registerRedactions(redactor, inj)
		injCopy := inj
		// Push cleanup before apply — it must run even if this apply half-fails.
		unwind = append(unwind, func() error { return injCopy.Cleanup(ctx, composed) })
		if err := inj.Apply(ctx, composed); err != nil {
			return fail(err)
		}
	}

	// 6/7. Hand back the composed workspace + a retention-honoring destroy that
	//      unwinds in reverse (injector cleanup → containment teardown → inner).
	return &Environment{
		Workspace: composed,
		Destroy:   composedDestroy(injectors, composed),
	}, nil
}

// reverseThunks returns a new slice with unwind's thunks in reverse order, so
// setup-failure teardown runs last-acquired first.
func reverseThunks(unwind []func() error) []func() error {
	reversed := make([]func() error, len(unwind))
	for i, f := range unwind {
		reversed[len(unwind)-1-i] = f
	}
	return reversed
}

// registerRedactions registers every one of an injector's secrets with the
// redactor before any Apply (§4).
func registerRedactions(redactor Redactor, inj CredentialInjector) {
	for _, secret := range inj.Redactions() {
		redactor.Register(secret)
	}
}

// composedDestroy builds the retention-honoring destroy for the composed
// environment: injector cleanups in reverse acquisition order, then the
// composed workspace teardown, with all errors aggregated.
func composedDestroy(injectors []CredentialInjector, composed Workspace) func(context.Context, Outcome) error {
	return func(dctx context.Context, outcome Outcome) error {
		thunks := make([]func() error, 0, len(injectors)+1)
		// Reverse acquisition: last-applied injector cleans up first.
		for i := len(injectors) - 1; i >= 0; i-- {
			inj := injectors[i]
			thunks = append(thunks, func() error { return inj.Cleanup(dctx, composed) })
		}
		thunks = append(thunks, func() error { return composed.Destroy(dctx, outcome) })
		errs := runAll(thunks)
		if len(errs) > 0 {
			return &TeardownError{Errors: errs, Context: "env.destroy"}
		}
		return nil
	}
}
