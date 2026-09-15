//go:build linux

package contain

import (
	"runtime"
	"syscall"

	"github.com/olesho/harness-wrapper/internal/landlock"
	"golang.org/x/sys/unix"
)

// spawnSpec is what the spawn thread needs. Every descriptor in it lives in
// the wrapper's shared table; the spawn thread uses its private copies.
type spawnSpec struct {
	path string
	argv []string
	env  []string
	// dir is where the child starts, normally /proc/self/fd/<N> of the pinned
	// working directory, so the chdir binds to the validated object.
	dir      string
	tty      int // the PTY slave, which becomes descriptors 0-2
	cgroupFD int // -1: no CLONE_INTO_CGROUP
	// ruleset is nil only in tests that drive the spawn on kernels without
	// Landlock ABI 9 (the hosted-runner stress job).
	ruleset *landlock.Ruleset
}

// spawnResult reports a spawn. tid is the spawn thread; handedOff records that
// the first goroutine landed on the main thread and stepped off it.
type spawnResult struct {
	pid       int
	tid       int
	handedOff bool
	stage     string
	err       error
}

// spawn starts the child from a dedicated, locked OS thread that is never
// unlocked (ADR-004):
//
//  1. close_range(3, ~0U, CLOSE_RANGE_UNSHARE|CLOSE_RANGE_CLOEXEC) gives the
//     thread a private copy of the descriptor table with every descriptor
//     from 3 up marked close-on-exec — in one call no concurrent open can
//     race, and without touching the wrapper's own table or flags;
//  2. the Landlock domain is enforced on this thread only (no TSYNC);
//  3. syscall.ForkExec forks from this thread, so the child inherits the
//     private table and the domain and, after exec, holds exactly its
//     terminal on 0-2. exec.Cmd and os.StartProcess are never used here: they
//     ask for CLONE_PIDFD, and the pidfd would land in this thread's private
//     table, where other threads cannot use it;
//  4. the pid is reported, and the last act empties the private table so a
//     thread the runtime parks instead of terminating holds nothing.
//
// The goroutine returns while locked, so the runtime terminates the thread,
// and it never creates new threads from a locked one. The main thread is the
// exception — the runtime parks it forever instead — so a goroutine that lands
// there hands the work to a fresh goroutine, keeping the main thread locked
// (so the retry cannot land on it too) and untouched, and unlocks it after.
func spawn(s spawnSpec) spawnResult {
	ch := make(chan spawnResult, 1)
	go func() {
		runtime.LockOSThread()
		if unix.Gettid() == unix.Getpid() {
			inner := make(chan spawnResult, 1)
			go func() {
				runtime.LockOSThread() // never unlocked
				spawnOnThisThread(s, inner, false)
			}()
			r := <-inner
			runtime.UnlockOSThread() // the main thread was never changed
			r.handedOff = true
			ch <- r
			return
		}
		spawnOnThisThread(s, ch, false)
		// Returning while locked: the runtime terminates this thread.
	}()
	return <-ch
}

// spawnOnThisThread runs steps 1-4 on the calling, locked thread and reports
// on ch. keepTable skips step 4 for a test that inspects the table.
func spawnOnThisThread(s spawnSpec, ch chan<- spawnResult, keepTable bool) {
	res := spawnResult{tid: unix.Gettid()}
	if err := unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_UNSHARE|unix.CLOSE_RANGE_CLOEXEC); err != nil {
		res.stage, res.err = "close_range", err
		ch <- res // the table was not unshared: nothing to empty
		return
	}
	res.pid, res.stage, res.err = forkExecContained(s)
	ch <- res
	if !keepTable {
		// The last act: nothing on this thread uses a descriptor — the
		// runtime's poller descriptors included — after this, and it
		// allocates nothing.
		_ = unix.CloseRange(0, ^uint(0), 0)
	}
}

// forkExecContained enforces the domain on the calling thread and forks the
// child from it.
func forkExecContained(s spawnSpec) (int, string, error) {
	if s.ruleset != nil {
		if err := s.ruleset.RestrictCurrentThread(); err != nil {
			return 0, "landlock", err
		}
	}
	sys := &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if s.cgroupFD >= 0 {
		sys.UseCgroupFD = true
		sys.CgroupFD = s.cgroupFD
	}
	pid, err := syscall.ForkExec(s.path, s.argv, &syscall.ProcAttr{
		Dir:   s.dir,
		Env:   s.env,
		Files: []uintptr{uintptr(s.tty), uintptr(s.tty), uintptr(s.tty)},
		Sys:   sys,
	})
	if err != nil {
		return 0, "fork_exec", err
	}
	return pid, "", nil
}
