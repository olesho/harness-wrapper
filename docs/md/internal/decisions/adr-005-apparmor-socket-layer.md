# ADR-005: Below Landlock ABI 9, an AppArmor profile denies pathname sockets

**Status:** Accepted (2026-09-19)

## Context

A contained launch required Landlock ABI 9 (Linux 7.1) for one right: `LANDLOCK_ACCESS_FS_RESOLVE_UNIX`,
with which the domain refuses connecting to a pathname UNIX socket created outside it — the user's
D-Bus and systemd sockets, container-runtime sockets, SSH agents. Every other right, scope and network
rule the launch uses exists from ABI 6 (Linux 6.12). Hosts on 6.12–7.0 (Debian 13, Ubuntu 26.04) could
not contain a harness at all.

Without `RESOLVE_UNIX`, nothing in Landlock mediates `connect()` on a pathname socket: path resolution
needs no Landlock right, and a Landlock ABI 8 domain connected to an outside socket in the prototype.
seccomp cannot close the gap either — it cannot read the `sockaddr` a pointer names.

## Decision

A request may accept ABI 6–8 by lowering `min_abi` (default still 9). On such a kernel the launch
stacks one static AppArmor profile, `harness-wrapper-contain`, onto the harness:

1. **The profile** (`harness-wrapper contain-apparmor-profile --root DIR…`, installed and loaded by
   root) allows everything but write and append everywhere (`/** rmixlk`), and write beneath its roots
   and on `/dev/null`, `/dev/tty` and `/dev/pts/[0-9]*`. AppArmor checks write permission on a pathname
   socket's path at `connect()`, so outside the roots the connect fails with `EACCES`. Landlock still
   decides every read, write, execute, TCP and IPC access; the profile adds only the socket denial and
   cannot widen anything. `abi <abi/3.0>` keeps it loadable by older parsers, and leaves classes it does
   not name unmediated.
2. **Roots are static; grants are per launch.** The profile's first line records its roots, read from
   `/etc/apparmor.d/harness-wrapper-contain` (root-owned, writable only by root). A launch refuses, at
   the `paths` stage, any writable grant outside every root — the profile would deny the harness's
   writes there. A per-session profile would match `RESOLVE_UNIX` more closely but needs a privileged
   loader on every launch; it was rejected.
3. **Applied per thread, at exec.** The spawn thread (ADR-004) writes `stack harness-wrapper-contain` to
   `/proc/thread-self/attr/apparmor/exec` before `PR_SET_NO_NEW_PRIVS` and `landlock_restrict_self`, so
   the stack applies at the child's exec and nowhere else. `stack`, not `exec`: it can only add
   restrictions to whatever confines the wrapper. A profile that is not loaded fails the write with
   `ENOENT`, before any fork. After `ForkExec` returns (exec succeeded), the wrapper reads the child's
   label and kills it unless the profile is in it, in enforce mode.
4. **Proved before every launch, unprivileged.** The wrapper cannot read the loaded-profile list
   without root, and the distribution does not say whether its AppArmor mediates pathname sockets
   through file rules. So before each launch it listens on a socket outside every root, stacks the
   profile onto a disposable thread (`/proc/thread-self/attr/apparmor/current`), confirms the thread's
   label, and connects: only `EACCES` passes. That one test rejects a missing profile, complain mode,
   and a kernel whose AppArmor lets the connect through. It costs about 0.2 ms. The thread is locked
   and never unlocked, and the main thread is handed off exactly as for the spawn, so the confinement
   dies with the thread.

The applied policy reports `pathname_unix_sockets: "denied_outside_roots"`, handled rights without
`resolve_unix`, and `apparmor: {profile, roots}`. A policy without the layer serializes exactly as
before, so its fingerprint is unchanged.

## Boundary

What this guarantees on ABI 6–8, with the layer installed:

- **No connect to a pathname socket outside the roots**, from the harness or anything it starts,
  including through a symlink or `/proc/<pid>/root`, which AppArmor resolves to the real path. A hard
  link that would give the socket a name beneath a root is refused by Landlock, which lacks `REFER` on
  the source. `TestAppArmorSocketLayer` exercises all three.
- **No weaker launch than asked.** A request that did not lower `min_abi` is refused on these kernels
  as before; a missing or ineffective layer refuses at the `kernel` stage.

What it does not:

- **Creator-based isolation.** A socket beneath a root is reachable whoever created it, so two
  contained sessions under the same root can reach each other's sockets (claude binds
  `$TMPDIR/cc-socks/<pid>.sock`, and TMPDIR is visible in `/proc/<pid>/environ` to the same user).
  At ABI 9 they cannot.
- **Other devices.** Only `/dev/null`, `/dev/tty` and PTY slaves are writable, which is also all the
  harness profiles grant.
- **Hosts without AppArmor.** SELinux-only hosts (Fedora, RHEL) have no layer; they need ABI 9.
