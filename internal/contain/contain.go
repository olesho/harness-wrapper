// Package contain turns a containment.Request into a contained launch: it
// resolves the harness profile, pins every granted path, provisions private
// state and the child environment, sets up cgroup supervision, and starts the
// harness from a dedicated locked OS thread that holds a private descriptor
// table and a thread-scoped Landlock domain (ADR-004).
//
// pkg/wrapper is its only production caller. Nothing here runs unless a caller
// requested containment; the uncontained spawn path never reaches this
// package.
package contain

import (
	"context"
	"errors"
	"fmt"

	"github.com/olesho/harness-wrapper/pkg/containment"
)

// LaunchOptions are the lifecycle choices a caller inside this module makes
// for a contained launch that the public wrapper.Containment request does not
// express: pkg/chat's stored conversations reuse persistent state and require
// supervision, and the structured runner keeps the harness's files until it
// has read them. They travel on the context wrapper.Start already takes, so
// the public API carries only the policy.
type LaunchOptions struct {
	// State is managed state that outlives the launch; see Input.State.
	State *State
	// RequireSupervision refuses the launch without cgroup supervision.
	RequireSupervision bool
	// SingleLaunch marks a chat conversation that will never be reopened
	// (harness.RunTurn, chatd, the structured runner): pkg/chat then keeps
	// its record but neither allocates persistent state nor requires
	// supervision, and records it as not resumable.
	SingleLaunch bool
	// ExpectTargets are the canonical targets recorded when a stored
	// conversation was created (see Targets); a resumed launch whose paths
	// now resolve elsewhere is refused.
	ExpectTargets map[string]string
	// Login marks a human-led sign-in; see Input.Login.
	Login bool
}

type launchOptionsKey struct{}

// WithLaunchOptions attaches o to ctx.
func WithLaunchOptions(ctx context.Context, o LaunchOptions) context.Context {
	return context.WithValue(ctx, launchOptionsKey{}, o)
}

// LaunchOptionsFrom returns the options attached to ctx, or the zero value.
func LaunchOptionsFrom(ctx context.Context) LaunchOptions {
	if ctx == nil {
		return LaunchOptions{}
	}
	o, _ := ctx.Value(launchOptionsKey{}).(LaunchOptions)
	return o
}

// Errors. pkg/wrapper maps them onto its own sentinels.
var (
	// ErrUnsupported: containment is not available on this platform.
	ErrUnsupported = errors.New("contain: unsupported platform")
	// ErrRefused: the request cannot be honoured as asked. Always wrapped in
	// a *RefusalError naming the stage.
	ErrRefused = errors.New("contain: refused")
	// ErrStateGone: the managed state an id names no longer exists.
	ErrStateGone = errors.New("managed state no longer exists")
)

// Refusal stages, in the order a launch meets them.
const (
	StageRequest     = "request"     // the request itself is invalid
	StageProfile     = "profile"     // no profile, profile inactive, or harness mode unsupported
	StageExecutable  = "executable"  // unknown or unverifiable install layout
	StagePaths       = "paths"       // a grant is missing, overlapping or forbidden
	StageState       = "state"       // private or caller-managed state unusable
	StageKernel      = "kernel"      // Landlock unavailable or below the required ABI
	StageSupervision = "supervision" // cgroup supervision required but unavailable
	StageRuleset     = "ruleset"     // a rule could not be installed
	StageLaunch      = "launch"      // the spawn thread failed before exec
)

// RefusalError explains why a contained launch was refused before the harness
// started.
type RefusalError struct {
	Stage string
	Err   error
}

func (e *RefusalError) Error() string {
	return fmt.Sprintf("containment refused (%s): %v", e.Stage, e.Err)
}

// Unwrap exposes both ErrRefused and the cause.
func (e *RefusalError) Unwrap() []error { return []error{ErrRefused, e.Err} }

func refuse(stage string, format string, args ...any) error {
	return &RefusalError{Stage: stage, Err: fmt.Errorf(format, args...)}
}

func refuseErr(stage string, err error) error {
	var re *RefusalError
	if errors.As(err, &re) {
		return err
	}
	return &RefusalError{Stage: stage, Err: err}
}

// Input is everything a contained launch is prepared from.
type Input struct {
	// Request is the caller's request; Prepare normalizes it.
	Request *containment.Request

	// Harness is wrapper.Config.Harness ("claude", "claude-code", "codex").
	Harness string
	// BinaryPath is wrapper.Config.BinaryPath, resolved like exec.Command
	// resolves it.
	BinaryPath string
	// Args are the final harness arguments (after the wrapper's effort, model
	// and permission injection), excluding argv[0].
	Args []string
	// LaunchRung is wrapper.EffectiveLaunchRung over Args: the canonical
	// permission rung the harness launches at ("" when unknown).
	LaunchRung string
	// WorkingDir is the harness working directory; empty means the wrapper's.
	WorkingDir string
	// Env is the caller's environment for the harness; nil means os.Environ().
	// Only allow-listed, authentication, control and PassEnv names survive.
	Env []string

	// State, when non-nil, is wrapper-managed state that outlives the launch
	// (a stored chat conversation, or a caller that reads the harness's files
	// afterwards). The launch neither creates nor deletes it; when nil, the
	// launch allocates private state and deletes it once its cgroup is empty.
	State *State

	// RequireSupervision refuses the launch when cgroup supervision is
	// unavailable. Launches into persistent State always require it.
	RequireSupervision bool

	// ExpectTargets, when set, maps each requested path (the working
	// directory, caller grants, StateDir) to the canonical target it must
	// still resolve to.
	ExpectTargets map[string]string

	// Login runs the harness's own login or status command for a human-led
	// sign-in (see LoginFlowFor), keeping the login in the request's
	// StateDir, which it requires. Args must be exactly one of those
	// commands; the profile need not be activated yet; codex's rung check
	// does not apply, as neither command runs tools; and the harness's
	// authentication variables are neither passed nor seeded, so the status
	// reports the stored login alone.
	Login bool
}

// Targets returns the requested-path → canonical-target map of an applied
// policy's working directory, caller grants and caller-managed state, which
// a stored conversation records so a resume can refuse a path that now
// resolves elsewhere.
func Targets(a *containment.Applied) map[string]string {
	if a == nil {
		return nil
	}
	out := map[string]string{}
	for _, g := range a.Grants {
		if g.Source != containment.SourceCaller && g.Source != containment.SourceWorkingDir {
			continue
		}
		key := g.Requested
		if key == "" {
			key = g.Path
		}
		out[key] = g.Path
	}
	if a.State.Mode == "caller" && a.State.StateDir != "" {
		key := a.State.StateDirRequested
		if key == "" {
			key = a.State.StateDir
		}
		out[key] = a.State.StateDir
	}
	return out
}

// ProfileID resolves the profile a contained launch of harness would use,
// without touching the harness binary: "<name>@<version>" and the manifest
// version. It refuses exactly as a launch would for an unknown harness or an
// inactive profile.
func ProfileID(harness string) (string, int, error) {
	m, err := profileFor(harness, false)
	if err != nil {
		return "", 0, err
	}
	return m.id(), m.ManifestVersion, nil
}
