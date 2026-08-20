package profile

import "testing"

// The worked vector from the design, and the one a shell implementation can be
// diffed against:
//
//	printf '%s' /tmp/p/claude | shasum -a 256 | cut -c1-8   ->  7629796b
const (
	vectorConfigDir = "/tmp/p/claude"
	vectorSuffix    = "7629796b"
)

func TestKeychainSlotClaudeCurrent(t *testing.T) {
	slot, ok := KeychainSlot(Claude, vectorConfigDir, "2.1.235 (Claude Code)")
	if !ok {
		t.Fatal("no slot for claude 2.1.235")
	}
	if slot.Suffix != vectorSuffix {
		t.Errorf("Suffix = %q, want %q", slot.Suffix, vectorSuffix)
	}
	if want := "Claude Code-credentials-" + vectorSuffix; slot.Service != want {
		t.Errorf("Service = %q, want %q", slot.Service, want)
	}
	if slot.BaseService != "Claude Code-credentials" {
		t.Errorf("BaseService = %q", slot.BaseService)
	}
	if !slot.OutranksFile {
		t.Error("OutranksFile = false; the slot wins over .credentials.json on 2.1.234+")
	}
	if slot.CredentialsFile != ".credentials.json" {
		t.Errorf("CredentialsFile = %q", slot.CredentialsFile)
	}
}

// TestKeychainSlotAccountAlwaysEmpty guards the purity boundary: the keychain
// account is the operator's macOS login name, which is host state. Nobody may
// later "helpfully" read $USER inside this package.
func TestKeychainSlotAccountAlwaysEmpty(t *testing.T) {
	for _, v := range []string{"2.1.234", "2.1.234 (Claude Code)", "9.9.9"} {
		slot, ok := KeychainSlot(Claude, vectorConfigDir, v)
		if !ok {
			t.Fatalf("no slot for %q", v)
		}
		if slot.Account != "" {
			t.Errorf("%q: Account = %q, want empty", v, slot.Account)
		}
	}
}

func TestKeychainSlotBoundary(t *testing.T) {
	if _, ok := KeychainSlot(Claude, vectorConfigDir, "2.1.234"); !ok {
		t.Error("2.1.234 (the first per-root version) got no slot")
	}
	if _, ok := KeychainSlot(Claude, vectorConfigDir, "2.1.233"); ok {
		t.Error("2.1.233 got a per-root slot; file auth only below 2.1.234")
	}
}

func TestKeychainSlotFailsClosed(t *testing.T) {
	cases := map[string]struct {
		h       Harness
		version string
	}{
		"codex any version": {Codex, "0.142.5"},
		"unknown harness":   {"nope", "2.1.235"},
		"unparseable":       {Claude, "unreleased build"},
		"empty version":     {Claude, ""},
		"older claude":      {Claude, "2.0.99"},
		"much older claude": {Claude, "1.9.999"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			slot, ok := KeychainSlot(tc.h, vectorConfigDir, tc.version)
			if ok {
				t.Fatalf("got a slot %+v; must fail closed to file-based auth", slot)
			}
			if slot != (AuthSlot{}) {
				t.Errorf("slot = %+v, want zero value", slot)
			}
		})
	}
}

// TestKeychainSlotDoesNotNormalise documents that configDir is hashed
// literally: a trailing slash is a DIFFERENT slot. Normalisation is the
// caller's job, and the caller must pass exactly what it injects into
// CLAUDE_CONFIG_DIR.
func TestKeychainSlotDoesNotNormalise(t *testing.T) {
	plain, _ := KeychainSlot(Claude, vectorConfigDir, "2.1.235")
	slashed, _ := KeychainSlot(Claude, vectorConfigDir+"/", "2.1.235")
	if plain.Suffix == slashed.Suffix {
		t.Error("trailing slash produced the same suffix; the path was normalised")
	}
}
