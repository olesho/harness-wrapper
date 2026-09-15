//go:build linux

package landlock

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// UAPI values golang.org/x/sys v0.43.0 does not define (include/uapi/linux/
// landlock.h). uapi_test.go pins them.
const (
	// ruleNetPort is LANDLOCK_RULE_NET_PORT, the second value of enum
	// landlock_rule_type (LANDLOCK_RULE_PATH_BENEATH is 1).
	ruleNetPort = 2
)

// netPortAttr is struct landlock_net_port_attr: two __u64, no padding.
type netPortAttr struct {
	allowedAccess uint64
	port          uint64
}

// ABI returns the kernel's Landlock ABI version. It distinguishes the reasons
// Landlock can be missing, because the fix differs: ENOSYS means the syscalls
// are absent or filtered by seccomp, EOPNOTSUPP that Landlock is built in but
// left out of the boot-time LSM list.
func ABI() (int, error) {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if e != 0 {
		return 0, unavailable(e)
	}
	return int(r), nil
}

// Errata returns the kernel's errata bitmask for its ABI, or 0 when the kernel
// cannot report one.
func Errata() int {
	r, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_ERRATA)
	if e != 0 {
		return 0
	}
	return int(r)
}

// Probe reports whether the kernel can enforce the rulesets this package
// builds: it returns the ABI and a non-nil error when that ABI is below
// RequiredABI or Landlock is unavailable. It is advisory — ruleset creation
// and enforcement remain authoritative, and every contained launch performs
// both.
func Probe() (int, error) {
	abi, err := ABI()
	if err != nil {
		return 0, err
	}
	if abi < RequiredABI {
		return abi, fmt.Errorf("%w: kernel Landlock ABI %d is below the required %d", ErrUnavailable, abi, RequiredABI)
	}
	return abi, nil
}

func unavailable(e unix.Errno) error {
	switch e {
	case unix.ENOSYS:
		return fmt.Errorf("%w: Landlock syscalls are unavailable (not built into the kernel, or blocked by seccomp): %w", ErrUnavailable, e)
	case unix.EOPNOTSUPP:
		return fmt.Errorf("%w: Landlock is built in but disabled at boot (not in the lsm= list): %w", ErrUnavailable, e)
	default:
		return fmt.Errorf("%w: %w", ErrUnavailable, e)
	}
}

// Ruleset is an open Landlock ruleset.
type Ruleset struct {
	fd          int
	abi         int
	restrictTCP bool
}

// New creates a ruleset handling HandledFS, both IPC scopes and, when
// cfg.RestrictTCP is set, TCP bind and connect. It fails with ErrUnavailable
// when the kernel's ABI is below max(cfg.MinABI, RequiredABI): kernel
// availability alone is not enough, every handled field must be accepted.
func New(cfg Config) (*Ruleset, error) {
	abi, err := ABI()
	if err != nil {
		return nil, err
	}
	minABI := max(cfg.MinABI, RequiredABI)
	if abi < minABI {
		return nil, fmt.Errorf("%w: kernel Landlock ABI %d is below the required %d", ErrUnavailable, abi, minABI)
	}
	attr := unix.LandlockRulesetAttr{
		Access_fs: uint64(HandledFS),
		Scoped:    uint64(Scopes()),
	}
	if cfg.RestrictTCP {
		attr.Access_net = uint64(AccessNetBindTCP | AccessNetConnectTCP)
	}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if e != 0 {
		return nil, fmt.Errorf("%w: landlock_create_ruleset (abi %d): %w", ErrRuleset, abi, e)
	}
	return &Ruleset{fd: int(fd), abi: abi, restrictTCP: cfg.RestrictTCP}, nil
}

// ABI returns the kernel ABI the ruleset was created under.
func (r *Ruleset) ABI() int { return r.abi }

// AddPath installs a path-beneath rule on rule.FD, the descriptor the caller
// validated, never a pathname reopened here. A rule whose effective rights are
// empty is an error: it would grant nothing while reading as a grant.
func (r *Ruleset) AddPath(rule Rule) error {
	access := rule.Effective()
	if access == 0 {
		return fmt.Errorf("%w: rule on fd %d grants no applicable right (requested %#x)", ErrRuleset, rule.FD, uint64(rule.Access))
	}
	attr := unix.LandlockPathBeneathAttr{Allowed_access: uint64(access), Parent_fd: int32(rule.FD)}
	_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(r.fd), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&attr)), 0, 0, 0)
	if e != 0 {
		return fmt.Errorf("%w: landlock_add_rule(path_beneath, fd %d, %#x): %w", ErrRuleset, rule.FD, uint64(access), e)
	}
	return nil
}

// AddConnectTCP allows connecting to remote TCP port. It is valid only on a
// ruleset created with RestrictTCP; no bind rule exists in this release.
func (r *Ruleset) AddConnectTCP(port uint16) error {
	if !r.restrictTCP {
		return fmt.Errorf("%w: TCP port rule on a ruleset that does not restrict TCP", ErrRuleset)
	}
	attr := netPortAttr{allowedAccess: uint64(AccessNetConnectTCP), port: uint64(port)}
	_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(r.fd), ruleNetPort,
		uintptr(unsafe.Pointer(&attr)), 0, 0, 0)
	if e != 0 {
		return fmt.Errorf("%w: landlock_add_rule(net_port, %d): %w", ErrRuleset, port, e)
	}
	return nil
}

// RestrictCurrentThread sets PR_SET_NO_NEW_PRIVS on the calling thread and
// enforces the ruleset on it with landlock_restrict_self(fd, 0) — no TSYNC, so
// no other thread of the process is touched. The calling goroutine must hold
// runtime.LockOSThread and must never unlock it: the thread carries the domain
// until it exits.
//
// It allocates nothing on success and makes no descriptor calls besides the
// two syscalls, so it is safe on a thread whose descriptor table is private.
func (r *Ruleset) RestrictCurrentThread() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("%w: prctl(PR_SET_NO_NEW_PRIVS): %w", ErrRestrict, err)
	}
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(r.fd), 0, 0); e != 0 {
		return fmt.Errorf("%w: landlock_restrict_self: %w", ErrRestrict, e)
	}
	return nil
}

// FD returns the ruleset descriptor, for callers that must keep it out of a
// descriptor sweep.
func (r *Ruleset) FD() int { return r.fd }

// Close releases the ruleset descriptor. A domain already enforced is not
// affected.
func (r *Ruleset) Close() error {
	if r == nil || r.fd < 0 {
		return nil
	}
	err := unix.Close(r.fd)
	r.fd = -1
	return err
}
