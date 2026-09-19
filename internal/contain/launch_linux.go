//go:build linux

package contain

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/olesho/harness-wrapper/internal/apparmor"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"golang.org/x/sys/unix"
)

// Launch is a prepared contained launch: profile resolved, every grant
// pinned and checked, private state and the child environment provisioned,
// supervision set up and the ruleset built. Nothing has started yet.
type Launch struct {
	req     *containment.Request
	m       *manifest
	applied containment.Applied

	path string
	argv []string
	env  []string

	wd         *pinned
	pins       []*pinned
	callerPins []*pinned // the request's own grants, for the target check
	ruleset    *landlock.Ruleset
	// sockets is the AppArmor socket layer, set when the kernel's Landlock
	// predates RESOLVE_UNIX (ADR-005).
	sockets *apparmor.Layer

	state     *State
	ownsState bool
	cg        *sessionCgroup

	started bool
}

type grantEntry struct {
	pin    *pinned
	class  string
	access landlock.AccessFS
	source string
}

// classAccess maps a manifest or request access class to Landlock rights.
func classAccess(class string) (landlock.AccessFS, error) {
	switch class {
	case "ro":
		return landlock.ReadOnly, nil
	case "rx":
		return landlock.ReadExec, nil
	case "rw":
		return landlock.ReadWrite, nil
	case "dev":
		return landlock.Device, nil
	case "read":
		return landlock.AccessFSReadFile, nil
	case "null":
		return landlock.AccessFSReadFile | landlock.AccessFSWriteFile | landlock.AccessFSTruncate, nil
	default:
		return 0, fmt.Errorf("unknown access class %q", class)
	}
}

