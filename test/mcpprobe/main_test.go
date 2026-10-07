package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbeOutcomes(t *testing.T) {
	readable := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(readable, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		forbidden, want string
	}{
		{"", "nonce=w no-target"},
		{readable, "nonce=w read-allowed"},
		{filepath.Join(t.TempDir(), "missing"), "nonce=w read-denied: "},
	} {
		if got := probe("w", tc.forbidden); !strings.HasPrefix(got, tc.want) {
			t.Errorf("probe(%q) = %q, want prefix %q", tc.forbidden, got, tc.want)
		}
	}
}
