package containment

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeMinABI(t *testing.T) {
	for in, want := range map[int]int{0: DefaultABI, 6: 6, 8: 8, 9: 9, 10: 10} {
		r, err := Normalize(&Request{Kind: KindLandlock, MinABI: in})
		if err != nil {
			t.Fatalf("MinABI %d: %v", in, err)
		}
		if r.MinABI != want {
			t.Errorf("MinABI %d normalized to %d, want %d", in, r.MinABI, want)
		}
	}
	for _, in := range []int{-1, 1, 5} {
		if _, err := Normalize(&Request{Kind: KindLandlock, MinABI: in}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("MinABI %d: %v, want ErrInvalidRequest", in, err)
		}
	}
	if DefaultABI != 9 || LowestABI != 6 {
		t.Error("the default must stay at RESOLVE_UNIX (ABI 9) and the floor at the IPC scopes (ABI 6)")
	}
	if MinimumABI != 9 {
		t.Error("MinimumABI is a released constant: its value must not change")
	}
}

func sampleApplied() *Applied {
	return &Applied{
		SchemaVersion: SchemaVersion, Kind: KindLandlock, ABI: 9, RequiredABI: 9,
		Profile: "claude-code@2.1.270", ProfileVersion: 1,
		HandledFS:       []string{"execute", "resolve_unix"},
		Grants:          []Grant{{Path: "/work", Access: "rw", Rights: []string{"write_file"}, Source: SourceWorkingDir}},
		TCP:             TCP{Mode: "unrestricted", Bind: "unrestricted"},
		PathnameSockets: PathnameSocketsDenied,
		Scopes:          []string{"abstract_unix_socket", "signal"},
		State:           State{Mode: "private", Home: "/s/home", Tmp: "/s/tmp"},
		Env:             []string{"HOME"},
	}
}

// A policy without the socket layer serializes, and so fingerprints, exactly
// as it did before the layer existed.
func TestFingerprintWithoutAppArmorUnchanged(t *testing.T) {
	a := sampleApplied()
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), "apparmor") {
		t.Fatalf("an Applied without the socket layer serializes an apparmor key: %s", b)
	}
	before := Fingerprint(a)
	a.AppArmor = &AppArmorLayer{Profile: "harness-wrapper-contain", Roots: []string{"/work"}}
	a.PathnameSockets = PathnameSocketsDeniedOutsideRoots
	if Fingerprint(a) == before {
		t.Fatal("the socket layer does not change the fingerprint")
	}
	a2 := a.Clone()
	a2.AppArmor.Roots = append(a2.AppArmor.Roots, "/other")
	if Fingerprint(a2) == Fingerprint(a) {
		t.Fatal("the socket layer's roots do not change the fingerprint")
	}
}

func TestCloneAppArmor(t *testing.T) {
	a := sampleApplied()
	a.AppArmor = &AppArmorLayer{Profile: "p", Roots: []string{"/r"}}
	c := a.Clone()
	c.AppArmor.Roots[0] = "/changed"
	c.AppArmor.Profile = "q"
	if a.AppArmor.Roots[0] != "/r" || a.AppArmor.Profile != "p" {
		t.Fatal("Clone shares the socket layer with the original")
	}
}
