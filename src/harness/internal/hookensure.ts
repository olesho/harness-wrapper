// Hook-install primitives: rendering a self-guarding hook command, and a
// sync cross-process lock + atomic write for the config files that hold it.
//
// Ported from harness-wrapper's pkg/harness/hookensure.go. Stdlib-only: this
// file has NO imports from anywhere else in src/harness/**, matching the Go
// original's package-local (no cross-file dependency) shape.

import { constants, closeSync, mkdirSync, openSync, readFileSync, renameSync, statSync, unlinkSync, writeFileSync, writeSync } from "node:fs"
import { dirname } from "node:path"

/**
 * Marker prefix tagging meta-harness-managed hook commands so an ensure call
 * can identify, refresh, or remove its own entries without disturbing the
 * user's other hooks. Rendered as a trailing shell comment (inert at
 * execution) followed by the owner string: `meta-harness-hook:<owner>`.
 *
 * Deliberately DIFFERENT from Go harness-wrapper's `"harness-wrapper-hook:"`
 * marker (hookensure.go:15). The two lockfiles do not interlock (see
 * withLockedFile's doc comment) — if this TS port and the Go original ever
 * manage the same worktree's settings.json, they must never recognize (and
 * therefore overwrite) each other's managed entries. A distinct marker keeps
 * that possible failure mode to "both sets of entries coexist" instead of
 * "one silently deletes the other's".
 */
export const HOOK_MARKER_PREFIX = "meta-harness-hook:"

/**
 * Reports whether a rendered hook command is one this port owns (by the
 * marker), so a merge can replace/remove it idempotently.
 */
export function isManagedHookCommand(command: string): boolean {
  return command.includes(HOOK_MARKER_PREFIX)
}

/**
 * Builds the hook command string written into a harness's config. It is
 * ALWAYS a POSIX shell command with a pre-exec env guard so a left-in-place
 * entry is inert on a non-wrapper run:
 *
 *   sh -c 'test -n "$HW_EVENT_SPOOL" || exit 0; exec <argv> <harnessName> <arg>' # meta-harness-hook:<owner>
 *
 * With HW_EVENT_SPOOL unset the guard exits 0 WITHOUT touching the target
 * binary, so a stale/moved entry-point can never break someone else's run.
 * `argv` is the entry-point path + subcommand (e.g. `["/abs/meta-harness",
 * "hooks"]`); every interpolated value is POSIX-single-quoted, so
 * spaces/quotes in the path are safe.
 */
export function renderHookCommand(argv: readonly string[], harnessName: string, arg: string, owner: string): string {
  const tokens = [...argv, harnessName, arg]
  const quoted = tokens.map(posixSingleQuote)
  const inner = `test -n "$HW_EVENT_SPOOL" || exit 0; exec ${quoted.join(" ")}`
  return `sh -c ${posixSingleQuote(inner)} # ${HOOK_MARKER_PREFIX}${owner}`
}

/** Wraps s in single quotes for POSIX sh, escaping embedded quotes as `'\''`. */
function posixSingleQuote(s: string): string {
  return `'${s.split("'").join("'\\''")}'`
}

/** Sidecar lock is presumed abandoned (and taken over) once it is this old. */
const STALE_LOCK_MS = 10_000
/** Backoff between failed lock-acquisition attempts. */
const LOCK_POLL_MS = 20

