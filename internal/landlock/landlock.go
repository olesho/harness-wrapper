// Package landlock is harness-wrapper's thin binding to the Linux Landlock
// LSM. It builds a ruleset from pinned path descriptors and restricts the
// CALLING THREAD ONLY: it never passes LANDLOCK_RESTRICT_SELF_TSYNC and never
// makes an all-thread restriction call, so the wrapper process itself stays
// unrestricted and only what the restricted thread later starts inherits the
// domain.
//
// The package makes its three syscalls through golang.org/x/sys/unix and
// imports no cgo package (go-landlock's syscall package pulls in libcap's psx;
// see ADR-004). Values x/sys v0.43.0 does not define — the ABI 9
// RESOLVE_UNIX right and the network-port rule — are defined here from the
// kernel's include/uapi/linux/landlock.h, and uapi_test.go pins every value
// the package uses against it.
//
// On platforms other than Linux every entry point returns ErrUnsupported.
package landlock

import "errors"

// AccessFS is a set of Landlock filesystem rights (LANDLOCK_ACCESS_FS_*).
type AccessFS uint64

// Filesystem rights, bit-for-bit the kernel UAPI.
const (
	AccessFSExecute     AccessFS = 1 << 0  // ABI 1
	AccessFSWriteFile   AccessFS = 1 << 1  // ABI 1
	AccessFSReadFile    AccessFS = 1 << 2  // ABI 1
	AccessFSReadDir     AccessFS = 1 << 3  // ABI 1
	AccessFSRemoveDir   AccessFS = 1 << 4  // ABI 1
	AccessFSRemoveFile  AccessFS = 1 << 5  // ABI 1
	AccessFSMakeChar    AccessFS = 1 << 6  // ABI 1
	AccessFSMakeDir     AccessFS = 1 << 7  // ABI 1
	AccessFSMakeReg     AccessFS = 1 << 8  // ABI 1
	AccessFSMakeSock    AccessFS = 1 << 9  // ABI 1
	AccessFSMakeFifo    AccessFS = 1 << 10 // ABI 1
	AccessFSMakeBlock   AccessFS = 1 << 11 // ABI 1
	AccessFSMakeSym     AccessFS = 1 << 12 // ABI 1
	AccessFSRefer       AccessFS = 1 << 13 // ABI 2
	AccessFSTruncate    AccessFS = 1 << 14 // ABI 3
	AccessFSIoctlDev    AccessFS = 1 << 15 // ABI 5
	AccessFSResolveUnix AccessFS = 1 << 16 // ABI 9
)

// AccessNet is a set of Landlock network rights (LANDLOCK_ACCESS_NET_*).
type AccessNet uint64

// Network rights (ABI 4).
const (
	AccessNetBindTCP    AccessNet = 1 << 0
	AccessNetConnectTCP AccessNet = 1 << 1
)

// Scope is a set of Landlock IPC scopes (LANDLOCK_SCOPE_*).
type Scope uint64

// IPC scopes (ABI 6).
const (
	ScopeAbstractUnixSocket Scope = 1 << 0
	ScopeSignal             Scope = 1 << 1
)

// RequiredABI is the Landlock ABI every ruleset this package builds needs: the
// handled set includes RESOLVE_UNIX, which ABI 9 introduced.
const RequiredABI = 9

// HandledFS is the explicit, versioned set of filesystem rights every ruleset
// handles: all ABI 9 rights. Rights later kernels add are NOT picked up
// automatically — widening the handled set changes what a policy means, so it
// is a deliberate change here, with its own tests.
const HandledFS = AccessFSExecute | AccessFSWriteFile | AccessFSReadFile | AccessFSReadDir |
	AccessFSRemoveDir | AccessFSRemoveFile | AccessFSMakeChar | AccessFSMakeDir |
	AccessFSMakeReg | AccessFSMakeSock | AccessFSMakeFifo | AccessFSMakeBlock |
	AccessFSMakeSym | AccessFSRefer | AccessFSTruncate | AccessFSIoctlDev |
	AccessFSResolveUnix

// FileRights are the rights that apply to a non-directory. The kernel rejects
// a path-beneath rule on a file that carries any other right, so every file
// rule is masked with it.
const FileRights = AccessFSExecute | AccessFSWriteFile | AccessFSReadFile | AccessFSTruncate | AccessFSIoctlDev

