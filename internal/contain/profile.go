package contain

import (
	"embed"
	"encoding/json"
	"fmt"
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
			builtins = append(builtins, m)
		}
	})
	return loadError
}

// profileFor returns the profile for a wrapper harness name.
func profileFor(harness string) (*manifest, error) {
	if err := loadManifests(); err != nil {
		return nil, refuseErr(StageProfile, err)
	}
	name := strings.ToLower(strings.TrimSpace(harness))
	testMu.Lock()
	for _, m := range testProfiles {
		if slices.Contains(m.Harnesses, name) {
			testMu.Unlock()
			return m, nil
		}
	}
	testMu.Unlock()
	for _, m := range builtins {
		if slices.Contains(m.Harnesses, name) {
			if !m.Activated && !activateAll.Load() {
				return nil, refuse(StageProfile,
					"the %s profile (%s, manifest %d) is not activated: %s",
					m.Name, m.HarnessVersion, m.ManifestVersion, m.Activation)
			}
			return m, nil
		}
	}
	return nil, refuse(StageProfile,
		"no containment profile for harness %q (contained launches support claude and codex)", harness)
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
		Activated:       true,
		Executable:      executableSpec{Kind: "test"},
		Env:             map[string]string{},
		testDirs:        slices.Clone(tp.ExecDirs),
	}
	if tp.ConfigEnv != "" {
		m.State = stateSpec{ConfigEnv: tp.ConfigEnv, ConfigDir: ".config-" + tp.Harness, ConfigUnderHome: true}
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
