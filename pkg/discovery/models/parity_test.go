package models

import (
	"os"
	"reflect"
	"sort"
	"testing"
)

// TestParityWithMetaHarness asserts that this repo's embedded models.json is
// semantically equal to the vendored snapshot of meta-harness's
// src/discovery/models.json (the TS port's curated registry). The two repos may
// format the file differently (pretty-printed vs compact), so equality is over
// parsed entries, never bytes.
//
// Coverage is one-directional by design: harness-wrapper is canonical and this
// test catches THIS repo drifting from the vendored snapshot. If meta-harness
// moves its registry first, nothing here notices until the snapshot is
// refreshed from a sibling checkout; the meta-harness-drifts-first direction is
// meta-harness's responsibility to enforce in its own CI. Refresh the snapshot
// by copying src/discovery/models.json into testdata/meta-harness-models.json.
func TestParityWithMetaHarness(t *testing.T) {
	data, err := os.ReadFile("testdata/meta-harness-models.json")
	if err != nil {
		t.Fatalf("read vendored meta-harness models: %v", err)
	}
	vendored, err := parse(data)
	if err != nil {
		t.Fatalf("parse vendored meta-harness models: %v", err)
	}
	live := registry

	// Key sets must be equal in both directions — no subset comparison.
	for name := range live {
		if _, ok := vendored[name]; !ok {
			t.Errorf("harness %q present here but missing from meta-harness snapshot", name)
		}
	}
	for name := range vendored {
		if _, ok := live[name]; !ok {
			t.Errorf("harness %q present in meta-harness snapshot but missing here", name)
		}
	}

	for name, want := range vendored {
		got, ok := live[name]
		if !ok {
			continue // already reported above
		}
		if got.Default != want.Default {
			t.Errorf("harness %q default drifted: here %q, meta-harness %q", name, got.Default, want.Default)
		}
		if !reflect.DeepEqual(got.Models, want.Models) {
			t.Errorf("harness %q models drifted: here %v, meta-harness %v", name, got.Models, want.Models)
		}
	}
}

// TestKnownModelsMatchesRegistry verifies the exported accessors against the
// curated data the acceptance criteria pin: claude-code lists 9 ids with default
// "opus"; codex lists 3 with default "gpt-5.5".
func TestKnownModelsMatchesRegistry(t *testing.T) {
	if got := DefaultModel("claude-code"); got != "opus" {
		t.Errorf("DefaultModel(claude-code) = %q, want %q", got, "opus")
	}
	if got := len(KnownModels("claude-code")); got != 9 {
		t.Errorf("len(KnownModels(claude-code)) = %d, want 9", got)
	}
	if got := DefaultModel("codex"); got != "gpt-5.5" {
		t.Errorf("DefaultModel(codex) = %q, want %q", got, "gpt-5.5")
	}
	if got := len(KnownModels("codex")); got != 3 {
		t.Errorf("len(KnownModels(codex)) = %d, want 3", got)
	}
}

// TestRegistryKeyAlias verifies the "claude" → "claude-code" normalization and
// case-insensitivity mirror the TS registryKey/norm semantics.
func TestRegistryKeyAlias(t *testing.T) {
	want := KnownModels("claude-code")
	for _, alias := range []string{"claude", "Claude", "  CLAUDE  ", "claude-code", "Claude-Code"} {
		got := KnownModels(alias)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("KnownModels(%q) = %v, want %v", alias, got, want)
		}
		if DefaultModel(alias) != "opus" {
			t.Errorf("DefaultModel(%q) = %q, want %q", alias, DefaultModel(alias), "opus")
		}
	}
}

// TestIsKnownModel covers the case-insensitive membership semantics and the
// empty/unknown edge cases from TS isKnownModel.
func TestIsKnownModel(t *testing.T) {
	cases := []struct {
		harness, model string
		want           bool
	}{
		{"claude-code", "opus", true},
		{"claude", "OPUS", true},
		{"claude-code", "  Opus  ", true},
		{"claude-code", "claude-opus-4-8", true},
		{"claude-code", "gpt-5.5", false},
		{"claude-code", "", false},
		{"codex", "gpt-5.4-mini", true},
		{"codex", "GPT-5.4-MINI", true},
		{"unknown", "opus", false},
	}
	for _, c := range cases {
		if got := IsKnownModel(c.harness, c.model); got != c.want {
			t.Errorf("IsKnownModel(%q, %q) = %v, want %v", c.harness, c.model, got, c.want)
		}
	}
}

// TestKnownModelsCopy verifies the returned slice is a defensive copy: mutating
// it must not corrupt the shared registry.
func TestKnownModelsCopy(t *testing.T) {
	a := KnownModels("claude-code")
	if len(a) == 0 {
		t.Fatal("expected non-empty model list")
	}
	sort.Strings(a)
	b := KnownModels("claude-code")
	if reflect.DeepEqual(a, b) {
		t.Skip("registry already sorted; copy semantics still hold")
	}
	// b must reflect original registry order, unaffected by sorting a.
	if b[0] != "default" {
		t.Errorf("KnownModels returned a shared (non-copied) slice: b[0] = %q, want %q", b[0], "default")
	}
}