// Named access classes used by harness profiles and caller grants.
const (
	// ReadOnly reads files and lists directories.
	ReadOnly = AccessFSReadFile | AccessFSReadDir
	// ReadExec adds execution, for runtime and install trees.
	ReadExec = ReadOnly | AccessFSExecute
	// ReadWrite is everything a working or state directory needs: read,
	// execute, write, create regular files, directories, sockets, FIFOs and
	// symlinks, remove, truncate and reparent. It deliberately omits device
	// creation (MAKE_CHAR, MAKE_BLOCK), IOCTL_DEV and RESOLVE_UNIX: pathname
	// socket access is never granted, on any path.
	ReadWrite = ReadExec | AccessFSWriteFile | AccessFSRemoveDir | AccessFSRemoveFile |
		AccessFSMakeDir | AccessFSMakeReg | AccessFSMakeSock | AccessFSMakeFifo |
		AccessFSMakeSym | AccessFSTruncate | AccessFSRefer
	// Device is read, write and device ioctls on one device node (the session
	// terminal, /dev/tty).
	Device = AccessFSReadFile | AccessFSWriteFile | AccessFSIoctlDev
)

// Errors. Callers match them with errors.Is; each failure wraps one.
var (
	// ErrUnsupported: this platform has no Landlock (anything but Linux).
	ErrUnsupported = errors.New("landlock: unsupported platform")
	// ErrUnavailable: the kernel cannot enforce the required ABI — Landlock is
	// not built, disabled at boot, blocked by seccomp, or too old.
	ErrUnavailable = errors.New("landlock: unavailable")
	// ErrRuleset: building the ruleset or installing a rule failed.
	ErrRuleset = errors.New("landlock: ruleset")
	// ErrRestrict: PR_SET_NO_NEW_PRIVS or landlock_restrict_self failed.
	ErrRestrict = errors.New("landlock: restrict")
)

var fsRightNames = []struct {
	right AccessFS
	name  string
}{
	{AccessFSExecute, "execute"},
	{AccessFSWriteFile, "write_file"},
	{AccessFSReadFile, "read_file"},
	{AccessFSReadDir, "read_dir"},
	{AccessFSRemoveDir, "remove_dir"},
	{AccessFSRemoveFile, "remove_file"},
	{AccessFSMakeChar, "make_char"},
	{AccessFSMakeDir, "make_dir"},
	{AccessFSMakeReg, "make_reg"},
	{AccessFSMakeSock, "make_sock"},
	{AccessFSMakeFifo, "make_fifo"},
	{AccessFSMakeBlock, "make_block"},
	{AccessFSMakeSym, "make_sym"},
	{AccessFSRefer, "refer"},
	{AccessFSTruncate, "truncate"},
	{AccessFSIoctlDev, "ioctl_dev"},
	{AccessFSResolveUnix, "resolve_unix"},
}

// Names lists a's rights by their UAPI suffix, in bit order.
func (a AccessFS) Names() []string {
	var out []string
	for _, r := range fsRightNames {
		if a&r.right != 0 {
			out = append(out, r.name)
		}
	}
	return out
}

// Names lists s's scopes.
func (s Scope) Names() []string {
	var out []string
	if s&ScopeAbstractUnixSocket != 0 {
		out = append(out, "abstract_unix_socket")
	}
	if s&ScopeSignal != 0 {
		out = append(out, "signal")
	}
	return out
}

// Config describes a ruleset to create.
type Config struct {
	// MinABI is the lowest kernel ABI to accept. Values below RequiredABI are
	// raised to it.
	MinABI int
	// RestrictTCP handles TCP bind and connect: both are denied except for
	// connect rules added with AddConnectTCP.
	RestrictTCP bool
}

// Rule is one path-beneath rule: a pinned descriptor (O_PATH is preferred)
// and the rights to allow beneath it.
type Rule struct {
	FD     int
	Access AccessFS
	// IsDir selects the directory rights; a non-directory rule is masked with
	// FileRights before it is installed.
	IsDir bool
}

// Effective returns the rights actually installed for r.
func (r Rule) Effective() AccessFS {
	if r.IsDir {
		return r.Access & HandledFS
	}
	return r.Access & FileRights
}

// Scopes returns the IPC scopes every ruleset sets: abstract UNIX sockets and
// signals outside the domain are always out of reach.
func Scopes() Scope { return ScopeAbstractUnixSocket | ScopeSignal }
