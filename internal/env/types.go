package env

import "context"

// Retention is how a run's resources are retained at destroy time (design §4).
//
// The empty value (absent, the common case) destroys on BOTH success and
// failure. Mirrors orche's sandboxRetention ('destroy' | 'keep-on-failure').
type Retention string

const (
	// RetentionAlways keeps on success and run-failure.
	RetentionAlways Retention = "always"
	// RetentionKeepOnFailure keeps only on a failed run.
	RetentionKeepOnFailure Retention = "keep-on-failure"
)

// Outcome is the result of a run, driving retention and teardown decisions.
//
// OutcomeSetupFailure is distinct because a preflight/apply failure leaves
// nothing of debugging value inside the sandbox — it ALWAYS destroys, ignoring
// retention (design §4).
type Outcome string

const (
	OutcomeSuccess      Outcome = "success"
	OutcomeFailure      Outcome = "failure"
	OutcomeSetupFailure Outcome = "setup-failure"
)

// Capability is a capability a credential injector's target must advertise
// (e.g. "egress-proxy"). Open string, not a closed enum (design §6).
type Capability = string

// PathKind selects one of the three workspace path conventions.
type PathKind string

const (
	PathRepo PathKind = "repo"
	PathHome PathKind = "home"
	PathTmp  PathKind = "tmp"
)

// ExecOpts carries per-exec overrides.
type ExecOpts struct {
	// Env is overlaid on the guest process. Transports without an env flag
	// cross it as an in-guest `env K=V` argv prefix (design §3).
	Env map[string]string
	// Cwd is the working directory for the guest process; empty defaults to the
	// repo path.
	Cwd string
	// Stdin, when non-nil, is written to the process's stdin, then closed. A nil
	// pointer means no stdin is supplied.
	Stdin *string
}

// ExecResult is the batch result of an exec.
type ExecResult struct {
	Code   int
	Stdout string
	Stderr string
}

// WorkspaceSpec are the inputs for creating a Workspace. It carries a
// DETERMINISTIC name for crash recovery (orche sandboxName pattern) so a
// crashed run's leftover can be found and deleted before recreate.
type WorkspaceSpec struct {
	// Image is the image / snapshot reference the machine boots from.
	Image string
	// Name is a DETERMINISTIC name identifying an interrupted stage's resource
	// across a host-process death.
	Name string
	// Labels are stamped on the resource for sweep/identification.
	Labels map[string]string
	// AutoStopInterval is the Daytona auto-stop interval in minutes (0
	// disables); ignored by provisioners without one.
	AutoStopInterval int
	// AutoDeleteInterval is the Daytona auto-delete interval in minutes.
	AutoDeleteInterval int
	// Retention policy. Empty destroys on both success and failure.
	Retention Retention
}

// PolicySpec is the trust tier + filesystem/network policy inputs handed to a
// containment's layer generator. None ignores it entirely.
type PolicySpec struct {
	Tier string
	// Extra holds additional open-ended policy keys (the TS `[k]: unknown`).
	Extra map[string]interface{}
}

// Provisioner decides WHERE the machine comes from.
type Provisioner interface {
	Name() string
	// Preflight does host-side checks only, zero resources acquired: CLI/API key
	// present, image resolvable (design §3).
	Preflight(ctx context.Context) error
	Create(ctx context.Context, spec WorkspaceSpec) (Workspace, error)
}

// Workspace is an exec + file-transfer transport onto a machine.
type Workspace interface {
	// Exec runs argv on the machine. env crosses via opts.Env; transports
	// without an env flag cross it as an in-guest `env K=V` argv prefix.
	Exec(ctx context.Context, argv []string, opts *ExecOpts) (ExecResult, error)
	Upload(ctx context.Context, hostPath, guestPath string) error
	Download(ctx context.Context, guestPath, hostPath string) error
	// GuestPath returns path conventions; orche precedent: repo=/sandbox/repo,
	// home=/sandbox/.home.
	GuestPath(kind PathKind) string
	// HostAlias performs a loopback rewrite for guest-reachable host URLs.
	HostAlias(hostURL string) string
	// Destroy honors spec.Retention; see lifecycle rules (§4) for when
	// retention applies.
	Destroy(ctx context.Context, outcome Outcome) error
}

