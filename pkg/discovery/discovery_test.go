package discovery

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

type nameContent struct{ name, body string }

// setShimPath writes the given shims into a fresh temp dir, points
// PATH at it, and arranges for the version cache to be cleared on
// test cleanup. Returns the temp dir.
func setShimPath(t *testing.T, shims ...nameContent) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shim tests require POSIX-shell semantics")
	}
	dir := t.TempDir()
	for _, s := range shims {
		body := s.body
		if body == "" {
			body = "#!/bin/sh\nexit 0\n"
		}
		if err := os.WriteFile(filepath.Join(dir, s.name), []byte(body), 0o755); err != nil {
			t.Fatalf("write shim %q: %v", s.name, err)
		}
	}
	t.Setenv("PATH", dir)
	t.Cleanup(ResetCache)
	return dir
}

func TestLookup_NotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Cleanup(ResetCache)

	got, err := Lookup("codex")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Installed {
		t.Error("want Installed=false")
	}
	if got.Harness != "codex" {
		t.Errorf("want Harness=codex, got %q", got.Harness)
	}
	if got.Binary != "codex" {
		t.Errorf("want Binary=codex, got %q", got.Binary)
	}
	if got.PinnedVersion != "0.130.0" {
		t.Errorf("want PinnedVersion=0.130.0, got %q", got.PinnedVersion)
	}
	if got.InstallHint == "" || !strings.Contains(got.InstallHint, "codex") {
		t.Errorf("InstallHint should mention codex, got %q", got.InstallHint)
	}
	if !got.VersionMatchesPin {
		t.Error("want VersionMatchesPin=true when not installed (no drift signal)")
	}
}

func TestLookup_InstalledViaHarnessKey(t *testing.T) {
	setShimPath(t, nameContent{"codex", "#!/bin/sh\necho 0.130.0\n"})

	got, err := Lookup("codex")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !got.Installed {
		t.Errorf("want Installed=true (probe err %q)", got.VersionProbeError)
	}
	if got.Harness != "codex" {
		t.Errorf("want Harness=codex, got %q", got.Harness)
	}
	if got.Binary != "codex" {
		t.Errorf("want Binary=codex, got %q", got.Binary)
	}
	if got.DetectedVersion != "0.130.0" {
		t.Errorf("want DetectedVersion=0.130.0, got %q", got.DetectedVersion)
	}
	if !got.VersionMatchesPin {
		t.Error("want VersionMatchesPin=true (detected matches pin)")
	}
}

func TestLookup_InstalledViaBinaryName(t *testing.T) {
	setShimPath(t, nameContent{"claude", "#!/bin/sh\necho 2.1.141\n"})

	got, err := Lookup("claude")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !got.Installed {
		t.Errorf("want Installed=true (probe err %q)", got.VersionProbeError)
	}
	if got.Harness != "claude-code" {
		t.Errorf("want Harness=claude-code (looked up by binary), got %q", got.Harness)
	}
	if got.Binary != "claude" {
		t.Errorf("want Binary=claude, got %q", got.Binary)
	}
	if got.DetectedVersion != "2.1.141" {
		t.Errorf("want DetectedVersion=2.1.141, got %q", got.DetectedVersion)
	}
	if !got.VersionMatchesPin {
		t.Error("want VersionMatchesPin=true")
	}
}

func TestLookup_UnknownName_NotInstalled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Cleanup(ResetCache)

	got, err := Lookup("xyzzy")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Installed {
		t.Error("want Installed=false")
	}
	if got.Harness != "" {
		t.Errorf("want Harness=\"\" for unknown name, got %q", got.Harness)
	}
	if got.PinnedVersion != "" || got.NPMPackage != "" {
		t.Errorf("want empty versions.json-derived fields, got pinned=%q pkg=%q",
			got.PinnedVersion, got.NPMPackage)
	}
	if !strings.Contains(got.InstallHint, "xyzzy") {
		t.Errorf("InstallHint should mention xyzzy, got %q", got.InstallHint)
	}
}

func TestLookup_UnknownName_Installed(t *testing.T) {
	setShimPath(t, nameContent{"xyzzy", "#!/bin/sh\necho irrelevant\n"})

	got, err := Lookup("xyzzy")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !got.Installed {
		t.Error("want Installed=true")
	}
	if got.Harness != "" {
		t.Errorf("want Harness=\"\", got %q", got.Harness)
	}
	if got.DetectedVersion != "" {
		t.Errorf("want DetectedVersion=\"\" (no probe for unknown), got %q",
			got.DetectedVersion)
	}
	if !got.VersionMatchesPin {
		t.Error("want VersionMatchesPin=true (no pin to drift from)")
	}
}

func TestLookup_VersionMismatch_FlagsDrift(t *testing.T) {
	setShimPath(t, nameContent{"codex", "#!/bin/sh\necho 9.9.9\n"})

	got, err := Lookup("codex")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.DetectedVersion != "9.9.9" {
		t.Errorf("want DetectedVersion=9.9.9, got %q", got.DetectedVersion)
	}
	if got.PinnedVersion != "0.130.0" {
		t.Errorf("want PinnedVersion=0.130.0, got %q", got.PinnedVersion)
	}
	if got.VersionMatchesPin {
		t.Error("want VersionMatchesPin=false (detected drifts from pin)")
	}
}

