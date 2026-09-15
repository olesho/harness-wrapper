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

// mainThreadTID is the runtime's main thread: package initialization runs
// locked to it in every build mode, including c-archive and c-shared, where
// it need not be the thread-group leader.
var mainThreadTID = unix.Gettid()

// forkExec is forkExecContained. A test replaces it to run the spawn thread's
// life many times without forking.
var forkExec = forkExecContained

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
//  4. the pid is reported and the goroutine returns while locked, so the
//     runtime terminates the thread and the kernel releases its private table
//     with it. The table is not emptied first: the runtime's exit path can
//     still write the netpoller's eventfd from this thread (mexit → handoffp →
//     wakeNetPoller → netpollBreak), and an emptied table turns that write
//     into a fatal error for the whole process.
//
// The runtime never creates new threads from a locked one. The main thread is
// the exception to termination — the runtime parks it forever instead, and a
// parked thread would keep its private table — so a goroutine that lands
// there hands the work to a fresh goroutine, keeping the main thread locked
// (so the retry cannot land on it too) and untouched, and unlocks it after.
func spawn(s spawnSpec) spawnResult {
	ch := make(chan spawnResult, 1)
	go func() {
		runtime.LockOSThread()
		if tid := unix.Gettid(); tid == mainThreadTID || tid == unix.Getpid() {
			inner := make(chan spawnResult, 1)
			go func() {
				runtime.LockOSThread() // never unlocked
				spawnOnThisThread(s, inner)
			}()
			r := <-inner
			runtime.UnlockOSThread() // the main thread was never changed
			r.handedOff = true
			ch <- r
			return
		}
		spawnOnThisThread(s, ch)
		// Returning while locked: the runtime terminates this thread.
	}()
	return <-ch
}

// spawnOnThisThread runs steps 1-3 on the calling, locked thread and reports
// on ch; its caller returns while locked (step 4).
func spawnOnThisThread(s spawnSpec, ch chan<- spawnResult) {
	res := spawnResult{tid: unix.Gettid()}
	if err := unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_UNSHARE|unix.CLOSE_RANGE_CLOEXEC); err != nil {
		res.stage, res.err = "close_range", err
		ch <- res
		return
	}
	res.pid, res.stage, res.err = forkExec(s)
	ch <- res
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
