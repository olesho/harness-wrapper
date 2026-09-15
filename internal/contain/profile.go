package contain

import (
	"embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

//go:embed profiles/*.json
var manifestFS embed.FS

// pathSpec is one path of a manifest.
type pathSpec struct {
	Path      string `json:"path"`
	Access    string `json:"access"`
	Required  bool   `json:"required"`
	MergedUsr bool   `json:"merged_usr,omitempty"`
	OneOf     string `json:"one_of,omitempty"`
	Reason    string `json:"reason"`
}

type platformBinary struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type platformPackage struct {
	Alias  string `json:"alias"`
	Triple string `json:"triple"`
}

type executableSpec struct {
	// Kind is "native" (one ELF identified by checksum), "npm-shim" (a node
	// script in an npm package with a vendored native binary) or "test".
	Kind             string                     `json:"kind"`
	Platforms        map[string]platformBinary  `json:"platforms,omitempty"`
	Package          string                     `json:"package,omitempty"`
	PlatformPackages map[string]platformPackage `json:"platform_packages,omitempty"`
	Interpreter      string                     `json:"interpreter,omitempty"`
}

type stateSpec struct {
	// ConfigEnv names the variable pointing the harness at its state root.
	ConfigEnv string `json:"config_env"`
	// ConfigDir is the state root, relative to HOME when ConfigUnderHome,
	// else a sibling of HOME and TMPDIR in the session root (CODEX_HOME must
	// not lie beneath TMPDIR).
	ConfigDir       string `json:"config_dir"`
	ConfigUnderHome bool   `json:"config_under_home"`
}

// loginSpec is how a person signs the pinned harness version in: the
// arguments of its own login command and of the command that reports whether
// it is signed in, and what the wrapper reads from their output. The patterns
// are Go regular expressions over the output with escape sequences removed,
// where an OSC 8 hyperlink's target counts as text.
type loginSpec struct {
	Args   []string `json:"args"`
	Status []string `json:"status"`
	// URL matches the address of the sign-in page.
	URL string `json:"url"`
	// UserCode, when set, matches the one-time code the person enters on
	// that page; its first group, if any, is the code.
	UserCode string `json:"user_code,omitempty"`
	// CodePrompt, when set, is the prompt at which the harness waits for the
	// code the page shows once the person has signed in.
	CodePrompt string `json:"code_prompt,omitempty"`
	// Success is printed once the login is stored.
	Success string `json:"success"`
	// LoggedIn matches the status command's output when signed in.
	LoggedIn string `json:"logged_in"`
	// TCPBind, when set, says why the login command binds a TCP listener,
	// which a restricted-TCP domain denies: such a login is refused under
	// RestrictTCP rather than left to fail inside the harness.
	TCPBind string `json:"tcp_bind,omitempty"`
	Reason  string `json:"reason"`
}

// manifest is a versioned harness profile.
type manifest struct {
	Name            string            `json:"name"`
	Harnesses       []string          `json:"harnesses"`
	ManifestVersion int               `json:"manifest_version"`
	HarnessVersion  string            `json:"harness_version"`
	Activated       bool              `json:"activated"`
	Activation      string            `json:"activation"`
	Executable      executableSpec    `json:"executable"`
	Paths           []pathSpec        `json:"paths"`
	State           stateSpec         `json:"state"`
	Env             map[string]string `json:"env"`
	EnvReasons      map[string]string `json:"env_reasons"`
	AuthEnv         []string          `json:"auth_env"`
	ControlEnv      []string          `json:"control_env"`
	MaxTmpdirBytes  int               `json:"max_tmpdir_bytes"`
	Login           *loginSpec        `json:"login,omitempty"`

	// testDirs are extra read/execute trees of a test profile.
	testDirs []string
}

// id is the profile's name as recorded in an applied policy.
func (m *manifest) id() string { return m.Name + "@" + m.HarnessVersion }

type baselineManifest struct {
	ManifestVersion int        `json:"manifest_version"`
	Paths           []pathSpec `json:"paths"`
}

var (
	loadOnce  sync.Once
	baseline  baselineManifest
	builtins  []*manifest
	loadError error

	testMu       sync.Mutex
	testProfiles []*manifest

	// activateAll lets the module's own conformance and integration tests
	// launch profiles whose authenticated conformance has not passed yet.
	activateAll atomic.Bool
)

func loadManifests() error {
	loadOnce.Do(func() {
		b, err := manifestFS.ReadFile("profiles/baseline.json")
		if err != nil {
			loadError = err
			return
		}
		if err := json.Unmarshal(b, &baseline); err != nil {
			loadError = fmt.Errorf("baseline manifest: %w", err)
			return
		}
		for _, name := range []string{"claude-code", "codex"} {
			b, err := manifestFS.ReadFile("profiles/" + name + ".json")
			if err != nil {
				loadError = err
				return
			}
			m := &manifest{}
			if err := json.Unmarshal(b, m); err != nil {
				loadError = fmt.Errorf("%s manifest: %w", name, err)
				return
			}
			if m.Login != nil {
				if _, err := m.Login.flow(m.id()); err != nil {
					loadError = err
					return
				}
			}
			builtins = append(builtins, m)
		}
	})
	return loadError
}

// profileFor returns the profile for a wrapper harness name. A profile not
// yet activated is refused, except for a login: signing in is the one step
// the activation runs need a person for, and a login launch runs nothing but
// the profile's own login and status commands (see Prepare).
func profileFor(harness string, login bool) (*manifest, error) {
	if err := loadManifests(); err != nil {
		return nil, refuseErr(StageProfile, err)
	}
	name := strings.ToLower(strings.TrimSpace(harness))
	testMu.Lock()
	var m *manifest
	for _, tm := range testProfiles {
		if slices.Contains(tm.Harnesses, name) {
			m = tm
			break
		}
	}
	testMu.Unlock()
	if m == nil {
		for _, bm := range builtins {
			if slices.Contains(bm.Harnesses, name) {
				m = bm
				break
			}
		}
	}
	switch {
	case m == nil:
		return nil, refuse(StageProfile,
			"no containment profile for harness %q (contained launches support claude and codex)", harness)
	case login && m.Login == nil:
		return nil, refuse(StageProfile, "the %s profile (%s) has no login flow", m.Name, m.HarnessVersion)
	case !login && !m.Activated && !activateAll.Load():
		return nil, refuse(StageProfile,
			"the %s profile (%s, manifest %d) is not activated: %s",
			m.Name, m.HarnessVersion, m.ManifestVersion, m.Activation)
	}
	return m, nil
}

// LoginFlow is a harness's login flow as its profile pins it (see loginSpec).
type LoginFlow struct {
	// Profile is the profile's "<name>@<version>".
	Profile string
	// Args and Status are the login and status commands' arguments.
	Args, Status []string
	// URL matches the sign-in page's address.
	URL *regexp.Regexp
	// UserCode matches the one-time code to enter on that page; nil when the
	// page needs none.
	UserCode *regexp.Regexp
	// CodePrompt is the prompt at which the harness waits for the code the
	// page shows after signing in; empty when it takes none.
	CodePrompt string
	// Success is printed once the login is stored.
	Success string
	// LoggedIn matches the status command's output when signed in.
	LoggedIn *regexp.Regexp
}

// LoginFlowFor returns the login flow the harness's profile pins. The profile
// need not be activated yet: signing in is how its activation runs begin.
func LoginFlowFor(harness string) (*LoginFlow, error) {
	m, err := profileFor(harness, true)
	if err != nil {
		return nil, err
	}
	return m.Login.flow(m.id())
}

func (s *loginSpec) flow(profile string) (*LoginFlow, error) {
	f := &LoginFlow{
		Profile:    profile,
		Args:       slices.Clone(s.Args),
		Status:     slices.Clone(s.Status),
		CodePrompt: s.CodePrompt,
		Success:    s.Success,
	}
	compile := func(field, expr string) (*regexp.Regexp, error) {
		if expr == "" {
			return nil, nil
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return nil, refuse(StageProfile, "the %s profile's login %s pattern: %v", profile, field, err)
		}
		return re, nil
	}
	var err error
	if f.URL, err = compile("url", s.URL); err != nil {
		return nil, err
	}
	if f.UserCode, err = compile("user_code", s.UserCode); err != nil {
		return nil, err
	}
	if f.LoggedIn, err = compile("logged_in", s.LoggedIn); err != nil {
		return nil, err
	}
	if len(f.Args) == 0 || len(f.Status) == 0 || f.URL == nil || f.LoggedIn == nil {
		return nil, refuse(StageProfile, "the %s profile's login flow needs args, status, url and logged_in", profile)
	}
	return f, nil
}

// TestProfile describes a profile the module's tests register for a stand-in
// harness: any executable, plus the listed read/execute trees.
type TestProfile struct {
	// Harness is the wrapper harness name the profile answers to.
	Harness string
	// ExecDirs are extra read/execute directories (the test binary's own).
	ExecDirs []string
	// ConfigEnv, when set, points the harness at a state root inside HOME.
	ConfigEnv string
	// AuthEnv are the stand-in's credential variables.
	AuthEnv []string
	// Login, when set, is the stand-in's login flow.
	Login *TestLogin
	// Inactive registers the profile as not yet activated, so only a login
	// may launch it.
	Inactive bool
}

// TestLogin is a stand-in's login flow; the fields mean what loginSpec's do.
type TestLogin struct {
	Args, Status                                          []string
	URL, UserCode, CodePrompt, Success, LoggedIn, TCPBind string
}

// RegisterTestProfile installs a profile for a stand-in harness until the
// returned function runs. It exists for this module's tests only: internal
// packages are not importable from outside the module.
func RegisterTestProfile(tp TestProfile) (restore func()) {
	m := &manifest{
		Name:            "test-" + tp.Harness,
		Harnesses:       []string{strings.ToLower(tp.Harness)},
		ManifestVersion: 1,
		HarnessVersion:  "test",
		Activated:       !tp.Inactive,
		Activation:      "a test profile registered as inactive",
		Executable:      executableSpec{Kind: "test"},
		Env:             map[string]string{},
		AuthEnv:         slices.Clone(tp.AuthEnv),
		testDirs:        slices.Clone(tp.ExecDirs),
	}
	if tp.ConfigEnv != "" {
		m.State = stateSpec{ConfigEnv: tp.ConfigEnv, ConfigDir: ".config-" + tp.Harness, ConfigUnderHome: true}
	}
	if l := tp.Login; l != nil {
		m.Login = &loginSpec{
			Args: slices.Clone(l.Args), Status: slices.Clone(l.Status),
			URL: l.URL, UserCode: l.UserCode, CodePrompt: l.CodePrompt, Success: l.Success, LoggedIn: l.LoggedIn,
			TCPBind: l.TCPBind,
		}
	}
	testMu.Lock()
	testProfiles = append(testProfiles, m)
	testMu.Unlock()
	return func() {
		testMu.Lock()
		defer testMu.Unlock()
		testProfiles = slices.DeleteFunc(testProfiles, func(x *manifest) bool { return x == m })
	}
}

// ActivateProfilesForTest lets launches use the built-in profiles before their
// authenticated conformance has passed, until the returned function runs. The
// conformance job itself needs it; nothing outside the module can call it.
func ActivateProfilesForTest() (restore func()) {
	prev := activateAll.Swap(true)
	return func() { activateAll.Store(prev) }
}

// BaselineManifestVersion reports the version of the shared baseline, which
// every profile's effective version includes.
func BaselineManifestVersion() int {
	if err := loadManifests(); err != nil {
		return 0
	}
	return baseline.ManifestVersion
}