// Prepare turns in into a Launch, or refuses it before anything runs. On
// refusal it releases whatever it acquired, including ephemeral state.
func Prepare(in Input) (l *Launch, err error) {
	req, nerr := containment.Normalize(in.Request)
	if nerr != nil {
		return nil, refuseErr(StageRequest, nerr)
	}
	if req == nil {
		return nil, refuse(StageRequest, "no containment requested")
	}
	m, err := profileFor(in.Harness, in.Login)
	if err != nil {
		return nil, err
	}
	if in.Login {
		err = checkLogin(m, req, in)
	} else {
		err = checkHarnessMode(m, in)
	}
	if err != nil {
		return nil, err
	}
	abi, err := landlock.Probe(req.MinABI)
	if err != nil {
		return nil, refuseErr(StageKernel, err)
	}
	var sockets *apparmor.Layer
	if abi < landlock.ResolveUnixABI {
		if sockets, err = socketLayer(abi); err != nil {
			return nil, refuseErr(StageKernel, err)
		}
	}

	callerEnv := in.Env
	if callerEnv == nil {
		callerEnv = os.Environ()
	}
	if in.Login {
		// Neither passed nor seeded: the status must report the login the
		// StateDir holds, which an inherited credential would mask.
		callerEnv = slices.DeleteFunc(slices.Clone(callerEnv), func(kv string) bool {
			name, _, _ := strings.Cut(kv, "=")
			return slices.Contains(m.AuthEnv, name)
		})
	}

	l = &Launch{req: req, m: m, sockets: sockets}
	defer func() {
		if err != nil {
			l.Release()
		}
	}()

	// Working directory, pinned: the child starts in this object, whatever
	// its path names by then.
	wdPath := in.WorkingDir
	if wdPath == "" {
		if wdPath, err = os.Getwd(); err != nil {
			return l, refuseErr(StagePaths, err)
		}
	}
	if l.wd, err = pinPath(wdPath); err != nil {
		return l, refuseErr(StagePaths, fmt.Errorf("working directory: %w", err))
	}
	if !l.wd.isDir() {
		return l, refuse(StagePaths, "working directory %s is not a directory", l.wd.canonical)
	}

	inst, err := identifyExecutable(m, in.BinaryPath, callerEnv)
	if err != nil {
		return l, err
	}
	l.pins = append(l.pins, inst.exe)
	l.pins = append(l.pins, inst.grants...)
	l.path = inst.path
	l.argv = append([]string{in.BinaryPath}, in.Args...)

	// Managed state: the given (outliving) state, or fresh ephemeral state.
	if in.State != nil {
		l.state = in.State
		if err = l.state.lock(); err != nil {
			return l, refuseErr(StageState, err)
		}
		if err = l.state.recoverPrevious(context.Background()); err != nil {
			return l, refuseErr(StageState, err)
		}
	} else {
		sweepStale()
		if l.state, err = NewState(false); err != nil {
			return l, refuseErr(StageState, err)
		}
		l.ownsState = true
	}

	lay, statePins, stateDir, err := l.provisionState(m, req, callerEnv)
	if err != nil {
		return l, err
	}

	grants, omitted, err := l.collectGrants(m, req, inst, statePins, stateDir)
	if err != nil {
		return l, err
	}

	if err = checkTargets(in.ExpectTargets, append([]*pinned{l.wd, stateDir}, l.callerPins...)); err != nil {
		return l, err
	}
	if sockets != nil {
		var writable []*pinned
		for _, g := range grants {
			if g.class == "rw" {
				writable = append(writable, g.pin)
			}
		}
		if err = checkSocketRoots(sockets, writable); err != nil {
			return l, err
		}
	}

	env, names := childEnv(m, req, callerEnv, lay, l.wd.canonical)
	l.env = env

	// Supervision: created and recorded before the harness starts, so a crash
	// between spawn and teardown still leaves the next owner a cgroup to kill.
	sup := containment.Supervision{Mode: supervisionNone}
	cg, reason, err := newSessionCgroup(l.state.ID)
	if err != nil {
		return l, refuseErr(StageSupervision, err)
	}
	if cg == nil {
		if in.RequireSupervision || l.state.Persistent {
			return l, refuse(StageSupervision, "cgroup supervision is required (%s) but unavailable: %s",
				map[bool]string{true: "stored conversation", false: "requested"}[l.state.Persistent], reason)
		}
		sup.Reason = reason
	} else {
		l.cg = cg
		sup = containment.Supervision{Mode: supervisionCgroup, Cgroup: cg.path}
	}
	if err = l.state.recordLaunch(&launchRecord{
		Supervision: sup.Mode, Cgroup: sup.Cgroup, BootID: bootID(), StartedAt: time.Now().UTC(),
	}); err != nil {
		return l, refuseErr(StageState, err)
	}

	if l.ruleset, err = landlock.New(landlock.Config{MinABI: req.MinABI, RestrictTCP: req.RestrictTCP}); err != nil {
		if errors.Is(err, landlock.ErrUnavailable) {
			return l, refuseErr(StageKernel, err)
		}
		return l, refuseErr(StageRuleset, err)
	}
	for _, g := range grants {
		if err = l.ruleset.AddPath(landlock.Rule{FD: g.pin.fd, Access: g.access, IsDir: g.pin.isDir()}); err != nil {
			return l, refuseErr(StageRuleset, fmt.Errorf("%s: %w", g.pin.canonical, err))
		}
	}
	for _, port := range req.ConnectTCP {
		if err = l.ruleset.AddConnectTCP(port); err != nil {
			return l, refuseErr(StageRuleset, err)
		}
	}

	l.applied = containment.Applied{
		SchemaVersion:   containment.SchemaVersion,
		Kind:            req.Kind,
		ABI:             l.ruleset.ABI(),
		RequiredABI:     req.MinABI,
		Profile:         m.id(),
		ProfileVersion:  m.ManifestVersion,
		HandledFS:       l.ruleset.HandledFS().Names(),
		PathnameSockets: containment.PathnameSocketsDenied,
		Scopes:          landlock.Scopes().Names(),
		State: containment.State{
			Mode:            map[bool]string{true: "caller", false: "private"}[req.StateDir != ""],
			ID:              l.state.ID,
			Home:            lay.Home,
			Tmp:             lay.Tmp,
			HarnessState:    lay.Config,
			HarnessStateEnv: m.State.ConfigEnv,
		},
		Supervision: sup,
		Env:         names,
		Omitted:     omitted,
	}
	if sockets != nil {
		l.applied.PathnameSockets = containment.PathnameSocketsDeniedOutsideRoots
		l.applied.AppArmor = &containment.AppArmorLayer{Profile: apparmor.ProfileName, Roots: slices.Clone(sockets.Roots)}
	}
	if req.RestrictTCP {
		l.applied.TCP = containment.TCP{Mode: "restricted", Connect: req.ConnectTCP, Bind: "denied"}
	} else {
		l.applied.TCP = containment.TCP{Mode: "unrestricted", Bind: "unrestricted"}
	}
	for _, g := range grants {
		rule := landlock.Rule{Access: g.access, IsDir: g.pin.isDir()}
		ag := containment.Grant{Path: g.pin.canonical, Access: g.class, Rights: (rule.Effective() & l.ruleset.HandledFS()).Names(), Source: g.source}
		if g.pin.requested != g.pin.canonical {
			ag.Requested = g.pin.requested
		}
		l.applied.Grants = append(l.applied.Grants, ag)
	}
	if stateDir != nil {
		l.applied.State.StateDir = stateDir.canonical
		if stateDir.requested != stateDir.canonical {
			l.applied.State.StateDirRequested = stateDir.requested
		}
	}
	l.applied.Fingerprint = containment.Fingerprint(&l.applied)
	return l, nil
}

