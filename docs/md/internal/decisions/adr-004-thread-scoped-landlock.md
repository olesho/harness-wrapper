# ADR-004: Contained launches restrict one locked thread, not the wrapper

**Status:** Accepted (2026-09-15)

**Intent:** principle 7, *say what is enforced, not what is intended*
([INTENT](../../../../INTENT.md#design-principles)) — the [Boundary](#boundary) states what
containment guarantees and what it does not, each with its cause. It also meets the success
criterion that an uncontained harness behaves as if containment did not exist: those launches never
reach this code.

## Context

Landlock restricts the calling thread and everything that thread later starts. A harness-wrapper
process is long-lived and shared: harness-chatd runs many conversations at once, some contained and
some not, and the wrapper itself must keep writing traces, transcripts and prompt files wherever it
likes. Restricting the whole process — go-landlock's high-level `Restrict…` API does exactly that,
through `LANDLOCK_RESTRICT_SELF_TSYNC` on ABI 8+ or `AllThreadsSyscall`/psx before — would sandbox
the wrapper and every other session with it, irreversibly.

The child must also not inherit the wrapper's descriptors. Go's Linux spawn passes on every descriptor
without `FD_CLOEXEC`, including ones an embedder or a C library opened, and Landlock does not revoke
authority a pre-opened descriptor already carries. An empty `ExtraFiles` is not descriptor
sanitization.

## Decision

A contained launch (`internal/contain`) starts the harness from a **dedicated, locked OS thread that
is never unlocked**:

1. On an ordinary goroutine: pin every granted path (openat2, `O_PATH`), build the ruleset from those
   same descriptors, create the session cgroup, and open the PTY pair from raw descriptors
   (`/dev/ptmx`, `TIOCGPTPEER`) outside Go's netpoller.
2. On a new goroutine that calls `runtime.LockOSThread` and never unlocks:
   `close_range(3, ~0U, CLOSE_RANGE_UNSHARE|CLOSE_RANGE_CLOEXEC)` gives the thread a private copy of
   the descriptor table with every descriptor from 3 up close-on-exec — atomically with respect to
   concurrent opens, and without touching the wrapper's table or flags;
   `prctl(PR_SET_NO_NEW_PRIVS)` and `landlock_restrict_self(fd, 0)` (no TSYNC) restrict this thread
   only; `syscall.ForkExec` with the slave as descriptors 0–2, `Setsid`, `Setctty` and `UseCgroupFD`
   starts the child from it. The pid is reported.
3. The goroutine returns while locked, so the runtime terminates the thread, and the kernel releases
   the private table with it. The runtime never creates new threads from a locked one (`newm` uses the
   template thread).
4. On an ordinary thread the wrapper opens a process handle (`os.FindProcess`, pidfd), wraps the master
   with `os.NewFile`, and supervises the session through the handle and the cgroup.

The main thread is the exception: the runtime parks it forever instead of terminating it (`mexit`),
and a parked thread would keep its private table. A spawn goroutine that lands on it keeps it locked,
hands the work to a fresh goroutine, and unlocks the untouched main thread afterwards. The main
thread is recognized by the tid recorded during package initialization, which the runtime runs
locked to it in every build mode, or as the thread-group leader.

The Landlock syscalls are made through `golang.org/x/sys/unix`. go-landlock is not imported.

## Alternatives

- **Importing go-landlock**: its low-level syscall package imports libcap's cgo `psx` unless the
  final build sets `landlocktsync`, which only an embedder's own build can do — every Linux build of
  every program embedding `pkg/wrapper` would compile C and take psx's licence, contained or not.
- **Re-exec trampoline** (`harness-wrapper __landlock-exec` restricts itself, then execs the harness):
  one more exec per launch, and a binary with the subcommand that embedders of `pkg/wrapper` would
  have to ship or find; re-executing `/proc/self/exe` instead would run the embedder's package
  initialisers before the policy applies. The private table gives the same guarantee in-process.
- **Descriptor hygiene in the shared table** (scan and set close-on-exec before the fork): races
  concurrent opens and changes the wrapper's own flags.
- **`exec.Cmd` on the private-table thread**: the pidfd problem described under
  [Boundary](#boundary).
- **Restrict the whole wrapper**: irreversible for the process, breaks concurrent sessions.

## Boundary

What this guarantees:

- **The domain cannot leak into the wrapper.** It is held by one thread, which dies with its
  goroutine; no other thread is created from it.
- **Everything the harness starts inherits the domain**, permanently: tools, MCP servers, subshells.
- **The child holds exactly its terminal** after exec. Descriptors the wrapper opens after the unshare
  are not in the copy; the copy's descriptors are close-on-exec; `ForkExec` dup2s the slave onto 0–2
  without it.
- **Validation and installation use the same objects.** A rule is added on the descriptor the checks
  ran against, never on a pathname reopened later, so a concurrent symlink or ancestor swap can make
  a launch fail but never grant the replacement.

What it does not:

- For about a millisecond the private copy references every file the process had open, which delays a
  concurrent last close by that long.
- The private table is never emptied before the thread exits. The runtime's own exit path (`mexit` →
  `handoffp` → `wakeNetPoller` → `netpollBreak`) can write the netpoller's eventfd from the spawn
  thread; with the table emptied, that write fails with EBADF and the runtime throws, killing the
  process with the message lost, since descriptor 2 is gone on that thread too.
  `TestSpawnThreadExitUnderTimerLoad` reproduces it within seconds.
- `exec.Cmd.Start` and `os.StartProcess` must never run on the spawn thread: they request
  `CLONE_PIDFD`, the kernel installs the pidfd in the thread's private table, and `os.Process` would
  later use that number on other threads, where it names nothing or an unrelated wrapper file. Hence
  `syscall.ForkExec` and an `os.Process`-based session (wait, signal, the cancel-then-`WaitDelay`
  escalation).
- `PR_SET_PDEATHSIG` must never be set: it fires when the forking *thread* exits, which this design
  does at once.
- A Go plugin initializes on the thread that opens it, so there the main thread is recognized only as
  the thread-group leader. A spawn that still landed on a parked main thread would keep that thread's
  private table, and the session's terminal with it, open.
- Landlock itself leaves `stat`, `chmod`, `chown`, `flock`, `fcntl` and `access` unmediated, and the
  profile grants `/proc` read-only (see [containment](../../guide/containment.md)).

## Consequences

- Contained and uncontained sessions coexist in one process; uncontained launches keep the
  `exec.Cmd` + `pty.Start` path unchanged and never reach this code.
- A contained launch costs one descriptor-table copy and one thread. The prototype measured about
  1.2–2.5 ms per spawn against 1.1–1.8 ms for `pty.Start`. The implementation's stress, on a 5-vCPU
  arm64 VM with eight spawners, four goroutines and (in cgo builds) four C threads opening
  descriptors in tight loops and uncontained sessions alongside, took 2.6–6.9 ms per contained spawn.
- The concurrent-spawn stress (`TestContainedSpawnStress`, 200 × 320 spawns with uncontained
  sessions, a GC loop and Go and C threads opening non-close-on-exec descriptors throughout) is a
  required CI job on x86_64 and arm64; any failed spawn, child on a foreign terminal, inherited
  descriptor or surviving spawn thread fails it.
- [ADR-005](adr-005-apparmor-socket-layer.md) builds on this spawn thread to add the AppArmor socket
  layer on Landlock ABI 6–8.
