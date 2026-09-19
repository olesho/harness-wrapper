// Package containment holds the data types of harness-wrapper's optional
// Landlock containment: the Request a caller sends, the Applied record the
// wrapper reports once enforcement succeeded, and their canonical forms.
//
// It is a leaf: it touches no operating-system facility and imports nothing
// from this module, so the wire layers (pkg/turnproto, cmd/harness-chatd) and
// the stored chat record can carry these types without depending on the PTY
// runtime. Enforcement lives in pkg/wrapper; this package only describes it.
//
// Containment is opt-in. A nil *Request means "no containment" everywhere, and
// nothing in harness-wrapper creates one on the caller's behalf.
package containment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// KindLandlock is the only containment kind this release implements.
const KindLandlock = "landlock"

// DefaultABI is the Landlock ABI a contained launch requires unless the
// request lowers it. ABI 9 (Linux 7.1) adds LANDLOCK_ACCESS_FS_RESOLVE_UNIX,
// with which the domain itself denies connections to external pathname UNIX
// sockets.
const DefaultABI = 9

// LowestABI is the lowest Landlock ABI a request may accept (Linux 6.12). On
// a kernel below DefaultABI, Landlock cannot deny pathname UNIX socket
// connects, so the launch stacks harness-wrapper's AppArmor socket layer and
// is refused when that layer is not installed or not effective (ADR-005).
const LowestABI = 6

// MinimumABI is DefaultABI, under the name it had before a request could
// lower the ABI.
//
// Deprecated: it is the default, not the lowest ABI a request may accept. Use
// DefaultABI, or LowestABI for the floor.
const MinimumABI = DefaultABI

// SchemaVersion versions the canonical serialization of Applied and of the
// stored requirement record. Readers must refuse versions they do not know.
const SchemaVersion = 1

// Request is a caller's containment request: the optional extra paths and
// network rules a contained launch adds to the harness profile's baseline.
// Every field's zero value is the conservative choice; see Normalize for the
// validation every surface applies.
type Request struct {
	// Kind selects the mechanism. Only KindLandlock is accepted.
	Kind string `json:"kind"`

	// ReadOnly are additional existing absolute paths the harness may read.
	ReadOnly []string `json:"read_only,omitempty"`

	// ReadWrite are additional existing absolute paths the harness may read,
	// write, create in and remove from.
	ReadWrite []string `json:"read_write,omitempty"`

	// RestrictTCP turns TCP filtering on. False (the default) leaves TCP
	// unrestricted; true denies every TCP bind and every TCP connect except to
	// the ports in ConnectTCP.
	RestrictTCP bool `json:"restrict_tcp,omitempty"`

	// ConnectTCP lists the remote ports the harness may connect to when
	// RestrictTCP is set. Empty with RestrictTCP denies all TCP connects;
	// ports without RestrictTCP are invalid.
	ConnectTCP []uint16 `json:"connect_tcp,omitempty"`

	// MinABI is the lowest Landlock ABI the launch accepts. Zero means
	// DefaultABI; a value below LowestABI is invalid. A value below DefaultABI
	// opts into kernels whose Landlock predates RESOLVE_UNIX, where pathname
	// UNIX socket connects are denied by the AppArmor socket layer instead —
	// by path, outside its installed roots, rather than by who created the
	// socket (ADR-005).
	MinABI int `json:"min_abi,omitempty"`

	// StateDir selects caller-managed persistent state (HOME and harness state
	// under it) instead of wrapper-managed private state. Sessions granted the
	// same StateDir share it deliberately.
	StateDir string `json:"state_dir,omitempty"`

	// PassEnv names additional variables to inherit from the caller's
	// environment. Values never appear in a policy; reserved HOME, temporary
	// and harness-state variables cannot be named.
	PassEnv []string `json:"pass_env,omitempty"`
}

// ErrInvalidRequest is wrapped by every Normalize failure.
var ErrInvalidRequest = errors.New("containment: invalid request")

