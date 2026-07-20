package versions

import "testing"

// TestParityWithMetaHarness asserts that this repo's embedded
// versions.json is semantically equal to the vendored snapshot of
// meta-harness's src/versions/versions.json (the TS port's pin file).
// The two repos format the file differently (compact vs pretty-printed),
// so equality is over parsed entries, never bytes.
//
// Coverage is one-directional by design: this test catches THIS repo
// drifting from the vendored snapshot. If meta-harness moves its pins
// first, nothing here notices until a dev runs
// scripts/sync-versions.sh --check against a sibling checkout; the
// meta-harness-drifts-first direction is enforced by META-HARNESS-91's
// symmetric vendored check in meta-harness's CI (not yet live). Refresh
// the snapshot with scripts/sync-versions.sh.
func TestParityWithMetaHarness(t *testing.T) {
	vendored, err := ReadFrom("testdata/meta-harness-versions.json")
	if err != nil {
		t.Fatalf("read vendored meta-harness pins: %v", err)
	}
	live, err := All()
	if err != nil {
		t.Fatalf("read embedded pins: %v", err)
	}

	// Key sets must be equal in both directions — no subset comparison.
	for name := range live {
		if _, ok := vendored[name]; !ok {
			t.Errorf("harness %q pinned here but missing from meta-harness snapshot", name)
		}
	}
	for name := range vendored {
		if _, ok := live[name]; !ok {
			t.Errorf("harness %q pinned in meta-harness snapshot but missing here", name)
		}
	}

	for name, want := range vendored {
		got, ok := live[name]
		if !ok {
			continue // already reported above
		}
		if got != want {
			t.Errorf("harness %q drifted from meta-harness: here %+v, meta-harness %+v", name, got, want)
		}
	}
}