// checkTargets refuses a resumed launch whose paths now resolve to other
// objects than the ones recorded when its conversation was created.
func checkTargets(expect map[string]string, pins []*pinned) error {
	if len(expect) == 0 {
		return nil
	}
	for _, p := range pins {
		if p == nil {
			continue
		}
		if want, ok := expect[p.requested]; ok && want != p.canonical {
			return refuse(StagePaths,
				"%s now resolves to %s, but this conversation was created with it resolving to %s; restore the path or start a new conversation",
				p.requested, p.canonical, want)
		}
	}
	return nil
}

// checkHarnessMode refuses harness modes the profile cannot contain. Codex's
// own Linux sandbox (bubblewrap) cannot run inside a Landlock domain — it
// needs user-namespace ID maps and mounts the domain denies — so a contained
// codex runs only at the rung that bypasses it, where the domain is the only
// boundary. Other modes are refused here, never rewritten.
func checkHarnessMode(m *manifest, in Input) error {
	if m.Name != "codex" {
		return nil
	}
	if in.LaunchRung != "bypass" {
		rung := in.LaunchRung
		if rung == "" {
			rung = "the harness default"
		}
		return refuse(StageProfile,
			"a contained codex runs only at the bypass rung (--permission-mode bypass or danger-full-access, or --dangerously-bypass-approvals-and-sandbox): codex's own sandbox cannot run inside a Landlock domain, and %s would start codex and then fail every tool call", rung)
	}
	return nil
}

// checkLogin admits a login launch: the profile's own login or status command
// and nothing else, into a caller StateDir. Private state would be deleted,
// login and all, when the launch ends, and managed state belongs to a stored
// conversation.
func checkLogin(m *manifest, req *containment.Request, in Input) error {
	if req.StateDir == "" {
		return refuse(StageState, "a login needs a StateDir to keep the login in: private state is deleted when the launch ends")
	}
	if in.State != nil {
		return refuse(StageState, "a login keeps its login in the request's StateDir, not in managed state")
	}
	if !slices.Equal(in.Args, m.Login.Args) && !slices.Equal(in.Args, m.Login.Status) {
		return refuse(StageRequest, "a login launch runs only the %s profile's login command %q or its status command %q, not %q",
			m.Name, m.Login.Args, m.Login.Status, in.Args)
	}
	if req.RestrictTCP && m.Login.TCPBind != "" && slices.Equal(in.Args, m.Login.Args) {
		return refuse(StageProfile,
			"the %s login (%s) binds a TCP listener (%s), which restricted TCP denies; sign in with TCP unrestricted, and restrict it for the sessions that use this StateDir",
			m.Name, m.HarnessVersion, m.Login.TCPBind)
	}
	return nil
}

