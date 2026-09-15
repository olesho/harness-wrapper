package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/internal/contain"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"github.com/olesho/harness-wrapper/pkg/wrapper/trace"
)

// Containment requests an optional Landlock boundary around the harness child
// on Linux: an extra, kernel-enforced layer outside whatever the harness's own
// sandbox and permission settings enforce, which keep their meanings. A nil
// *Containment (the default everywhere) means no containment, and the
// uncontained launch path is exactly the one used without this feature.
//
// See pkg/containment for the fields and docs/md/guide/permissions.md for what
// the boundary covers and what it does not.
type Containment = containment.Request

// ContainmentLandlock is the only containment kind.
const ContainmentLandlock = containment.KindLandlock

// Containment errors. Both wrap ErrInvalidConfig: a request the wrapper cannot
// honour as asked is refused before the harness starts, never downgraded, and
// transports map it to the same 400 invalid_config as any other bad Config.
var (
	// ErrContainmentUnsupported: containment was requested on a platform
	// without Landlock (anything but Linux).
	ErrContainmentUnsupported = fmt.Errorf("%w: containment is not supported on %s", ErrInvalidConfig, runtime.GOOS)

	// ErrContainmentRefused: the request is invalid, or a required
	// protection cannot be enforced here — kernel support, profile, paths,
	// state, supervision or an unsupported harness mode. The wrapped cause
	// says which.
	ErrContainmentRefused = fmt.Errorf("%w: containment refused", ErrInvalidConfig)

	// ErrLaunchDenied: exec of the harness failed with EACCES on the contained
	// path. It preserves the errno and names the binary but does not prove
	// Landlock was responsible — DAC permissions and other LSMs deny too. It
	// matches ErrPTYAllocation, the sentinel every other start failure matches.
	ErrLaunchDenied = fmt.Errorf("%w: launch denied", ErrPTYAllocation)
)

// validateContainment checks a containment request before anything starts.
// A nil request validates exactly as before on every platform.
func validateContainment(cfg *Config) error {
	if cfg.Containment == nil {
		return nil
	}
	if runtime.GOOS != "linux" {
		return ErrContainmentUnsupported
	}
	if _, err := containment.Normalize(cfg.Containment); err != nil {
		return fmt.Errorf("%w: %w", ErrContainmentRefused, err)
	}
	return nil
}

// containedState is the contained-session half of a Session.
type containedState struct {
	launch *contain.Launch

	mu      sync.Mutex
	applied *containment.Applied
}

// Containment returns the effective policy of a contained session — the
// normalized grants, TCP and IPC scopes, private state, supervision mode and
// fingerprint recorded once enforcement succeeded — or nil for an uncontained
// session. After Wait it also carries the cleanup outcome.
func (s *Session) Containment() *containment.Applied {
	if s.contained == nil {
		return nil
	}
	s.contained.mu.Lock()
	defer s.contained.mu.Unlock()
	return s.contained.applied.Clone()
}

// startContainedSession is startSession's contained branch: profile, pinned
// grants, private state, supervision and ruleset prepared by internal/contain,
// a raw-descriptor PTY pair, and the spawn from a locked thread with a private
// descriptor table. Every refusal happens before the harness starts.
func startContainedSession(ctx context.Context, cfg Config) (*Session, error) {
	opts := contain.LaunchOptionsFrom(ctx)
	launch, err := contain.Prepare(contain.Input{
		Request:            cfg.Containment,
		Harness:            cfg.Harness,
		BinaryPath:         cfg.BinaryPath,
		Args:               cfg.Args,
		LaunchRung:         EffectiveLaunchRung(cfg.Harness, cfg.Args, cfg.PermissionMode),
		WorkingDir:         cfg.WorkingDir,
		Env:                cfg.Env,
		State:              opts.State,
		RequireSupervision: opts.RequireSupervision,
		ExpectTargets:      opts.ExpectTargets,
	})
	if err != nil {
		return nil, containmentStartError(cfg, "prepare", err)
	}

	masterFD, slaveFD, err := contain.OpenPTYPair()
	if err != nil {
		launch.Release()
		return nil, containmentStartError(cfg, "pty", fmt.Errorf("%w: %v", ErrPTYAllocation, err))
	}
	if err := launch.AddTerminal(slaveFD); err != nil {
		_ = syscall.Close(slaveFD)
		_ = syscall.Close(masterFD)
		launch.Release()
		return nil, containmentStartError(cfg, "terminal", err)
	}

	startedAt := time.Now()
	pid, err := launch.Start(slaveFD)
	_ = syscall.Close(slaveFD)
	if err != nil {
		_ = syscall.Close(masterFD)
		launch.Release()
		return nil, containmentStartError(cfg, "start", err)
	}
	// The master becomes an *os.File only now, and os.NewFile leaves a
	// blocking descriptor out of the netpoller.
	ptmx := os.NewFile(uintptr(masterFD), "/dev/ptmx")

	proc, err := os.FindProcess(pid) // pidfd_open, on an ordinary thread
	if err != nil {
		// Cannot happen for an unreaped child; kill it through the cgroup.
		_ = ptmx.Close()
		launch.Finish(pid, false, time.Now())
		return nil, containmentStartError(cfg, "start", fmt.Errorf("%w: open process handle: %v", ErrPTYAllocation, err))
	}
	term := &groupTerminator{proc: proc}
	term.mu.Lock()
	term.resolveLocked()
	term.mu.Unlock()

	applied := launch.Applied()
	cfg.Trace.Emit(trace.Event{At: time.Now(), Kind: "pty_opened", Fields: map[string]any{"pid": pid}})
	cfg.Trace.Emit(trace.Event{At: time.Now(), Kind: "containment_applied", Fields: appliedFields(applied)})

	s := newSession(cfg, ptmx, pid, startedAt, term)
	s.proc = proc
	s.contained = &containedState{launch: launch, applied: applied}
	go s.supervise(ctx)
	return s, nil
}

