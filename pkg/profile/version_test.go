package profile

import "testing"

func TestVersionAtLeast(t *testing.T) {
	tests := []struct {
		version string
		min     string
		want    bool
	}{
		{"2.1.234", "2.1.234", true},
		{"2.1.235", "2.1.234", true},
		{"2.1.233", "2.1.234", false},
		{"2.2.0", "2.1.234", true},
		{"3.0.0", "2.1.234", true},
		{"1.9.999", "2.1.234", false},
		// Numeric, not lexicographic: a string compare would call this false,
		// which is the bug this function exists to avoid.
		{"10.0.0", "9.0.0", true},
		{"9.0.0", "10.0.0", false},
		{"2.10.0", "2.9.0", true},
		// Noisy real-world output.
		{"2.1.235 (Claude Code)", "2.1.234", true},
		{"claude-code/2.1.235 darwin-arm64", "2.1.234", true},
		// Pre-release and build suffixes are ignored: compared on X.Y.Z only.
		{"2.1.234-rc.1", "2.1.234", true},
		{"2.1.234+build.7", "2.1.234", true},
		{"2.1.233-rc.1", "2.1.234", false},
	}
	for _, tc := range tests {
		t.Run(tc.version+" >= "+tc.min, func(t *testing.T) {
			got, err := VersionAtLeast(tc.version, tc.min)
			if err != nil {
				t.Fatalf("VersionAtLeast: %v", err)
			}
			if got != tc.want {
				t.Errorf("VersionAtLeast(%q, %q) = %v, want %v", tc.version, tc.min, got, tc.want)
			}
		})
	}
}

func TestVersionAtLeastErrors(t *testing.T) {
	for _, tc := range []struct{ version, min string }{
		{"", "2.1.234"},
		{"unreleased", "2.1.234"},
		{"2.1", "2.1.234"},
		{"2.1.234", "nonsense"},
	} {
		if _, err := VersionAtLeast(tc.version, tc.min); err == nil {
			t.Errorf("VersionAtLeast(%q, %q): want an error", tc.version, tc.min)
		}
	}
}
