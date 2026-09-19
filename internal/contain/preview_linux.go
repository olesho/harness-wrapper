//go:build linux

package contain

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/olesho/harness-wrapper/internal/apparmor"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/containment"
	"golang.org/x/sys/unix"
)

// Placeholders for directories a preview cannot allocate.
const (
	placeholderHome     = "$STATE/home"
	placeholderTmp      = "$STATE/tmp"
	placeholderTerminal = "$TERMINAL"
)

// PreviewLaunch runs the checks Prepare runs, collecting every problem
// instead of stopping at the first, and allocates nothing: no state, no
// cgroup, no ruleset. Only an invalid request is an error.
func PreviewLaunch(in Input) (*Preview, error) {
	req, err := containment.Normalize(in.Request)
	if err != nil {
		return nil, refuseErr(StageRequest, err)
	}
	if req == nil {
		return nil, refuse(StageRequest, "no containment requested")
	}
	p := &Preview{Preview: true}
	missing := func(format string, args ...any) { p.Missing = append(p.Missing, fmt.Sprintf(format, args...)) }
	var pins []*pinned
	defer func() {
		for _, x := range pins {
			x.close()
		}
	}()
	pin := func(path string) (*pinned, error) {
		x, err := pinPath(path)
		if err == nil {
			pins = append(pins, x)
		}
		return x, err
	}

	m, err := profileFor(in.Harness, false)
	if err != nil {
		missing("%v", err)
		if m = inactiveProfile(in.Harness); m == nil {
			p.WouldLaunch = false
			return p, nil
		}
	}
	if err := checkHarnessMode(m, in); err != nil {
		missing("%v", err)
	}
	abi, err := landlock.ABI()
	p.KernelABI = abi
	handled := landlock.HandledFS
	var sockets *apparmor.Layer
	switch {
	case err != nil:
		p.Kernel = err.Error()
		missing("kernel: %v", err)
	case abi < req.MinABI:
		p.Kernel = fmt.Sprintf("Landlock ABI %d is below the required %d", abi, req.MinABI)
		missing("kernel: %s", p.Kernel)
	case abi < landlock.ResolveUnixABI:
		handled = landlock.HandledFSFor(abi)
		if sockets, err = socketLayer(abi); err != nil {
			p.Kernel = err.Error()
			missing("kernel: %v", err)
		}
	}

	callerEnv := in.Env
	if callerEnv == nil {
		callerEnv = os.Environ()
	}
	planned := &containment.Applied{
		SchemaVersion:   containment.SchemaVersion,
		Kind:            req.Kind,
		ABI:             abi,
		RequiredABI:     req.MinABI,
		Profile:         m.id(),
		ProfileVersion:  m.ManifestVersion,
		HandledFS:       handled.Names(),
		PathnameSockets: containment.PathnameSocketsDenied,
		Scopes:          landlock.Scopes().Names(),
	}
	if sockets != nil {
		planned.PathnameSockets = containment.PathnameSocketsDeniedOutsideRoots
		planned.AppArmor = &containment.AppArmorLayer{Profile: apparmor.ProfileName, Roots: slices.Clone(sockets.Roots)}
	}
	grant := func(path, requested, class, source string, isDir bool) {
		access, _ := classAccess(class)
		rule := landlock.Rule{Access: access, IsDir: isDir}
		g := containment.Grant{Path: path, Access: class, Rights: (rule.Effective() & handled).Names(), Source: source}
		if requested != "" && requested != path {
			g.Requested = requested
		}
		planned.Grants = append(planned.Grants, g)
	}

	// Baseline and profile paths, in Prepare's order.
	oneOf := map[string]bool{}
	for _, spec := range append(append([]pathSpec{}, baseline.Paths...), m.Paths...) {
		if spec.OneOf != "" {
			if _, seen := oneOf[spec.OneOf]; !seen {
				oneOf[spec.OneOf] = false
			}
		}
		if spec.MergedUsr {
			if fi, err := os.Lstat(spec.Path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				p.Omitted = append(p.Omitted, spec.Path+" (merged into /usr)")
				continue
			}
		}
		x, err := pin(spec.Path)
		if err != nil {
			if errors.Is(err, errNotFound) && !spec.Required {
				p.Omitted = append(p.Omitted, spec.Path)
			} else {
				missing("profile path %s (%s): %v", spec.Path, spec.Reason, err)
			}
			continue
		}
		source := containment.SourceProfile
		if spec.Access == "dev" || spec.Access == "null" || spec.Access == "read" {
			source = containment.SourceDevice
		}
		grant(x.canonical, x.requested, spec.Access, source, x.isDir())
		if spec.OneOf != "" {
			oneOf[spec.OneOf] = true
		}
	}
	for group, found := range oneOf {
		if !found {
			missing("none of the %s paths the profile needs exists on this host", group)
		}
	}

	if inst, err := identifyExecutable(m, in.BinaryPath, callerEnv); err != nil {
		missing("executable: %v", err)
	} else {
		pins = append(pins, inst.exe)
		pins = append(pins, inst.grants...)
		grant(inst.exe.canonical, inst.exe.requested, "rx", containment.SourceProfile, false)
		for _, g := range inst.grants {
			grant(g.canonical, g.requested, "rx", containment.SourceProfile, g.isDir())
		}
	}

	wdPath := in.WorkingDir
	if wdPath == "" {
		wdPath, _ = os.Getwd()
	}
	wd, err := pin(wdPath)
	switch {
	case err != nil:
		missing("working directory: %v", err)
		wd = nil
	case !wd.isDir():
		missing("working directory %s is not a directory", wd.canonical)
	default:
		grant(wd.canonical, wd.requested, "rw", containment.SourceWorkingDir, true)
	}

	// State, with placeholders for what a launch would allocate.
	lay := layout{Home: placeholderHome, Tmp: placeholderTmp}
	planned.State = containment.State{Mode: "private", Home: lay.Home, Tmp: lay.Tmp, HarnessStateEnv: m.State.ConfigEnv}
	var stateDir *pinned
	if req.StateDir != "" {
		planned.State.Mode = "caller"
		if stateDir, err = pin(req.StateDir); err != nil {
			missing("StateDir: %v", err)
		} else {
			lay.Home = filepath.Join(stateDir.canonical, "home")
			planned.State.Home = lay.Home
			planned.State.StateDir = stateDir.canonical
		}
	}
	switch {
	case m.State.ConfigDir == "":
	case m.State.ConfigUnderHome:
		lay.Config = lay.Home + "/" + m.State.ConfigDir
	case stateDir != nil:
		lay.Config = filepath.Join(stateDir.canonical, m.State.ConfigDir)
	default:
		lay.Config = "$STATE/" + m.State.ConfigDir
	}
	planned.State.HarnessState = lay.Config
	if m.MaxTmpdirBytes > 0 {
		if parent, err := StateParent(); err == nil && len(parent)+len("/xxxxxxxxxxxx/tmp") > m.MaxTmpdirBytes {
			missing("the private TMPDIR beneath %s would exceed %s's %d-byte limit; point XDG_STATE_HOME at a shorter directory", parent, m.Name, m.MaxTmpdirBytes)
		}
	}
	grant(lay.Tmp, "", "rw", containment.SourceState, true)
	grant(lay.Home, "", "rw", containment.SourceState, true)
	if lay.Config != "" && !m.State.ConfigUnderHome {
		grant(lay.Config, "", "rw", containment.SourceState, true)
	}

	var callerRO, callerRW []*pinned
	for _, path := range req.ReadOnly {
		if x, err := pin(path); err != nil {
			missing("read-only grant: %v", err)
		} else {
			callerRO = append(callerRO, x)
		}
	}
	for _, path := range req.ReadWrite {
		if x, err := pin(path); err != nil {
			missing("read-write grant: %v", err)
		} else {
			callerRW = append(callerRW, x)
		}
	}
	exposed := append(append([]*pinned{}, callerRO...), callerRW...)
	if wd != nil {
		exposed = append(exposed, wd)
	}
	if stateDir != nil {
		exposed = append(exposed, stateDir)
	}
	if parentPath, err := StateParent(); err == nil {
		// The parent may not exist yet; then nothing can expose it but its
		// own ancestors, checked through the nearest existing one.
		for probe := parentPath; probe != "/" && probe != "."; probe = filepath.Dir(probe) {
			parent, err := pin(probe)
			if err != nil {
				continue
			}
			for _, x := range exposed {
				if x.contains(parent) || (probe == parentPath && parent.contains(x)) {
					missing("%s overlaps the managed-state directory %s, which holds other sessions' private state", x.canonical, parentPath)
				}
			}
			break
		}
	}
	writable := append([]*pinned{}, callerRW...)
	if wd != nil {
		writable = append(writable, wd)
	}
	for _, ro := range callerRO {
		for _, w := range writable {
			if w.contains(ro) {
				missing("read-only grant %s lies inside the writable grant %s, so it would not be read-only", ro.canonical, w.canonical)
			}
		}
	}
	if sockets != nil {
		// Private TMPDIR (and, without a StateDir, HOME and harness state)
		// live beneath the managed-state parent, which may not exist yet.
		stateRW := append([]*pinned{}, writable...)
		if stateDir != nil {
			stateRW = append(stateRW, stateDir)
		}
		for _, w := range stateRW {
			if err := checkSocketRoots(sockets, []*pinned{w}); err != nil {
				missing("%v", err)
			}
		}
		parent, err := StateParent()
		if resolved, rerr := filepath.EvalSymlinks(parent); rerr == nil {
			parent = resolved
		}
		if err == nil && !sockets.Covers(parent) {
			missing("the managed-state directory %s, which holds the private TMPDIR, lies outside the AppArmor socket layer's roots %v, where %s denies writes; add a root that covers it and reload the profile",
				parent, sockets.Roots, apparmor.ProfileName)
		}
	}
	cgroupfs, cgErr := pin(cgroupRoot)
	for _, w := range writable {
		if w.fsType == unix.CGROUP2_SUPER_MAGIC || w.fsType == unix.CGROUP_SUPER_MAGIC || (cgErr == nil && w.contains(cgroupfs)) {
			missing("writable grant %s exposes cgroupfs", w.canonical)
		}
	}
	for _, x := range callerRO {
		grant(x.canonical, x.requested, "ro", containment.SourceCaller, x.isDir())
	}
	for _, x := range callerRW {
		grant(x.canonical, x.requested, "rw", containment.SourceCaller, x.isDir())
	}
	grant(placeholderTerminal, "", "dev", containment.SourceTerminal, false)

	if req.RestrictTCP {
		planned.TCP = containment.TCP{Mode: "restricted", Connect: req.ConnectTCP, Bind: "denied"}
	} else {
		planned.TCP = containment.TCP{Mode: "unrestricted", Bind: "unrestricted"}
	}
	wdName := placeholderHome
	if wd != nil {
		wdName = wd.canonical
	}
	_, planned.Env = childEnv(m, req, callerEnv, lay, wdName)

	if own, reason := probeSupervisionFn(); own == "" {
		p.Supervision, p.SupervisionReason = supervisionNone, reason
		if in.RequireSupervision {
			missing("cgroup supervision is required but unavailable: %s", reason)
		}
	} else {
		p.Supervision = supervisionCgroup
	}
	if wd != nil {
		p.Notes = append(p.Notes, gitMetadataNotes(wd.canonical, append(append([]*pinned{wd}, callerRO...), callerRW...))...)
	}
	planned.Supervision = containment.Supervision{Mode: p.Supervision, Reason: p.SupervisionReason}
	planned.Omitted = p.Omitted
	planned.Fingerprint = containment.Fingerprint(planned)
	p.Planned = planned
	p.WouldLaunch = len(p.Missing) == 0
	return p, nil
}

// inactiveProfile returns the built-in profile for harness even when it is not
// activated, so a preview can still plan it (the refusal is listed).
func inactiveProfile(harness string) *manifest {
	if loadManifests() != nil {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(harness))
	for _, m := range builtins {
		for _, h := range m.Harnesses {
			if h == name {
				return m
			}
		}
	}
	return nil
}
