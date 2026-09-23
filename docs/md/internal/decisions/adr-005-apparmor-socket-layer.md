# ADR-005: Below Landlock ABI 9, an AppArmor profile denies pathname sockets

**Status:** Accepted (2026-09-19) — builds on [ADR-004](adr-004-thread-scoped-landlock.md)

**Intent:** principle 7, *say what is enforced, not what is intended*
([INTENT](../../../../INTENT.md#design-principles)) — containment refuses rather than degrades, the
applied policy names the rule that held, and the missing creator-based isolation is stated rather
than implied. Also principle 6: the reporting is additive, and an existing policy's fingerprint does
not move.

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

The wrapper detects the kernel's ABI. On ABI 9 and later nothing changes. On ABI 6–8 the launch
stacks one static AppArmor profile, `harness-wrapper-contain`, onto the harness — when root has
installed it, and is refused otherwise:

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
   writes there. **The loaded profile is bound to that file:** it is named
   `harness-wrapper-contain-<digest>`, the first 64 bits of the SHA-256 of the policy rendered under the
   bare stem, and the file must be byte-for-byte what the generator produces for its roots. A launch
   stacks only the name the file implies, so a file changed without a reload — roots narrowed, say —
   names a profile that is not loaded and the launch is refused, instead of reporting roots the kernel
   does not enforce.
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
5. **Detected, not requested.** A request with `min_abi` unset detects; it is kept unset when
   normalized, so it is a distinct policy from `min_abi` 9, which a stored conversation or a turn
   restatement compares exactly. `required_abi` reports what the launch enforced: 9 for Landlock
   alone, 6 (or the request's higher floor) under the layer.

## Alternatives

- **Landlock alone, or seccomp.** Neither can mediate the connect below ABI 9. See Context.
- **A per-session profile.** It would match `RESOLVE_UNIX` more closely, but needs a privileged loader
  on every launch.
- **A per-request opt-in.** Every caller would have had to know the host's kernel, and the consent
  that matters — accepting a path-based socket rule — is the host owner's, given by installing the
  profile.

## Boundary

What this guarantees on ABI 6–8, with the layer installed:

- **No connect to a pathname socket outside the roots**, from the harness or anything it starts,
  including through a symlink or `/proc/<pid>/root`, which AppArmor resolves to the real path. A hard
  link that would give the socket a name beneath a root is refused by Landlock, which lacks `REFER` on
  the source. `TestAppArmorSocketLayer` exercises all three.
- **No weaker launch than asked, and none unannounced.** Installing the profile is the host's opt-in:
  without it, ABI 6–8 refuses as before, and a missing or ineffective layer refuses at the `kernel`
  stage. A request that must have ABI 9's creator-based rule sets `min_abi` 9 and is refused on these
  kernels whatever is installed. The applied policy always says which rule applied.

What it does not:

- **Creator-based isolation.** A socket beneath a root is reachable whoever created it, so two
  contained sessions under the same root can reach each other's sockets (claude binds
  `$TMPDIR/cc-socks/<pid>.sock`, and TMPDIR is visible in `/proc/<pid>/environ` to the same user).
  At ABI 9 they cannot.
- **Other devices.** Only `/dev/null`, `/dev/tty` and PTY slaves are writable, which is also all the
  harness profiles grant.
- **Hosts without AppArmor.** SELinux-only hosts (Fedora, RHEL) have no layer; they need ABI 9.

## Consequences

- Hosts on Linux 6.12–7.0 with AppArmor (Debian 13, Ubuntu 26.04) can contain a harness once root
  has installed the profile. Hosts on ABI 9 and later are unaffected.
- The applied policy reports `pathname_unix_sockets: "denied_outside_roots"`, handled rights without
  `resolve_unix`, and `apparmor: {profile, roots}`. A policy without the layer serializes exactly as
  before, so its fingerprint is unchanged.
- Each contained launch on ABI 6–8 pays the pre-launch proof: about 0.2 ms and one disposable thread.