// provisionState lays out HOME, TMPDIR and the harness's own state root,
// writes the seeds, and returns the pinned directories to grant. Every
// directory is created through the pinned parent, never re-resolved by name.
func (l *Launch) provisionState(m *manifest, req *containment.Request, callerEnv []string) (layout, []*pinned, *pinned, error) {
	var lay layout
	var pins []*pinned
	keep := func(p *pinned) *pinned {
		l.pins = append(l.pins, p)
		pins = append(pins, p)
		return p
	}

	tmp, err := ensureDir(l.state.root, "tmp")
	if err != nil {
		return lay, nil, nil, refuseErr(StageState, err)
	}
	keep(tmp)
	lay.Tmp = tmp.canonical
	if m.MaxTmpdirBytes > 0 && len(lay.Tmp) > m.MaxTmpdirBytes {
		return lay, nil, nil, refuse(StageState,
			"the private TMPDIR %s is %d bytes, over %s's limit of %d (%s); point XDG_STATE_HOME at a shorter directory",
			lay.Tmp, len(lay.Tmp), m.Name, m.MaxTmpdirBytes, "a longer socket path makes it share /tmp with every same-user session")
	}

	base := l.state.root
	var stateDir *pinned
	if req.StateDir != "" {
		if stateDir, err = pinPath(req.StateDir); err != nil {
			return lay, nil, nil, refuseErr(StageState, fmt.Errorf("StateDir: %w", err))
		}
		l.pins = append(l.pins, stateDir)
		if !stateDir.isDir() {
			return lay, nil, nil, refuse(StageState, "StateDir %s is not a directory", stateDir.canonical)
		}
		base = stateDir
	}
	home, err := ensureDir(base, "home")
	if err != nil {
		return lay, nil, nil, refuseErr(StageState, err)
	}
	keep(home)
	lay.Home = home.canonical

	var config *pinned
	switch {
	case m.State.ConfigDir == "":
	case m.State.ConfigUnderHome:
		// Beneath the granted HOME: no rule of its own.
		if config, err = ensureDir(home, m.State.ConfigDir); err != nil {
			return lay, nil, nil, refuseErr(StageState, err)
		}
		l.pins = append(l.pins, config)
	default:
		// A sibling of HOME and TMPDIR, never beneath TMPDIR: codex refuses
		// to create its helper links under TMPDIR.
		if config, err = ensureDir(base, m.State.ConfigDir); err != nil {
			return lay, nil, nil, refuseErr(StageState, err)
		}
		keep(config)
	}
	if config != nil {
		lay.Config = config.canonical
	}

	switch m.Name {
	case "claude-code":
		apiKey, _ := lookupEnv(callerEnv, "ANTHROPIC_API_KEY")
		seed, err := claudeSeed(l.wd.canonical, apiKey)
		if err == nil {
			err = seedFile(config, ".claude.json", seed)
		}
		if err != nil {
			return lay, nil, nil, refuseErr(StageState, err)
		}
	case "codex":
		seed, err := codexConfig(l.wd.canonical)
		if err == nil {
			err = seedFile(config, "config.toml", seed)
		}
		if err == nil {
			if key, ok := lookupEnv(callerEnv, "CODEX_API_KEY"); ok && key != "" {
				var auth []byte
				if auth, err = codexAuth(key); err == nil {
					err = seedFile(config, "auth.json", auth)
				}
			}
		}
		if err != nil {
			return lay, nil, nil, refuseErr(StageState, err)
		}
	}
	return lay, pins, stateDir, nil
}

