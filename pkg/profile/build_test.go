package profile

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// TestBuildPlanTestdataRoundTrip is the end-to-end acceptance path: a realistic
// source tree splits into provision/seed, the provisioned half builds a
// manifest, the manifest verifies, and a single mutated byte breaks it.
func TestBuildPlanTestdataRoundTrip(t *testing.T) {
	src := os.DirFS("testdata/profile/claude")

	plan, err := BuildPlan(src, Claude)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	wantProvision := []string{"CLAUDE.md", "settings.json", "skills/x.md"}
	if !reflect.DeepEqual(plan.Provision, wantProvision) {
		t.Errorf("Provision = %v, want %v", plan.Provision, wantProvision)
	}
	wantSeed := []string{".credentials.json"}
	if !reflect.DeepEqual(plan.Seed, wantSeed) {
		t.Errorf("Seed = %v, want %v", plan.Seed, wantSeed)
	}
	// sessions/ is excluded, so nothing under it appears anywhere.
	for _, rel := range append(append([]string{}, plan.Provision...), plan.Seed...) {
		if strings.HasPrefix(rel, "sessions/") {
			t.Errorf("excluded dir was walked: %q", rel)
		}
	}

	const version = "2.1.235 (Claude Code)"
	m, err := BuildManifest(src, plan.Provision, version)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if err := Verify(VerifyInput{FS: src, Manifest: m, HarnessVersion: version}); err != nil {
		t.Fatalf("Verify on a freshly built manifest: %v", err)
	}

	// Mutate one provisioned byte in a copy of the tree.
	mutated := fstest.MapFS{}
	for _, rel := range plan.Provision {
		data, err := os.ReadFile(filepath.Join("testdata/profile/claude", rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if rel == "settings.json" {
			data = append(data, ' ')
		}
		mutated[rel] = &fstest.MapFile{Data: data}
	}
	err = Verify(VerifyInput{FS: mutated, Manifest: m, HarnessVersion: version})
	if !errors.Is(err, ErrFingerprintMismatch) {
		t.Errorf("mutated byte: err = %v, want ErrFingerprintMismatch", err)
	}
}

func TestBuildPlanDropsJunkAndManifest(t *testing.T) {
	src := fstest.MapFS{
		"settings.json":        {Data: []byte("{}")},
		".DS_Store":            {Data: []byte("junk")},
		"skills/.DS_Store":     {Data: []byte("junk")},
		ManifestName:           {Data: []byte("{}")},
		"sessions/live.jsonl":  {Data: []byte("churn")},
		"projects/p/state.txt": {Data: []byte("churn")},
		".credentials.json":    {Data: []byte("secret")},
	}
	plan, err := BuildPlan(src, Claude)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if !reflect.DeepEqual(plan.Provision, []string{"settings.json"}) {
		t.Errorf("Provision = %v, want [settings.json]", plan.Provision)
	}
	if !reflect.DeepEqual(plan.Seed, []string{".credentials.json"}) {
		t.Errorf("Seed = %v, want [.credentials.json]", plan.Seed)
	}
}

func TestBuildPlanSorted(t *testing.T) {
	src := fstest.MapFS{
		"z.md":        {Data: []byte("z")},
		"a.md":        {Data: []byte("a")},
		"skills/b.md": {Data: []byte("b")},
	}
	plan, err := BuildPlan(src, Claude)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	want := []string{"a.md", "skills/b.md", "z.md"}
	if !reflect.DeepEqual(plan.Provision, want) {
		t.Errorf("Provision = %v, want %v", plan.Provision, want)
	}
}

func TestBuildPlanEmptyTree(t *testing.T) {
	plan, err := BuildPlan(fstest.MapFS{}, Claude)
	if err != nil {
		t.Fatalf("BuildPlan on an empty FS: %v", err)
	}
	if len(plan.Provision) != 0 || len(plan.Seed) != 0 {
		t.Errorf("plan = %+v, want empty", plan)
	}
}

func TestBuildPlanUnknownHarness(t *testing.T) {
	if _, err := BuildPlan(fstest.MapFS{}, "nope"); !errors.Is(err, ErrUnknownHarness) {
		t.Errorf("err = %v, want ErrUnknownHarness", err)
	}
}

// TestBuildPlanRejectsSymlink: following a symlink would launder content past
// the fingerprint, which is the one thing the fingerprint exists to notice.
func TestBuildPlanRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "linked.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, err := BuildPlan(os.DirFS(dir), Claude)
	if err == nil {
		t.Fatal("BuildPlan accepted a symlink")
	}
	if !strings.Contains(err.Error(), "linked.md") {
		t.Errorf("error %q does not name the symlink", err)
	}
}

func TestBuildManifestSortsAndDedupes(t *testing.T) {
	m, err := BuildManifest(goldenTree(), []string{"b/c.txt", "a.txt", "a.txt"}, "2.1.235")
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !reflect.DeepEqual(m.Files, []string{"a.txt", "b/c.txt"}) {
		t.Errorf("Files = %v, want [a.txt b/c.txt]", m.Files)
	}
	if m.Fingerprint != goldenSum {
		t.Errorf("Fingerprint = %q, want %q", m.Fingerprint, goldenSum)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("built manifest does not validate: %v", err)
	}
}

func TestBuildManifestRejectsEmptyVersion(t *testing.T) {
	_, err := BuildManifest(goldenTree(), []string{"a.txt"}, "")
	if !errors.Is(err, ErrManifestInvalid) {
		t.Errorf("err = %v, want ErrManifestInvalid", err)
	}
}

func TestBuildManifestMissingFile(t *testing.T) {
	if _, err := BuildManifest(goldenTree(), []string{"gone.txt"}, "2.1.235"); err == nil {
		t.Error("BuildManifest accepted a missing file")
	}
}