// containmentStartError classifies a contained start failure and records the
// refusal in the trace. A missing binary stays ErrBinaryNotFound; an EACCES
// from exec is ErrLaunchDenied; anything refused before the harness started is
// ErrContainmentRefused (or ErrContainmentUnsupported).
func containmentStartError(cfg Config, step string, err error) error {
	var out error
	var re *contain.RefusalError
	stage := step
	switch {
	case errors.Is(err, contain.ErrUnsupported):
		out = ErrContainmentUnsupported
	case errors.As(err, &re):
		stage = re.Stage
		out = fmt.Errorf("%w: %w", ErrContainmentRefused, err)
	case isBinaryNotFound(err):
		out = fmt.Errorf("%w: %v", ErrBinaryNotFound, err)
	case errors.Is(err, syscall.EACCES):
		out = fmt.Errorf("%w: exec %s: %w (the kernel refused to execute it; Landlock, file permissions or another security module can each cause this)", ErrLaunchDenied, cfg.BinaryPath, err)
	case errors.Is(err, ErrPTYAllocation):
		out = err
	default:
		out = fmt.Errorf("%w: %v", ErrPTYAllocation, err)
	}
	cfg.Trace.Emit(trace.Event{
		At:     time.Now(),
		Kind:   "containment_refused",
		Fields: map[string]any{"stage": stage, "error": out.Error()},
	})
	return out
}

// appliedFields renders an applied policy as trace fields.
func appliedFields(a *containment.Applied) map[string]any {
	b, err := json.Marshal(a)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// finishContained replaces groupTerminator.finish for a contained session:
// under cgroup supervision the whole tree ends with the session, however the
// harness exited — SIGTERM to its group (unless termination already sent it),
// the grace period, then cgroup.kill and "populated 0" — so Wait returns only
// once the cgroup is empty. Without supervision it escalates over the process
// group exactly as an uncontained session does, and keeps the private state.
func (s *Session) finishContained() {
	s.term.mu.Lock()
	s.term.resolveLocked()
	s.term.reaped = true
	pgid, termAt := s.term.pgid, s.term.termAt
	s.term.mu.Unlock()

	launch := s.contained.launch
	if !launch.Supervised() {
		s.term.finish(s.cfg.WaitDelay)
	}
	deadline := time.Now().Add(s.cfg.WaitDelay)
	if !termAt.IsZero() {
		deadline = termAt.Add(s.cfg.WaitDelay)
	}
	cleanup := launch.Finish(pgid, !termAt.IsZero(), deadline)

	s.contained.mu.Lock()
	s.contained.applied.Supervision.Cleanup = cleanup
	s.contained.mu.Unlock()
	s.cfg.Trace.Emit(trace.Event{
		At:     time.Now(),
		Kind:   "containment_cleanup",
		Fields: map[string]any{"supervision": s.contained.applied.Supervision.Mode, "cleanup": cleanup},
	})
}

// processState returns the harness's exit state once it has been reaped.
func (s *Session) processState() *os.ProcessState {
	if s.contained != nil {
		return s.procState
	}
	return s.cmd.ProcessState
}

// waitContained reaps a contained harness through its process handle and
// reproduces exec.CommandContext's cancellation: SIGTERM to the harness's
// process group when ctx ends, then SIGKILL to the harness itself once
// Config.WaitDelay has passed. The rest of the tree is finishContained's.
func (s *Session) waitContained(ctx context.Context, waitCh chan<- waitResult) {
	leaderDone := make(chan struct{})
	go func() {
		state, err := s.proc.Wait()
		s.procState = state
		close(leaderDone)
		waitCh <- waitResult{err: err, endedAt: time.Now()}
	}()
	go func() {
		select {
		case <-leaderDone:
			return
		case <-ctx.Done():
		}
		_ = s.term.signal(syscall.SIGTERM)
		timer := time.NewTimer(s.cfg.WaitDelay)
		defer timer.Stop()
		select {
		case <-leaderDone:
		case <-timer.C:
			_ = s.proc.Kill()
		}
	}()
}