// Containment decides WHAT the agent may touch by contributing a
// ContainmentLayer of primitives that the core Compose combinator decorates a
// Workspace with. Containments never hand-roll a Workspace wrapper.
type Containment interface {
	Name() string
	// Preflight runs runtime capability checks ONLY, executed via the inner
	// workspace's exec (containment runs where inner runs). Operator
	// provisioning is out of preflight.
	Preflight(ctx context.Context, ws Workspace) error
	// Layer returns the primitives consumed by the core Compose combinator (§5).
	Layer(policy PolicySpec) ContainmentLayer
}

// Acquirer is the OPTIONAL (structurally probed) acquire hook of a Containment:
// acquire containment resources (e.g. `openshell sandbox create`) and return a
// layer CLOSED OVER them. Env prefers Acquire over Layer at lifecycle step 4.
// A failed Acquire must best-effort delete its own half-created resources
// before returning an error so it never leaks.
type Acquirer interface {
	Acquire(ctx context.Context, ws Workspace, policy PolicySpec) (ContainmentLayer, error)
}

// ContainmentLayer is the set of primitives a Containment contributes.
type ContainmentLayer interface {
	// ExecWrap wraps an exec, e.g. prefix `openshell sandbox exec -n … -- env
	// K=V …`. An IDENTITY layer returns argv/opts unchanged.
	ExecWrap(argv []string, opts ExecOpts) ([]string, ExecOpts)
	// CrossUpload returns argv (run via inner exec) that moves a staged file
	// across the policy boundary to guestPath. Return nil/empty to signal NO
	// boundary — Compose uploads straight to guestPath via the inner workspace.
	CrossUpload(stagingPath, guestPath string) []string
	// CrossDownload is the symmetric download crossing. Empty means no boundary.
	CrossDownload(guestPath, stagingPath string) []string
	// PathMap lets containment paths shadow inner paths. Return "" to defer to
	// the inner path (identity containment).
	PathMap(kind PathKind) string
	// Teardown returns argv (run via inner exec) tearing the containment down,
	// e.g. `openshell sandbox delete <name>`. Empty means nothing to tear down.
	Teardown() []string
}

// AliasMapper is the OPTIONAL (structurally probed) host-URL rewrite of a
// ContainmentLayer, folded on top of the inner's for a two-hop alias (§5.1).
type AliasMapper interface {
	AliasMap(hostURL string) string
}

// CredentialInjector applies secrets to a workspace and cleans them up.
type CredentialInjector interface {
	// Requires lists capabilities the target must advertise; checked before any
	// resource is acquired (design §6).
	Requires() []Capability
	Apply(ctx context.Context, ws Workspace) error
	// Redactions are secrets registered for log redaction — registered BEFORE
	// apply begins (§4).
	Redactions() []string
	// Cleanup is bound to destroy; idempotent; runs even on failure paths /
	// half-failed apply.
	Cleanup(ctx context.Context, ws Workspace) error
}

// Redactor is a sink for secret strings to scrub from logs. The core registers
// every injector's Redactions here BEFORE any Apply (design §4).
type Redactor interface {
	Register(secret string)
}

// EnvConfig are the selectors + optional policy/credential wiring handed to the
// core Env factory.
type EnvConfig struct {
	Provision Provisioner
	Contain   Containment
	Spec      WorkspaceSpec
	Policy    PolicySpec
	Injectors []CredentialInjector
	// Redactor is where injector redactions are registered. Nil defaults to a
	// no-op sink.
	Redactor Redactor
}

// Environment is the acquired environment handed back by Env: the composed
// workspace plus a retention-honoring, reverse-order destroy.
type Environment struct {
	Workspace Workspace
	Destroy   func(ctx context.Context, outcome Outcome) error
}
