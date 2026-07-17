// Package env is a clean-room Go re-implementation of the meta-harness
// environment-layer core (design §3–§6): the two orthogonal axes — a
// Provisioner (WHERE the machine comes from) and a Containment (WHAT the agent
// may touch) — meeting at the Workspace contract (an exec + file-transfer
// transport onto a machine), the core-owned Compose combinator, the lifecycle
// engine (Env), the shell-argv helpers, retention resolution, and the shipped
// degenerate implementations (the Local provisioner and the None containment).
//
// It is a batch request/response model: Workspace.Exec takes a context.Context,
// an argv, and options, and returns an ExecResult{Code, Stdout, Stderr} — there
// is no PTY or streaming. The meta-harness "Go-style Context" type maps 1:1 to
// context.Context as the first parameter of every transport method.
//
// The package intentionally has NO driver dependency. Child packages
// (internal/env/openshell, internal/env/daytona) will import THIS package; the
// core imports none of them, keeping the driver->core import direction and
// avoiding a cycle.
package env