func TestLookup_VersionProbeError_DoesNotFlagDrift(t *testing.T) {
	setShimPath(t, nameContent{"codex", "#!/bin/sh\nexit 1\n"})

	got, err := Lookup("codex")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !got.Installed {
		t.Error("want Installed=true (binary exists, just exits 1)")
	}
	if got.DetectedVersion != "" {
		t.Errorf("want DetectedVersion=\"\", got %q", got.DetectedVersion)
	}
	if got.VersionProbeError == "" {
		t.Error("want non-empty VersionProbeError")
	}
	if !got.VersionMatchesPin {
		t.Error("want VersionMatchesPin=true (probe failed → unknown, not drift)")
	}
}

func TestLookup_VersionUnpinned_TreatedAsCompatible(t *testing.T) {
	setShimPath(t, nameContent{"gemini", "#!/bin/sh\necho 0.1.0\n"})

	got, err := Lookup("gemini")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.DetectedVersion != "0.1.0" {
		t.Errorf("want DetectedVersion=0.1.0, got %q", got.DetectedVersion)
	}
	if got.PinnedVersion != "" {
		t.Errorf("want PinnedVersion=\"\" (gemini intentionally unpinned), got %q",
			got.PinnedVersion)
	}
	if !got.VersionMatchesPin {
		t.Error("want VersionMatchesPin=true when pin is empty")
	}
}

func TestDiscover_ReturnsAllHarnesses(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Cleanup(ResetCache)

	all, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := map[string]bool{"codex": false, "claude-code": false, "gemini": false, "opencode": false, "pi": false}
	for _, info := range all {
		if _, ok := want[info.Harness]; !ok {
			t.Errorf("unexpected harness in Discover: %q", info.Harness)
			continue
		}
		want[info.Harness] = true
		if info.Installed {
			t.Errorf("harness %q should not be installed under scrubbed PATH", info.Harness)
		}
	}
	for h, seen := range want {
		if !seen {
			t.Errorf("Discover missing harness %q", h)
		}
	}
}

func TestInit_ShipsDefaultProbes(t *testing.T) {
	for _, h := range []string{"codex", "claude-code", "gemini", "opencode", "pi"} {
		if _, ok := probeFor(h); !ok {
			t.Errorf("expected default probe for %q after init()", h)
		}
	}
}

func TestRegisterProbe_NilPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil Probe")
		}
	}()
	RegisterProbe("codex", nil)
}

func TestResetCache_EmptyIsFine(t *testing.T) {
	ResetCache()
	ResetCache() // idempotent
}

// countingProbe wraps another probe and counts invocations. Used to
// verify the cache short-circuits repeated detects.
type countingProbe struct {
	inner Probe
	n     atomic.Int64
}

func (cp *countingProbe) Detect(ctx context.Context, path string) (string, error) {
	cp.n.Add(1)
	return cp.inner.Detect(ctx, path)
}

func swapCodexProbe(t *testing.T, p Probe) {
	t.Helper()
	probesMu.Lock()
	prev := probes["codex"]
	probes["codex"] = p
	probesMu.Unlock()
	t.Cleanup(func() {
		probesMu.Lock()
		probes["codex"] = prev
		probesMu.Unlock()
	})
}

func TestLookup_CachesByPathAndMtime(t *testing.T) {
	cp := &countingProbe{inner: semverDashVProbe{}}
	swapCodexProbe(t, cp)
	setShimPath(t, nameContent{"codex", "#!/bin/sh\necho 0.130.0\n"})

	if _, err := Lookup("codex"); err != nil {
		t.Fatalf("first Lookup: %v", err)
	}
	if _, err := Lookup("codex"); err != nil {
		t.Fatalf("second Lookup: %v", err)
	}

	if got := cp.n.Load(); got != 1 {
		t.Errorf("want 1 probe invocation (second should hit cache), got %d", got)
	}
}

func TestLookup_ResetCacheReprobes(t *testing.T) {
	cp := &countingProbe{inner: semverDashVProbe{}}
	swapCodexProbe(t, cp)
	setShimPath(t, nameContent{"codex", "#!/bin/sh\necho 0.130.0\n"})

	if _, err := Lookup("codex"); err != nil {
		t.Fatalf("first Lookup: %v", err)
	}
	ResetCache()
	if _, err := Lookup("codex"); err != nil {
		t.Fatalf("second Lookup: %v", err)
	}

	if got := cp.n.Load(); got != 2 {
		t.Errorf("want 2 probe invocations after ResetCache, got %d", got)
	}
}

func TestSemverRe_MatchesVariousShapes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0.130.0", "0.130.0"},
		{"codex-cli 0.130.0", "0.130.0"},
		{"Claude Code 2.1.141 (build abc)", "2.1.141"},
		{"v1.0.0-beta.3", "1.0.0-beta.3"},
		{"no version here", ""},
	}
	for _, tc := range cases {
		got := semverRe.FindString(tc.in)
		if got != tc.want {
			t.Errorf("semverRe(%q): want %q, got %q", tc.in, tc.want, got)
		}
	}
}