// collectGrants pins the baseline and profile paths, adds the executable,
// working-directory, state and caller grants, and applies the checks that
// keep a request from claiming more (or less) than it gets.
func (l *Launch) collectGrants(m *manifest, req *containment.Request, inst *install, statePins []*pinned, stateDir *pinned) ([]grantEntry, []string, error) {
	var grants []grantEntry
	var omitted []string
	add := func(p *pinned, class, source string) error {
		access, err := classAccess(class)
		if err != nil {
			return refuseErr(StagePaths, err)
		}
		grants = append(grants, grantEntry{pin: p, class: class, access: access, source: source})
		return nil
	}

	specs := append(append([]pathSpec{}, baseline.Paths...), m.Paths...)
	oneOf := map[string]bool{}
	for _, spec := range specs {
		if spec.OneOf != "" {
			if _, seen := oneOf[spec.OneOf]; !seen {
				oneOf[spec.OneOf] = false
			}
		}
		if spec.MergedUsr {
			if fi, err := os.Lstat(spec.Path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				omitted = append(omitted, spec.Path+" (merged into /usr)")
				continue
			}
		}
		p, err := pinPath(spec.Path)
		if err != nil {
			if errors.Is(err, errNotFound) && !spec.Required {
				omitted = append(omitted, spec.Path)
				continue
			}
			return nil, nil, refuseErr(StagePaths, fmt.Errorf("profile path %s (%s): %w", spec.Path, spec.Reason, err))
		}
		l.pins = append(l.pins, p)
		source := containment.SourceProfile
		if spec.Access == "dev" || spec.Access == "null" || spec.Access == "read" {
			source = containment.SourceDevice
		}
		if err := add(p, spec.Access, source); err != nil {
			return nil, nil, err
		}
		if spec.OneOf != "" {
			oneOf[spec.OneOf] = true
		}
	}
	for group, found := range oneOf {
		if !found {
			return nil, nil, refuse(StagePaths, "none of the %s paths the profile needs exists on this host", group)
		}
	}

	if err := add(inst.exe, "rx", containment.SourceProfile); err != nil {
		return nil, nil, err
	}
	for _, g := range inst.grants {
		if err := add(g, "rx", containment.SourceProfile); err != nil {
			return nil, nil, err
		}
	}
	if err := add(l.wd, "rw", containment.SourceWorkingDir); err != nil {
		return nil, nil, err
	}
	for _, p := range statePins {
		if err := add(p, "rw", containment.SourceState); err != nil {
			return nil, nil, err
		}
	}

	var callerRO, callerRW []*pinned
	for _, path := range req.ReadOnly {
		p, err := pinPath(path)
		if err != nil {
			return nil, nil, refuseErr(StagePaths, fmt.Errorf("read-only grant: %w", err))
		}
		l.pins = append(l.pins, p)
		l.callerPins = append(l.callerPins, p)
		callerRO = append(callerRO, p)
	}
	for _, path := range req.ReadWrite {
		p, err := pinPath(path)
		if err != nil {
			return nil, nil, refuseErr(StagePaths, fmt.Errorf("read-write grant: %w", err))
		}
		l.pins = append(l.pins, p)
		l.callerPins = append(l.callerPins, p)
		callerRW = append(callerRW, p)
	}

	// Nothing the caller controls may expose the managed-state parent, which
	// holds every other session's private state, or lie inside it.
	exposed := append(append([]*pinned{l.wd}, callerRO...), callerRW...)
	if stateDir != nil {
		exposed = append(exposed, stateDir)
	}
	for _, p := range exposed {
		if p.overlaps(l.state.parent) {
			return nil, nil, refuse(StagePaths, "%s overlaps the managed-state directory %s, which holds other sessions' private state", p.canonical, l.state.parent.canonical)
		}
	}

	// Rules in one layer only add: a read-only child cannot subtract a
	// writable ancestor's grant, so a read-only declaration inside a writable
	// grant would claim a protection the kernel does not give.
	writable := append(append([]*pinned{l.wd}, statePins...), callerRW...)
	for _, ro := range callerRO {
		for _, w := range writable {
			if w.contains(ro) {
				return nil, nil, refuse(StagePaths, "read-only grant %s lies inside the writable grant %s, so it would not be read-only", ro.canonical, w.canonical)
			}
		}
	}

	// No writable grant may expose cgroupfs: a descendant could move itself
	// into the wrapper's cgroup and outlive the session.
	cgroupfs, cgErr := pinPath(cgroupRoot)
	if cgErr == nil {
		defer cgroupfs.close()
	}
	for _, w := range append([]*pinned{l.wd}, callerRW...) {
		if w.fsType == unix.CGROUP2_SUPER_MAGIC || w.fsType == unix.CGROUP_SUPER_MAGIC || (cgErr == nil && w.contains(cgroupfs)) {
			return nil, nil, refuse(StagePaths, "writable grant %s exposes cgroupfs", w.canonical)
		}
	}

	for _, p := range callerRO {
		if err := add(p, "ro", containment.SourceCaller); err != nil {
			return nil, nil, err
		}
	}
	for _, p := range callerRW {
		if err := add(p, "rw", containment.SourceCaller); err != nil {
			return nil, nil, err
		}
	}
	return grants, omitted, nil
}