// envNameRE matches a portable environment variable name.
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ReservedEnv reports whether name is a variable the wrapper controls inside a
// contained session: HOME, the temporary-directory variables and the harness
// state roots. PassEnv may not name them, because inheriting one would point
// the harness back at the caller's own state.
func ReservedEnv(name string) bool {
	switch name {
	case "HOME", "TMPDIR", "TMP", "TEMP", "PWD",
		"CLAUDE_CONFIG_DIR", "CLAUDE_CODE_TMPDIR", "CODEX_HOME",
		"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME":
		return true
	}
	return false
}

// Normalize validates r and returns its canonical form: kind checked, paths
// cleaned (still as the caller wrote them — canonical targets are resolved at
// launch), duplicates removed, lists sorted and the minimum ABI made explicit.
// Two requests that normalize to equal values ask for the same policy.
//
// A nil request normalizes to nil: no containment.
func Normalize(r *Request) (*Request, error) {
	if r == nil {
		return nil, nil
	}
	if r.Kind != KindLandlock {
		return nil, fmt.Errorf("%w: unknown kind %q (supported: %q)", ErrInvalidRequest, r.Kind, KindLandlock)
	}
	out := &Request{Kind: r.Kind, RestrictTCP: r.RestrictTCP}

	var err error
	if out.ReadOnly, err = normalizePaths("read-only", r.ReadOnly); err != nil {
		return nil, err
	}
	if out.ReadWrite, err = normalizePaths("read-write", r.ReadWrite); err != nil {
		return nil, err
	}

	if len(r.ConnectTCP) > 0 && !r.RestrictTCP {
		return nil, fmt.Errorf("%w: connect ports %v require RestrictTCP (ports alone would imply a restriction that is not applied)", ErrInvalidRequest, r.ConnectTCP)
	}
	for _, p := range r.ConnectTCP {
		if p == 0 {
			return nil, fmt.Errorf("%w: TCP port 0 is not a connect target", ErrInvalidRequest)
		}
	}
	out.ConnectTCP = sortedUnique(r.ConnectTCP)

	switch {
	case r.MinABI == 0:
		out.MinABI = DefaultABI
	case r.MinABI < LowestABI:
		return nil, fmt.Errorf("%w: MinABI %d is below the supported minimum %d", ErrInvalidRequest, r.MinABI, LowestABI)
	default:
		out.MinABI = r.MinABI
	}

	if r.StateDir != "" {
		if !filepath.IsAbs(r.StateDir) {
			return nil, fmt.Errorf("%w: StateDir %q is not an absolute path", ErrInvalidRequest, r.StateDir)
		}
		out.StateDir = filepath.Clean(r.StateDir)
	}

	for _, name := range r.PassEnv {
		if !envNameRE.MatchString(name) {
			return nil, fmt.Errorf("%w: PassEnv %q is not a valid environment variable name", ErrInvalidRequest, name)
		}
		if ReservedEnv(name) {
			return nil, fmt.Errorf("%w: PassEnv %q is wrapper-controlled inside a contained session", ErrInvalidRequest, name)
		}
	}
	out.PassEnv = sortedUnique(r.PassEnv)
	return out, nil
}

func normalizePaths(label string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p == "" || !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%w: %s path %q is not an absolute path", ErrInvalidRequest, label, p)
		}
		if strings.ContainsRune(p, 0) {
			return nil, fmt.Errorf("%w: %s path %q contains a NUL byte", ErrInvalidRequest, label, p)
		}
		out = append(out, filepath.Clean(p))
	}
	return sortedUnique(out), nil
}

