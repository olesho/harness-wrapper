package env

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
)

// Core-owned composition combinator (design §5).
//
// Compose(inner, layer) returns a Workspace in which EVERY operation is defined
// in terms of inner-workspace operations, per the §5.1 operation-mapping table.
// Written once so a new containment backend cannot get teardown ordering or
// staging cleanup subtly wrong — pairwise implementation testing collapses into
// "test the combinator + test each layer's primitives".

// stagingSeq is a per-process monotonic counter so concurrent uploads never
// collide on a staging path. A counter is deterministic and, unlike
// time/randomness, always available.
var stagingSeq int64

func basename(p string) string {
	i := strings.LastIndexAny(p, `/\`)
	if i >= 0 {
		return p[i+1:]
	}
	return p
}

func stagingPathFor(inner Workspace, guest string) string {
	n := atomic.AddInt64(&stagingSeq, 1)
	return fmt.Sprintf("%s/env-stage-%d-%s", inner.GuestPath(PathTmp), n, basename(guest))
}

func execChecked(ctx context.Context, inner Workspace, argv []string, what string) error {
	r, err := inner.Exec(ctx, argv, nil)
	if err != nil {
		return err
	}
	if r.Code != 0 {
		detail := r.Stderr
		if detail == "" {
			detail = r.Stdout
		}
		return fmt.Errorf("compose: %s failed (exit %d): %s", what, r.Code, detail)
	}
	return nil
}

type composedWorkspace struct {
	inner Workspace
	layer ContainmentLayer
}

// Compose decorates inner with the containment primitives of layer.
func Compose(inner Workspace, layer ContainmentLayer) Workspace {
	return &composedWorkspace{inner: inner, layer: layer}
}

func (c *composedWorkspace) Exec(ctx context.Context, argv []string, opts *ExecOpts) (ExecResult, error) {
	// execWrap prefixes the containment's exec command; the whole thing then
	// runs via the INNER exec (the containment runs where inner runs, §5.1).
	var o ExecOpts
	if opts != nil {
		o = *opts
	}
	wrapped, wrappedOpts := c.layer.ExecWrap(argv, o)
	return c.inner.Exec(ctx, wrapped, &wrappedOpts)
}

func (c *composedWorkspace) Upload(ctx context.Context, hostPath, guestPath string) error {
	stage := stagingPathFor(c.inner, guestPath)
	cross := c.layer.CrossUpload(stage, guestPath)
	if len(cross) == 0 {
		// Identity containment: no policy boundary — upload straight to the final
		// path via the inner workspace.
		return c.inner.Upload(ctx, hostPath, guestPath)
	}
	// Real boundary: land the file on the inner (staging), then cross it in.
	if err := c.inner.Upload(ctx, hostPath, stage); err != nil {
		return err
	}
	return execChecked(ctx, c.inner, cross, "crossUpload")
}

func (c *composedWorkspace) Download(ctx context.Context, guestPath, hostPath string) error {
	stage := stagingPathFor(c.inner, guestPath)
	cross := c.layer.CrossDownload(guestPath, stage)
	if len(cross) == 0 {
		return c.inner.Download(ctx, guestPath, hostPath)
	}
	// Cross the file out to staging on the inner, then pull staging to host.
	if err := execChecked(ctx, c.inner, cross, "crossDownload"); err != nil {
		return err
	}
	return c.inner.Download(ctx, stage, hostPath)
}

func (c *composedWorkspace) GuestPath(kind PathKind) string {
	// Containment paths SHADOW inner paths; "" defers to the inner path.
	if mapped := c.layer.PathMap(kind); mapped != "" {
		return mapped
	}
	return c.inner.GuestPath(kind)
}

func (c *composedWorkspace) HostAlias(hostURL string) string {
	// Fold across BOTH hops: true host → provisioned machine (inner.HostAlias) →
	// contained sandbox (layer.AliasMap, if the layer rewrites URLs).
	viaInner := c.inner.HostAlias(hostURL)
	if am, ok := c.layer.(AliasMapper); ok {
		return am.AliasMap(viaInner)
	}
	return viaInner
}

func (c *composedWorkspace) Destroy(ctx context.Context, outcome Outcome) error {
	// Outer (containment) teardown, THEN inner destroy — per-layer partial
	// failure aggregated, never short-circuited (§4 / §5.1).
	errs := runAll([]func() error{
		func() error {
			t := c.layer.Teardown()
			if len(t) > 0 {
				return execChecked(ctx, c.inner, t, "teardown")
			}
			return nil
		},
		func() error {
			return c.inner.Destroy(ctx, outcome)
		},
	})
	if len(errs) > 0 {
		return &TeardownError{Errors: errs, Context: "compose.destroy"}
	}
	return nil
}