// AddTerminal grants the session's PTY slave (read, write, device ioctls) so
// the harness can reopen its own terminal by name. Only this terminal: never
// all of /dev/pts. Call it after Prepare and before Start.
func (l *Launch) AddTerminal(slave int) error {
	p, err := pinFD(slave, "session terminal")
	if err != nil {
		return refuseErr(StageRuleset, err)
	}
	l.pins = append(l.pins, p)
	rule := landlock.Rule{FD: p.fd, Access: landlock.Device, IsDir: false}
	if err := l.ruleset.AddPath(rule); err != nil {
		return refuseErr(StageRuleset, fmt.Errorf("session terminal %s: %w", p.canonical, err))
	}
	l.applied.Grants = append(l.applied.Grants, containment.Grant{
		Path: p.canonical, Access: "dev", Rights: rule.Effective().Names(), Source: containment.SourceTerminal,
	})
	l.applied.Fingerprint = containment.Fingerprint(&l.applied)
	return nil
}

// Applied returns a copy of the effective policy.
func (l *Launch) Applied() *containment.Applied { return l.applied.Clone() }

// Supervised reports whether a session cgroup contains the launch.
func (l *Launch) Supervised() bool { return l.cg != nil }

// Start spawns the harness on a dedicated locked thread with slave as its
// terminal. A failure before exec (the domain could not be enforced) is a
// refusal; a failure of exec itself is returned as the errno, for the caller
// to classify. Either way the parent's copies of the pinned descriptors and
// the ruleset are released: the child, if any, holds only its terminal.
func (l *Launch) Start(slave int) (int, error) {
	cgfd := -1
	if l.cg != nil {
		cgfd = l.cg.fd
	}
	r := spawn(spawnSpec{
		path:     l.path,
		argv:     l.argv,
		env:      l.env,
		dir:      "/proc/self/fd/" + strconv.Itoa(l.wd.fd),
		tty:      slave,
		cgroupFD: cgfd,
		ruleset:  l.ruleset,

		apparmorStack: l.sockets != nil,
	})
	l.releasePins()
	// CLONE_INTO_CGROUP was the descriptor's only use; Finish and recovery
	// work from the path.
	l.cg.closeFD()
	if r.err != nil {
		if r.stage == "fork_exec" {
			return 0, r.err
		}
		return 0, refuseErr(StageLaunch, fmt.Errorf("%s: %w", r.stage, r.err))
	}
	if l.sockets != nil {
		// The stack was requested before exec, which fails when it cannot be
		// applied; this confirms the running harness carries it.
		if err := apparmor.CheckConfined(r.pid); err != nil {
			_ = syscall.Kill(r.pid, syscall.SIGKILL)
			if l.cg != nil {
				_ = l.cg.kill()
			}
			var ws syscall.WaitStatus
			_, _ = syscall.Wait4(r.pid, &ws, 0, nil)
			return 0, refuseErr(StageLaunch, err)
		}
	}
	l.started = true
	if rec, err := l.state.readLifecycle(); err == nil && rec.Launch != nil {
		rec.Launch.PID = r.pid
		_ = l.state.writeLifecycle(rec)
	}
	return r.pid, nil
}

