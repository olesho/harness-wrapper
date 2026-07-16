// Package discovery answers "is harness X's CLI installed on PATH,
// and at what version?" for every harness declared in versions.json.
// It is the single source of truth for harness availability across
// every harness-wrapper consumer.
//
// The package is read-only with respect to the filesystem: it never
// writes, never modifies PATH, never installs anything. It holds an
// in-memory cache of detected versions keyed by binary path + mtime so
// repeated lookups (e.g. in a long-running supervisor) are cheap.
package discovery

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/olesho/harness-wrapper/pkg/versions"
)

// Info describes the availability and version state of one harness CLI.
type Info struct {
	// Name is the lookup name the caller passed in.
	Name string

	// Harness is the canonical harness key from versions.json (e.g.
	// "claude-code"). Empty when Name does not match any known harness.
	Harness string

	// Binary is the on-PATH executable name actually probed. Equal to
	// the entry's binary field for known harnesses, or to Name for
	// unknown names.
	Binary string

	// Path is the absolute path of the binary as resolved by
	// exec.LookPath. Empty when Installed is false.
	Path string

	// Installed reports whether Binary was found on PATH.
	Installed bool

	// InstallHint is a one-line human-readable hint shown when
	// Installed is false. Empty otherwise.
	InstallHint string

	// PinnedVersion is the versions.json `pinned` value for known
	// harnesses; empty for unknown harnesses or unpinned entries.
	PinnedVersion string

	// DetectedVersion is the version parsed from the binary's
	// --version output, when a probe is registered for this harness
	// and the invocation succeeded.
	DetectedVersion string

	// VersionMatchesPin reports whether DetectedVersion equals
	// PinnedVersion. Returns true when either is empty — callers must
	// not treat "unknown" as drift.
	VersionMatchesPin bool

	// VersionProbeError carries the human-readable failure reason when
	// a probe was registered and attempted but failed. Empty when no
	// probe is registered or the probe succeeded.
	VersionProbeError string

	// NPMPackage is the versions.json `package` value for known
	// harnesses; empty for unknown.
	NPMPackage string
}

// Probe parses a harness binary's version from its --version (or
// equivalent) output. Implementations should be cheap (one subprocess
// call) and treat parse failures as errors rather than returning the
// raw output.
type Probe interface {
	Detect(ctx context.Context, binPath string) (string, error)
}

// defaultProbeTimeout bounds a single `<binary> --version` invocation.
// It is a safety net for a genuinely hung binary, not a latency target,
// so it is set generously: the harness CLIs are node-based (claude,
// codex, gemini) and a cold `--version` can take 1-2s just for node to
// start, more on a loaded machine. A tight bound (the original 2s) made
// detection spuriously fail under heavy parallel load — the probe child
// was SIGKILLed mid-start ("--version: signal: killed"), reported as an
// unknown version and, with a pin set, a false drift signal.
const defaultProbeTimeout = 20 * time.Second

var (
	probesMu sync.RWMutex
	probes   = map[string]Probe{}

	cache sync.Map // path string -> *cacheEntry
)

type cacheEntry struct {
	mtime   time.Time
	version string
	err     error
}

// RegisterProbe associates a version probe with a canonical harness
// key (as it appears in versions.json). Overwrites any prior
// registration. Safe to call from init().
//
// Panics if p is nil.
func RegisterProbe(harness string, p Probe) {
	if p == nil {
		panic("discovery: RegisterProbe called with nil Probe")
	}
	probesMu.Lock()
	defer probesMu.Unlock()
	probes[harness] = p
}

func probeFor(harness string) (Probe, bool) {
	probesMu.RLock()
	defer probesMu.RUnlock()
	p, ok := probes[harness]
	return p, ok
}

// Lookup resolves a name to availability info. The name may be:
//
//   - a canonical harness key (e.g. "claude-code")
//   - a registered binary name (e.g. "claude")
//   - any other binary name (treated as a raw PATH probe; Harness and
//     the versions.json-sourced fields are left empty)
//
// Returns Info populated as far as possible. An error is returned only
// for internal failures (e.g. versions.json is unreadable); a binary
// that is simply not on PATH is a normal result with Installed=false.
func Lookup(name string) (Info, error) {
	all, err := versions.All()
	if err != nil {
		return Info{}, fmt.Errorf("discovery: %w", err)
	}

	var (
		harness string
		entry   versions.Entry
		binary  = name
	)
	if e, ok := all[name]; ok {
		harness, entry = name, e
		binary = e.Binary
	} else {
		for k, e := range all {
			if e.Binary == name {
				harness, entry, binary = k, e, e.Binary
				break
			}
		}
	}

	info := Info{
		Name:          name,
		Harness:       harness,
		Binary:        binary,
		PinnedVersion: entry.Pinned,
		NPMPackage:    entry.Package,
		// Default to true so callers never treat "unknown" as drift.
		// Only flipped to false when both PinnedVersion and
		// DetectedVersion are populated and unequal.
		VersionMatchesPin: true,
	}

	path, lerr := exec.LookPath(binary)
	if lerr != nil {
		info.InstallHint = buildInstallHint(binary, harness, entry.Package)
		return info, nil
	}
	info.Path = path
	info.Installed = true

	if harness == "" {
		return info, nil
	}
	probe, hasProbe := probeFor(harness)
	if !hasProbe {
		return info, nil
	}
	version, perr := cachedDetect(probe, path)
	if perr != nil {
		info.VersionProbeError = perr.Error()
		return info, nil
	}
	info.DetectedVersion = version
	if entry.Pinned != "" {
		info.VersionMatchesPin = version == entry.Pinned
	}
	return info, nil
}

// Discover returns Info for every harness declared in versions.json.
// Useful for "what's installed on this machine?" UIs. Order is not
// guaranteed.
func Discover() ([]Info, error) {
	all, err := versions.All()
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	out := make([]Info, 0, len(all))
	for harness := range all {
		info, err := Lookup(harness)
		if err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, nil
}

// ResetCache clears the version-detection cache. Intended for tests
// that need to re-probe after swapping a shim.
func ResetCache() {
	cache.Range(func(k, _ any) bool {
		cache.Delete(k)
		return true
	})
}

func buildInstallHint(binary, harness, npmPkg string) string {
	if npmPkg != "" && harness != "" {
		return fmt.Sprintf("%q not on PATH. Install %s (e.g. `npm i -g %s`).",
			binary, harness, npmPkg)
	}
	return fmt.Sprintf("%q not on PATH.", binary)
}

func cachedDetect(p Probe, path string) (string, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	mtime := stat.ModTime()

	if raw, ok := cache.Load(path); ok {
		entry := raw.(*cacheEntry)
		if entry.mtime.Equal(mtime) {
			return entry.version, entry.err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultProbeTimeout)
	defer cancel()

	version, perr := p.Detect(ctx, path)
	cache.Store(path, &cacheEntry{mtime: mtime, version: version, err: perr})
	return version, perr
}