func sortedUnique[T ~string | ~uint16](in []T) []T {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// Equal reports whether a and b request the same policy. Both are normalized
// first; an invalid request is never equal to anything.
func Equal(a, b *Request) bool {
	na, errA := Normalize(a)
	nb, errB := Normalize(b)
	if errA != nil || errB != nil {
		return false
	}
	if na == nil || nb == nil {
		return na == nil && nb == nil
	}
	ja, _ := json.Marshal(na)
	jb, _ := json.Marshal(nb)
	return string(ja) == string(jb)
}

// CLIFlags renders r as harness-wrapper's --contain* command-line flags:
// every option when r is set, none when r is nil. The CLI's tmux re-exec and
// pkg/env's guest-runner argv both use it, so an option added to Request is
// forwarded on every path — a dropped one would run the harness uncontained.
func (r *Request) CLIFlags() []string {
	if r == nil {
		return nil
	}
	argv := []string{"--contain", r.Kind}
	for _, p := range r.ReadWrite {
		argv = append(argv, "--contain-rw", p)
	}
	for _, p := range r.ReadOnly {
		argv = append(argv, "--contain-ro", p)
	}
	if r.RestrictTCP {
		argv = append(argv, "--contain-restrict-tcp")
	}
	for _, p := range r.ConnectTCP {
		argv = append(argv, "--contain-allow-tcp", fmt.Sprint(p))
	}
	if r.MinABI != 0 {
		argv = append(argv, "--contain-min-abi", fmt.Sprint(r.MinABI))
	}
	if r.StateDir != "" {
		argv = append(argv, "--contain-state-dir", r.StateDir)
	}
	for _, n := range r.PassEnv {
		argv = append(argv, "--contain-pass-env", n)
	}
	return argv
}

// Clone returns a deep copy of r (nil for nil).
func (r *Request) Clone() *Request {
	if r == nil {
		return nil
	}
	c := *r
	c.ReadOnly = slices.Clone(r.ReadOnly)
	c.ReadWrite = slices.Clone(r.ReadWrite)
	c.ConnectTCP = slices.Clone(r.ConnectTCP)
	c.PassEnv = slices.Clone(r.PassEnv)
	return &c
}

// Grant sources: who asked for a path to be reachable.
const (
	SourceProfile    = "profile"     // the harness profile's baseline
	SourceWorkingDir = "working_dir" // the session's working directory
	SourceState      = "state"       // private or caller-managed harness state
	SourceCaller     = "caller"      // Request.ReadOnly / Request.ReadWrite
	SourceDevice     = "device"      // device nodes such as /dev/null and /dev/tty
	SourceTerminal   = "terminal"    // the session's own PTY slave
)

// Grant is one filesystem rule of an applied policy.
type Grant struct {
	// Path is the canonical target the rule was installed on (symlinks
	// resolved), or a placeholder such as "$STATE/home" in a preview.
	Path string `json:"path"`
	// Access is a short class name: "ro", "rx", "rw" or "dev".
	Access string `json:"access"`
	// Rights lists the Landlock filesystem rights of the rule, masked to the
	// rights applicable to the target (a regular file cannot carry directory
	// rights).
	Rights []string `json:"rights"`
	// Source records why the path is reachable (Source* constants).
	Source string `json:"source"`
	// Requested is the path as the caller or profile wrote it, when it differs
	// from Path.
	Requested string `json:"requested,omitempty"`
}

// TCP describes the network part of an applied policy.
type TCP struct {
	// Mode is "unrestricted" (filtering off) or "restricted".
	Mode string `json:"mode"`
	// Connect lists the permitted remote ports when restricted; empty means
	// every TCP connect is denied.
	Connect []uint16 `json:"connect,omitempty"`
	// Bind is "unrestricted" or "denied".
	Bind string `json:"bind"`
}

// State describes where the harness keeps HOME and its own state.
type State struct {
	// Mode is "private" (wrapper-managed, one directory per session or stored
	// conversation) or "caller" (Request.StateDir, shared deliberately).
	Mode string `json:"mode"`
	// ID names wrapper-managed state; empty for caller-managed state.
	ID string `json:"id,omitempty"`
	// Home, Tmp and HarnessState are the directories provisioned as HOME,
	// TMPDIR and the harness-specific state root; HarnessStateEnv names the
	// variable pointing the harness at the latter (CLAUDE_CONFIG_DIR,
	// CODEX_HOME). Wrapper-side readers of the harness's session files use
	// exactly these directories, never the caller's own.
	Home            string `json:"home"`
	Tmp             string `json:"tmp"`
	HarnessState    string `json:"harness_state,omitempty"`
	HarnessStateEnv string `json:"harness_state_env,omitempty"`
	// StateDir is the canonical caller-managed state directory ("caller"
	// mode only), and StateDirRequested the path the request named when it
	// differs.
	StateDir          string `json:"state_dir,omitempty"`
	StateDirRequested string `json:"state_dir_requested,omitempty"`
}

// Supervision records how the session's process tree is supervised.
type Supervision struct {
	// Mode is "cgroup" (a per-session cgroup v2 contains every descendant) or
	// "none" (the host delegates no cgroup; see the cleanup caveats).
	Mode string `json:"mode"`
	// Cgroup is the session cgroup's path under "cgroup" supervision.
	Cgroup string `json:"cgroup,omitempty"`
	// Reason explains "none".
	Reason string `json:"reason,omitempty"`
	// Cleanup is empty while the session runs, then "complete" once every
	// process in the session cgroup is gone and ephemeral state is deleted,
	// or "incomplete: <why>" — always the outcome without supervision, when
	// private state is kept for the caller to remove.
	Cleanup string `json:"cleanup,omitempty"`
}

// PathnameSockets values.
const (
	// PathnameSocketsDenied: Landlock (RESOLVE_UNIX) refuses connecting to a
	// pathname UNIX socket created outside the domain, on every path.
	PathnameSocketsDenied = "denied"
	// PathnameSocketsDeniedOutsideRoots: the kernel's Landlock predates
	// RESOLVE_UNIX, and the AppArmor socket layer refuses connecting to a
	// pathname UNIX socket outside its roots (and the devices a harness
	// writes). A socket beneath a root is reachable whoever created it.
	PathnameSocketsDeniedOutsideRoots = "denied_outside_roots"
)

// AppArmorLayer is the AppArmor profile stacked onto a harness whose kernel's
// Landlock predates RESOLVE_UNIX.
type AppArmorLayer struct {
	// Profile is the stacked profile's name.
	Profile string `json:"profile"`
	// Roots are the directories beneath which the profile allows writes and
	// pathname UNIX socket connects.
	Roots []string `json:"roots"`
}

// Supervision modes.
const (
	SupervisionCgroup = "cgroup"
	SupervisionNone   = "none"
)

// Applied is the normalized effective policy of a contained launch, recorded
// only after enforcement succeeded. Its Fingerprint identifies the policy
// independently of per-session paths, the kernel's ABI and supervision.
type Applied struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	// ABI is the kernel's Landlock ABI; RequiredABI is the minimum the
	// request demanded.
	ABI         int `json:"abi"`
	RequiredABI int `json:"required_abi"`
	// Profile is the harness profile, "<name>@<harness version>", and
	// ProfileVersion the version of its manifest.
	Profile        string `json:"profile"`
	ProfileVersion int    `json:"profile_version"`
	// HandledFS lists every filesystem right the ruleset handles: anything
	// handled and not granted by a rule is denied.
	HandledFS []string `json:"handled_fs"`
	Grants    []Grant  `json:"grants"`
	TCP       TCP      `json:"tcp"`
	// PathnameSockets is PathnameSocketsDenied or
	// PathnameSocketsDeniedOutsideRoots.
	PathnameSockets string `json:"pathname_unix_sockets"`
	// AppArmor describes the AppArmor socket layer, set only when it enforces
	// PathnameSockets (PathnameSocketsDeniedOutsideRoots).
	AppArmor *AppArmorLayer `json:"apparmor,omitempty"`
	// Scopes lists the IPC scopes: "abstract_unix_socket" and "signal".
	Scopes      []string    `json:"scopes"`
	State       State       `json:"state"`
	Supervision Supervision `json:"supervision"`
	// Env lists the names of the variables provisioned into the harness
	// environment. Never values.
	Env []string `json:"env"`
	// Omitted lists optional profile paths absent on this host.
	Omitted []string `json:"omitted,omitempty"`
	// Fingerprint is Fingerprint(a), filled in by the wrapper.
	Fingerprint string `json:"fingerprint"`
}