func (l *Launch) releasePins() {
	for _, p := range l.pins {
		p.close()
	}
	l.pins = nil
	l.wd.close()
	if l.ruleset != nil {
		_ = l.ruleset.Close()
		l.ruleset = nil
	}
}

// Release undoes a prepared launch that will not start (or failed to):
// descriptors closed, the empty session cgroup removed, ephemeral state
// deleted. Idempotent.
func (l *Launch) Release() {
	if l == nil {
		return
	}
	l.releasePins()
	if l.cg != nil {
		l.cg.closeFD()
		if !l.started {
			_ = removeCgroup(l.cg.path)
		}
	}
	if l.state != nil && !l.started {
		l.state.endLaunch(cleanupComplete)
		if l.ownsState {
			_ = l.state.removeTree()
			l.state.Close()
		} else {
			l.state.unlock()
		}
		l.state = nil
	}
}

// Finish ends the session's process tree after the harness leader has been
// reaped and releases its private resources. Under supervision it SIGTERMs
// the harness's process group (unless termination already did), waits until
// the cgroup is empty or deadline passes, SIGKILLs everything left with
// cgroup.kill, waits for "populated 0", removes the cgroup, and deletes
// ephemeral state. Without supervision nothing can prove the tree gone, so the
// state is kept for the caller to remove and the cleanup is reported
// incomplete. It returns the cleanup outcome recorded in the applied policy.
func (l *Launch) Finish(pgid int, termSent bool, deadline time.Time) string {
	if l.cg == nil {
		cleanup := "incomplete: no cgroup supervision; private state kept at " + l.state.Root()
		l.state.endLaunch(cleanup)
		l.state.unlock()
		return cleanup
	}
	defer l.cg.closeFD()
	if pop, err := populated(l.cg.path); err == nil && pop {
		if !termSent && pgid > 1 {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		}
		if wait := time.Until(deadline); wait > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), wait)
			_ = waitEmpty(ctx, l.cg.path)
			cancel()
		}
	}
	cleanup := cleanupComplete
	if err := l.cg.kill(); err != nil {
		cleanup = "incomplete: cgroup.kill: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), emptyBudget)
	err := waitEmpty(ctx, l.cg.path)
	cancel()
	switch {
	case err != nil:
		cleanup = "incomplete: " + err.Error() + "; private state kept at " + l.state.Root()
	default:
		if rerr := removeCgroup(l.cg.path); rerr != nil {
			cleanup = "incomplete: remove cgroup: " + rerr.Error()
		}
	}
	l.state.endLaunch(cleanup)
	if cleanup == cleanupComplete && l.ownsState {
		if rerr := l.state.removeTree(); rerr != nil {
			cleanup = "incomplete: " + rerr.Error()
		}
		l.state.Close()
	} else {
		l.state.unlock()
	}
	return cleanup
}

// sweepStale removes ephemeral state whose launch is gone: its lock is free
// (locks die with their holder) and its recorded cgroup, if any, can be
// recovered. State whose launch ran unsupervised is kept, as reported when it
// ran. Persistent state belongs to its stored conversation and is skipped.
func sweepStale() {
	parent, err := openStateParent()
	if err != nil {
		return
	}
	defer parent.close()
	entries, err := os.ReadDir(parent.canonical)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		// Leave young state alone: its creator may be between mkdir and
		// taking the lock.
		if fi, err := e.Info(); err != nil || time.Since(fi.ModTime()) < time.Minute {
			continue
		}
		root, err := pinAt(parent, e.Name())
		if err != nil {
			continue
		}
		s := &State{ID: e.Name(), parent: parent, root: root, lockFD: -1}
		if s.lock() != nil {
			root.close() // a live launch holds it
			continue
		}
		rec, err := s.readLifecycle()
		if err == nil && !rec.Persistent && s.recoverPrevious(context.Background()) == nil {
			_ = s.removeTree()
		}
		s.unlock()
		root.close()
	}
}
