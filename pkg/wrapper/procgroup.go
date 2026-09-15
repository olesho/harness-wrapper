package wrapper

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// groupReapBudget bounds how long a terminated run waits, after SIGKILLing
// what is left of the harness's process group, for the group to empty. Killed
// members vanish as soon as their parent (init, once the leader is gone) reaps
// them; this only matters on a host whose reaper is slow or absent, and a
// member still listed then is a zombie, not a running process.
const groupReapBudget = 2 * time.Second

// groupPollInterval is how often a terminating run re-checks whether its
// process group has emptied.
const groupPollInterval = 20 * time.Millisecond

// groupTerminator owns the termination of one harness and everything it
// spawned. The harness is a session leader (pty.Start sets Setsid), so its
// process group is its whole tool subtree; see the package doc.
//
// Two rules make group signals safe, and both live here:
//
//   - The group is resolved while the leader cannot yet have been reaped, so
//     its ID is the leader's own and cannot belong to anyone else. A group that
//     does not resolve, or resolves to 0, 1 or the wrapper's own group, is never
//     signalled; the harness alone is.
//   - After the leader is reaped, the group is signalled only while it still has
//     members. A process-group ID cannot be recycled while any member exists, so
//     that check keeps a late signal off an unrelated group.
//
// And one rule makes termination complete: once SIGTERM has been sent, the
// escalation belongs to finish, which the supervisor runs AFTER reaping the
// leader. The old design armed a timer from the cancel path and disarmed it on
// the leader's reap, and escalated the Stop path only while the leader lived —
// so a leader that exited on SIGTERM left a TERM-ignoring child running.
type groupTerminator struct {
	cmd *exec.Cmd
	// proc is the harness of a contained session, which is not started
	// through exec.Cmd; exactly one of cmd and proc is set.
	proc *os.Process

	mu       sync.Mutex
	resolved bool
	pgid     int       // 0: no safe group, signal the harness process only
	reaped   bool      // the leader has been reaped
	termAt   time.Time // first SIGTERM; zero means termination was never requested
}

// resolveLocked records the harness's process group on first use. exec.Cmd
// sets Process before it can invoke Cancel, and every caller runs before the
// supervisor reaps the leader, so this always sees an unreaped leader.
func (g *groupTerminator) resolveLocked() {
	p := g.process()
	if g.resolved || p == nil {
		return
	}
	g.resolved = true
	g.pgid = resolveSessionGroup(p.Pid)
}

// process returns the harness's process handle, nil before it started.
func (g *groupTerminator) process() *os.Process {
	if g.cmd != nil {
		return g.cmd.Process
	}
	return g.proc
}

// signal delivers sig to the harness's process group, or to the harness alone
// when no safe group is known. The first SIGTERM marks termination as
// requested, which is what obliges finish to escalate. A target that is
// already gone is not an error: a signal error must never change a run's
// classification.
func (g *groupTerminator) signal(sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolveLocked()
	if sig == syscall.SIGTERM && g.termAt.IsZero() {
		g.termAt = time.Now()
	}
	switch {
	case g.pgid > 0 && g.reaped && !groupHasMembers(g.pgid):
		return nil // the group is gone; its ID may already name another group
	case g.pgid > 0:
		return ignoreProcessGone(killGroup(g.pgid, sig))
	case !g.reaped && g.process() != nil:
		return ignoreProcessGone(g.process().Signal(sig))
	}
	return nil
}

// finish runs once the supervisor has reaped the leader. If termination was
// requested it completes the escalation before returning: it waits until
// waitDelay has passed since the SIGTERM or the group has emptied, SIGKILLs
// what is left, then waits up to groupReapBudget for the group to empty. The
// supervisor closes the session only afterwards, which is why Wait and Stop
// return only once the tool subtree is gone.
//
// A harness that exited on its own requested nothing, and finish returns at
// once: descendants it deliberately left running are the caller's business.
func (g *groupTerminator) finish(waitDelay time.Duration) {
	g.mu.Lock()
	g.resolveLocked()
	g.reaped = true
	pgid, termAt := g.pgid, g.termAt
	g.mu.Unlock()
	if pgid <= 0 || termAt.IsZero() {
		return
	}

	killAt := termAt.Add(waitDelay)
	for groupHasMembers(pgid) {
		if wait := time.Until(killAt); wait > 0 {
			time.Sleep(min(groupPollInterval, wait))
			continue
		}
		_ = g.signal(syscall.SIGKILL)
		deadline := time.Now().Add(groupReapBudget)
		for groupHasMembers(pgid) && time.Now().Before(deadline) {
			time.Sleep(groupPollInterval)
		}
		return
	}
}