// Clone returns a deep copy of a (nil for nil).
func (a *Applied) Clone() *Applied {
	if a == nil {
		return nil
	}
	c := *a
	c.HandledFS = slices.Clone(a.HandledFS)
	c.Grants = make([]Grant, len(a.Grants))
	for i, g := range a.Grants {
		g.Rights = slices.Clone(g.Rights)
		c.Grants[i] = g
	}
	c.TCP.Connect = slices.Clone(a.TCP.Connect)
	c.Scopes = slices.Clone(a.Scopes)
	c.Env = slices.Clone(a.Env)
	c.Omitted = slices.Clone(a.Omitted)
	if a.AppArmor != nil {
		aa := *a.AppArmor
		aa.Roots = slices.Clone(a.AppArmor.Roots)
		c.AppArmor = &aa
	}
	return &c
}

// fingerprintView is the policy-defining part of Applied, with per-session
// state paths replaced by placeholders so that two sessions under the same
// policy share a fingerprint.
type fingerprintView struct {
	SchemaVersion   int      `json:"schema_version"`
	Kind            string   `json:"kind"`
	RequiredABI     int      `json:"required_abi"`
	Profile         string   `json:"profile"`
	ProfileVersion  int      `json:"profile_version"`
	HandledFS       []string `json:"handled_fs"`
	Grants          []Grant  `json:"grants"`
	TCP             TCP      `json:"tcp"`
	PathnameSockets string   `json:"pathname_unix_sockets"`
	// AppArmor is omitted when nil, so a policy without the socket layer keeps
	// the fingerprint it had before the layer existed.
	AppArmor  *AppArmorLayer `json:"apparmor,omitempty"`
	Scopes    []string       `json:"scopes"`
	StateMode string         `json:"state_mode"`
	Env       []string       `json:"env"`
}

