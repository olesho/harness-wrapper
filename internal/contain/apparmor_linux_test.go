//go:build linux

package contain

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/internal/apparmor"
	"github.com/olesho/harness-wrapper/internal/landlock"
	"github.com/olesho/harness-wrapper/pkg/containment"
)

// requireSocketLayer skips unless the kernel's Landlock predates RESOLVE_UNIX
// and the AppArmor socket layer is installed, loaded and effective here — or
// fails, when HW_APPARMOR_REQUIRE says this cell exists to exercise it.
func requireSocketLayer(t *testing.T) *apparmor.Layer {
	t.Helper()
	skip := func(format string, args ...any) {
		t.Helper()
		if v := os.Getenv("HW_APPARMOR_REQUIRE"); v != "" && v != "0" {
			t.Fatalf(format, args...)
		}
		t.Skipf(format, args...)
	}
	abi, err := landlock.ABI()
	if err != nil || abi < landlock.MinimumABI || abi >= landlock.ResolveUnixABI {
		skip("the socket layer needs Landlock ABI %d-%d; this kernel has %d (%v)", landlock.MinimumABI, landlock.ResolveUnixABI-1, abi, err)
	}
	layer, err := socketLayer(abi)
	if err != nil {
		skip("the AppArmor socket layer is not usable here: %v", err)
	}
	return layer
}

// socketLayerArea returns a directory beneath one of the layer's roots, for
// the working directory and managed state a contained launch writes.
func socketLayerArea(t *testing.T, layer *apparmor.Layer) string {
	t.Helper()
	for _, r := range layer.Roots {
		if dir, err := os.MkdirTemp(r, "hw-aa-test-"); err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
			return dir
		}
	}
	t.Skipf("no root of %v is writable by this user", layer.Roots)
	return ""
}

