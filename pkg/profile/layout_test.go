package profile

import (
	"errors"
	"strings"
	"testing"
)

func TestHarnessAccessors(t *testing.T) {
	tests := []struct {
		h      Harness
		envVar string
		binary string
	}{
		{Claude, "CLAUDE_CONFIG_DIR", "claude"},
		{Codex, "CODEX_HOME", "codex"},
	}
	for _, tc := range tests {
		t.Run(string(tc.h), func(t *testing.T) {
			if got := tc.h.EnvVar(); got != tc.envVar {
				t.Errorf("EnvVar = %q, want %q", got, tc.envVar)
			}
			if got := tc.h.Binary(); got != tc.binary {
				t.Errorf("Binary = %q, want %q", got, tc.binary)
			}
			if err := tc.h.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
		})
	}
}

func TestHarnessesStableOrder(t *testing.T) {
	got := Harnesses()
	if len(got) != 2 || got[0] != Claude || got[1] != Codex {
		t.Fatalf("Harnesses() = %v, want [claude codex]", got)
	}
	// The caller may not mutate the package's view of the world.
	got[0] = "tampered"
	if Harnesses()[0] != Claude {
		t.Error("Harnesses() returns a shared slice")
	}
}

func TestUnknownHarness(t *testing.T) {
	const h Harness = "nope"
	if err := h.Validate(); !errors.Is(err, ErrUnknownHarness) {
		t.Errorf("Validate = %v, want ErrUnknownHarness", err)
	}
	if got := h.EnvVar(); got != "" {
		t.Errorf("EnvVar = %q, want empty", got)
	}
	if got := h.Binary(); got != "" {
		t.Errorf("Binary = %q, want empty", got)
	}
	if got := h.SeedFiles(); got != nil {
		t.Errorf("SeedFiles = %v, want nil", got)
	}
	if got := h.ExcludedDirs(); got != nil {
		t.Errorf("ExcludedDirs = %v, want nil", got)
	}
	if h.IsSeed("auth.json") {
		t.Error("IsSeed true for an unknown harness")
	}
	if h.IsExcludedDir("sessions") {
		t.Error("IsExcludedDir true for an unknown harness")
	}
}

// TestIsSeedTopLevelOnly pins the rule the shell implementation encodes as
// "os.sep not in rel": a seed name nested under a provisioned directory is
// ordinary content and must be manifested.
func TestIsSeedTopLevelOnly(t *testing.T) {
	if !Codex.IsSeed("auth.json") {
		t.Error("codex auth.json is not a seed")
	}
	if Codex.IsSeed("skills/auth.json") {
		t.Error("skills/auth.json was treated as a seed")
	}
	if !Claude.IsSeed(".credentials.json") || !Claude.IsSeed(".claude.json") {
		t.Error("claude seed pair not recognised")
	}
	// auth.json is declared for codex only: claude has none, and declaring an
	// inert entry there would misdescribe the contract.
	if Claude.IsSeed("auth.json") {
		t.Error("auth.json is a claude seed; it is codex-only")
	}
}

func TestExcludedDirs(t *testing.T) {
	for _, h := range Harnesses() {
		dirs := h.ExcludedDirs()
		if len(dirs) == 0 {
			t.Errorf("%s: no excluded dirs", h)
		}
		for _, d := range dirs {
			if d == "" || d != strings.ToLower(d) || strings.ContainsRune(d, '/') {
				t.Errorf("%s: bad excluded dir %q", h, d)
			}
			if !h.IsExcludedDir(d) {
				t.Errorf("%s: IsExcludedDir(%q) = false", h, d)
			}
		}
		// skills/ is versioned workspace content and must be provisioned.
		if h.IsExcludedDir("skills") {
			t.Errorf("%s: skills is excluded; it must be provisioned", h)
		}
	}
	if !Claude.IsExcludedDir("sessions") || !Claude.IsExcludedDir("projects") {
		t.Error("claude runtime dirs are not excluded")
	}
	if !Codex.IsExcludedDir("log") {
		t.Error("codex log dir is not excluded")
	}
}

func TestSeedListsAreCopies(t *testing.T) {
	seeds := Claude.SeedFiles()
	if len(seeds) == 0 {
		t.Fatal("no claude seeds")
	}
	seeds[0] = "tampered"
	if Claude.SeedFiles()[0] == "tampered" {
		t.Error("SeedFiles returns the package's own slice")
	}
	dirs := Claude.ExcludedDirs()
	dirs[0] = "tampered"
	if Claude.ExcludedDirs()[0] == "tampered" {
		t.Error("ExcludedDirs returns the package's own slice")
	}
}