// Fingerprint returns a stable identifier of a's policy: the SHA-256 of its
// canonical serialization, excluding the kernel ABI, supervision and the
// concrete per-session state directories.
func Fingerprint(a *Applied) string {
	if a == nil {
		return ""
	}
	v := fingerprintView{
		SchemaVersion:   a.SchemaVersion,
		Kind:            a.Kind,
		RequiredABI:     a.RequiredABI,
		Profile:         a.Profile,
		ProfileVersion:  a.ProfileVersion,
		HandledFS:       a.HandledFS,
		TCP:             a.TCP,
		PathnameSockets: a.PathnameSockets,
		AppArmor:        a.AppArmor,
		Scopes:          a.Scopes,
		StateMode:       a.State.Mode,
		Env:             a.Env,
	}
	replacer := stateReplacer(a.State)
	for _, g := range a.Grants {
		if g.Source == SourceTerminal {
			g.Path = "$TERMINAL"
		} else {
			g.Path = replacer.Replace(g.Path)
		}
		g.Requested = ""
		v.Grants = append(v.Grants, g)
	}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func stateReplacer(s State) *strings.Replacer {
	var pairs []string
	add := func(path, name string) {
		if path != "" {
			pairs = append(pairs, path, name)
		}
	}
	// Longest first: HarnessState may sit inside Home.
	add(s.HarnessState, "$STATE/harness")
	add(s.Home, "$STATE/home")
	add(s.Tmp, "$STATE/tmp")
	return strings.NewReplacer(pairs...)
}