/**
 * Runs fn under an exclusive lock on targetPath so concurrent same-worktree
 * ensures don't clobber each other, then writes fn's result atomically.
 *
 * The lock is a STABLE SIDECAR file (`<targetPath>.lock`), matching Go's
 * design (hookensure.go:61) of locking a stable path rather than the target
 * itself (the target is replaced via temp+rename, so a lock held on its old
 * inode/path would be dropped the instant the rename lands). fn receives the
 * current file bytes (null if absent) and returns the new content, or null to
 * mean "no change" (nothing is written). The target itself is written via
 * temp+rename for torn-read safety — see atomicWriteFile.
 *
 * ## Lock mechanism: O_EXCL + stale takeover, NOT flock
 *
 * Go's original takes `syscall.Flock` — an OS advisory lock tied to an open
 * file descriptor that the kernel releases automatically if the holder dies,
 * even via SIGKILL. Node has no synchronous flock binding, so this port
 * instead creates the sidecar with `O_CREAT | O_EXCL` (fails if it already
 * exists) and polls: if the existing sidecar's mtime is older than ~10s, it is
 * presumed abandoned (a crashed holder) and taken over (unlinked + recreated).
 * Two consequences follow:
 *
 * 1. **No auto-release on SIGKILL.** Unlike flock, a killed holder leaves the
 *    sidecar in place; every other locker blocks for up to the ~10s
 *    staleness window before takeover kicks in. Deliberate
 *    availability/complexity tradeoff, not an oversight.
 *
 * 2. **This TS lock and Go's flock do not interlock.** A Go harness-wrapper
 *    process and this TS port never observe each other's lock at all — Go
 *    holds an advisory kernel lock this port's O_EXCL check never consults,
 *    and vice versa. If both manage the same settings.json concurrently, one
 *    process's write can be silently lost (last-write-wins on the atomic
 *    rename). Atomic temp+rename writes still rule out a TORN/partially
 *    written file either way — but a lost *merge* is possible until the next
 *    self-healing `ensure` call re-applies it.
 *
 * The sidecar is also NEVER cleaned up after use — matching Go's behavior of
 * leaving `<targetPath>.lock` on disk permanently. This is BY DESIGN, not a
 * leak: the staleness check needs a stable path to stat across the *next*
 * invocation (very possibly a different process), and removing it after use
 * would just recreate the same "doesn't exist yet" race that O_EXCL exists to
 * close. (Go's comment: flock needs a stable inode to lock, so cleanup would
 * defeat the guard — same reasoning applies here to the stable path.)
 *
 * ## Blocking scope: the whole process, not one caller
 *
 * THIS IS A CATEGORICAL DIFFERENCE FROM GO, NOT MERELY AN IMPLEMENTATION
 * DETAIL. Go's `syscall.Flock` (hookensure.go:61) blocks only the calling
 * GOROUTINE — every other goroutine in the same Go process (other concurrent
 * harness runs, other conversations) keeps being scheduled while one
 * goroutine waits on the lock. This port's contention loop runs synchronous
 * `openSync`/`statSync` calls and sleeps via `Atomics.wait`, both of which run
 * on Node's SINGLE THREAD. During lock contention, EVERY OTHER active
 * operation in the same process stalls for the contention window — up to the
 * ~10s stale-takeover ceiling in the worst case — including other
 * conversations' event loops, timers, and in-flight I/O callbacks.
 * meta-harness is explicitly designed to host multiple concurrent chat
 * `Conversation`s in one process, so this is not a hypothetical: a caller
 * invoking `withLockedFile` from a busy host process should expect a
 * lock-contended call to pause ALL unrelated work process-wide, not just its
 * own call stack.
 */
export function withLockedFile(targetPath: string, fn: (existing: Buffer | null) => Buffer | Uint8Array | null): void {
  mkdirSync(dirname(targetPath), { recursive: true })
  const lockPath = `${targetPath}.lock`
  const fd = acquireLock(lockPath)
  try {
    let existing: Buffer | null = null
    try {
      existing = readFileSync(targetPath)
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err
    }
    const next = fn(existing)
    if (next === null) return
    atomicWriteFile(targetPath, next)
  } finally {
    closeSync(fd)
  }
}

/** Acquires the sidecar lock (O_EXCL + stale takeover), returning its fd. */
function acquireLock(lockPath: string): number {
  for (;;) {
    try {
      const fd = openSync(lockPath, constants.O_CREAT | constants.O_EXCL | constants.O_RDWR, 0o600)
      try {
        writeSync(fd, `${process.pid}\n`)
      } catch {
        // Diagnostic only; a failed pid write must not fail the lock itself.
      }
      return fd
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code !== "EEXIST") throw err
    }
    if (takeOverIfStale(lockPath)) continue
    sleepSync(LOCK_POLL_MS)
  }
}

/** Unlinks lockPath if it is absent or older than STALE_LOCK_MS. */
function takeOverIfStale(lockPath: string): boolean {
  let mtimeMs: number
  try {
    mtimeMs = statSync(lockPath).mtimeMs
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return true // raced with the holder; retry immediately
    throw err
  }
  if (Date.now() - mtimeMs < STALE_LOCK_MS) return false
  try {
    unlinkSync(lockPath)
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err
  }
  return true
}

/** Synchronously blocks the current thread for ms via Atomics.wait. */
function sleepSync(ms: number): void {
  const view = new Int32Array(new SharedArrayBuffer(4))
  Atomics.wait(view, 0, 0, ms)
}

let tmpSeq = 0

/**
 * Writes data to a uniquely-named temp file in the target's directory and
 * renames it into place (atomic on the same filesystem).
 */
export function atomicWriteFile(path: string, data: Buffer | Uint8Array): void {
  const tmp = `${path}.tmp-${Date.now()}-${process.pid}-${tmpSeq++}`
  writeFileSync(tmp, data, { mode: 0o600 })
  try {
    renameSync(tmp, path)
  } catch (err) {
    try {
      unlinkSync(tmp)
    } catch {
      // Best-effort cleanup; the original error is what matters.
    }
    throw err
  }
}