func listenUnix(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

func wantRefusal(t *testing.T, in Input, stage string) {
	t.Helper()
	l, err := Prepare(in)
	if err == nil {
		l.Release()
		t.Fatalf("Prepare accepted the launch; want a refusal at stage %q", stage)
	}
	var re *RefusalError
	if !errors.As(err, &re) || re.Stage != stage {
		t.Fatalf("refusal %v, want stage %q", err, stage)
	}
	t.Logf("refused as designed: %v", err)
}

// TestAppArmorSocketLayer is the fallback's end-to-end cell: on Landlock ABI
// 6-8, a request that leaves MinABI unset detects the installed socket layer
// and stacks it, so the harness is refused a pathname socket outside the roots
// and reaches one beneath them; a request that requires Landlock alone, or a
// writable grant the profile would break, is refused before anything starts.
func TestAppArmorSocketLayer(t *testing.T) {
	layer := requireSocketLayer(t)
	self := testHarness(t)
	area := socketLayerArea(t, layer)
	wd := filepath.Join(area, "wd")
	for _, d := range []string{wd, filepath.Join(area, "state")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(area, "state"))

	outDir, err := selfTestDir(layer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outDir) })
	outside := filepath.Join(outDir, "s")
	inside := filepath.Join(wd, "in.sock")
	listenUnix(t, outside)
	listenUnix(t, inside)

	// Ways to name the outside socket from beneath a root: the profile must
	// judge the socket's real path, not the name used.
	hard := filepath.Join(wd, "hard.sock")
	soft := filepath.Join(wd, "soft.sock")
	viaProc := "/proc/self/root" + outside

	report := filepath.Join(wd, "report.json")
	checks := []string{
		"connect-unix=" + outside,
		"connect-unix=" + inside,
		"listen-unix=" + filepath.Join(wd, "own.sock"),
		"write=" + filepath.Join(wd, "f"),
		"write=" + filepath.Join(outDir, "f"),
		"link=" + outside + ":" + hard,
		"connect-unix=" + hard,
		"symlink=" + outside + ":" + soft,
		"connect-unix=" + soft,
		"connect-unix=" + viaProc,
	}
	in := helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock},
		append([]string{report}, checks...)...)
	applied, st, _ := runContained(t, in)
	if !st.Success() {
		t.Fatalf("helper exited %v", st)
	}
	rep := readHelperReport(t, report)
	for check, want := range map[string][]string{
		"connect-unix=" + outside:                      {"EACCES"},
		"connect-unix=" + inside:                       {"ok"},
		"listen-unix=" + filepath.Join(wd, "own.sock"): {"ok"},
		"write=" + filepath.Join(wd, "f"):              {"ok"},
		"write=" + filepath.Join(outDir, "f"):          {"EACCES"},
		// Landlock refuses the cross-directory link (no REFER on the source).
		"link=" + outside + ":" + hard:    {"EXDEV", "EACCES"},
		"connect-unix=" + hard:            {"ENOENT", "EACCES"},
		"symlink=" + outside + ":" + soft: {"ok"},
		"connect-unix=" + soft:            {"EACCES"},
		"connect-unix=" + viaProc:         {"EACCES"},
	} {
		if got := rep.Checks[check]; !slices.Contains(want, got) {
			t.Errorf("%s: %s, want one of %v", check, got, want)
		}
	}
	if applied.PathnameSockets != containment.PathnameSocketsDeniedOutsideRoots {
		t.Errorf("pathname sockets %q, want %q", applied.PathnameSockets, containment.PathnameSocketsDeniedOutsideRoots)
	}
	if applied.AppArmor == nil || applied.AppArmor.Profile != layer.Profile || !slices.Equal(applied.AppArmor.Roots, layer.Roots) {
		t.Errorf("applied socket layer %+v, want %s with roots %v", applied.AppArmor, layer.Profile, layer.Roots)
	}
	if slices.Contains(applied.HandledFS, "resolve_unix") {
		t.Errorf("handled rights %v claim resolve_unix on ABI %d", applied.HandledFS, applied.ABI)
	}
	if applied.RequiredABI != containment.LowestABI {
		t.Errorf("required ABI %d, want %d under the socket layer", applied.RequiredABI, containment.LowestABI)
	}

	t.Run("explicit floor below the kernel stacks it too", func(t *testing.T) {
		kernelABI := applied.ABI
		applied, st, _ := runContained(t, helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock, MinABI: kernelABI},
			report, "connect-unix="+outside))
		if !st.Success() || applied.AppArmor == nil || applied.RequiredABI != kernelABI {
			t.Fatalf("exit %v, applied %+v", st, applied)
		}
		if got := readHelperReport(t, report).Checks["connect-unix="+outside]; got != "EACCES" {
			t.Fatalf("outside socket: %s, want EACCES", got)
		}
	})
	// The installed source narrowed (or otherwise changed) without a reload:
	// it describes a profile that is not loaded, so the launch must be
	// refused rather than report roots the loaded profile does not enforce.
	// Staging it for real needs root; the source is swapped in-process, and
	// the kernel still holds only the profile for the real roots.
	t.Run("source changed without a reload refused", func(t *testing.T) {
		narrowed := []string{filepath.Join(layer.Roots[0], "narrowed")}
		src, err := apparmor.Profile(narrowed)
		if err != nil {
			t.Fatal(err)
		}
		stale, err := apparmor.Parse([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		orig := installedSocketLayer
		installedSocketLayer = func() (*apparmor.Layer, error) { return stale, nil }
		defer func() { installedSocketLayer = orig }()
		l, err := Prepare(helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock}, report))
		if err == nil {
			l.Release()
			t.Fatal("a launch was prepared under a source whose profile is not loaded")
		}
		var re *RefusalError
		if !errors.As(err, &re) || re.Stage != StageKernel || !strings.Contains(err.Error(), "not loaded") {
			t.Fatalf("refusal %v, want stage %q naming the unloaded profile", err, StageKernel)
		}
		t.Logf("refused as designed: %v", err)
	})
	t.Run("min_abi 9 refused", func(t *testing.T) {
		wantRefusal(t, helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock, MinABI: containment.ResolveUnixABI}, report), StageKernel)
	})
	t.Run("writable grant outside the roots refused", func(t *testing.T) {
		wantRefusal(t, helperInput(t, self, wd, &containment.Request{
			Kind: containment.KindLandlock, ReadWrite: []string{outDir},
		}, report), StagePaths)
	})
	t.Run("working directory outside the roots refused", func(t *testing.T) {
		wantRefusal(t, helperInput(t, self, outDir, &containment.Request{
			Kind: containment.KindLandlock,
		}, report), StagePaths)
	})
	t.Run("preview reports the layer", func(t *testing.T) {
		p, err := PreviewLaunch(helperInput(t, self, wd, &containment.Request{Kind: containment.KindLandlock}))
		if err != nil {
			t.Fatal(err)
		}
		if !p.WouldLaunch || p.Planned.AppArmor == nil || p.Planned.PathnameSockets != containment.PathnameSocketsDeniedOutsideRoots {
			t.Fatalf("preview %+v missing %v, want a launch under the socket layer", p.Planned, p.Missing)
		}
	})
}
