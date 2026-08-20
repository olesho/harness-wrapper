package profile

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func validManifest() Manifest {
	return Manifest{
		Files:          []string{"CLAUDE.md", "settings.json", "skills/x.md"},
		Fingerprint:    goldenSum,
		HarnessVersion: "2.1.235 (Claude Code)",
	}
}

func TestManifestRoundTrip(t *testing.T) {
	want := validManifest()
	data, err := want.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.HasSuffix(data, []byte("\n")) {
		t.Error("Marshal output has no trailing newline")
	}
	if !json.Valid(data) {
		t.Fatalf("Marshal produced invalid JSON: %s", data)
	}
	got, err := ParseManifest(data)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the manifest:\n got %+v\nwant %+v", got, want)
	}
	// Marshal is byte-stable, so rewriting an unchanged profile is a no-op.
	again, err := got.Marshal()
	if err != nil {
		t.Fatalf("Marshal again: %v", err)
	}
	if !bytes.Equal(data, again) {
		t.Errorf("Marshal is not byte-stable:\n%s\nvs\n%s", data, again)
	}
}

func TestManifestWireFieldNames(t *testing.T) {
	data, err := validManifest().Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The frozen wire contract: other implementations read these exact keys.
	for _, key := range []string{"files", "fingerprint", "harness_version"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("manifest JSON is missing the %q key", key)
		}
	}
	if len(raw) != 3 {
		t.Errorf("manifest JSON has %d keys, want exactly 3: %v", len(raw), raw)
	}
	if ManifestName != ".manifest.json" {
		t.Errorf("ManifestName = %q, want %q", ManifestName, ".manifest.json")
	}
}

func TestParseManifestRejectsMalformedJSON(t *testing.T) {
	for name, data := range map[string]string{
		"truncated": `{"files": ["a.txt"], "fingerprint": "`,
		"not json":  "not json at all",
		"wrong type": `{"files": "a.txt", "fingerprint": "` + goldenSum +
			`", "harness_version": "2.1.235"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(data)); !errors.Is(err, ErrManifestInvalid) {
				t.Errorf("err = %v, want ErrManifestInvalid", err)
			}
		})
	}
}

func TestParseManifestToleratesUnknownFields(t *testing.T) {
	data := `{"files": [], "fingerprint": "` + emptySum +
		`", "harness_version": "2.1.235", "future_field": 1}`
	if _, err := ParseManifest([]byte(data)); err != nil {
		t.Errorf("unknown field rejected: %v", err)
	}
}

func TestManifestValidate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Manifest)
		ok   bool
	}{
		{"valid", func(*Manifest) {}, true},
		{"empty file list", func(m *Manifest) { m.Files = nil }, true},
		{"unsorted", func(m *Manifest) { m.Files = []string{"b.md", "a.md"} }, false},
		{"duplicate", func(m *Manifest) { m.Files = []string{"a.md", "a.md"} }, false},
		{"absolute", func(m *Manifest) { m.Files = []string{"/etc/passwd"} }, false},
		{"dot dot", func(m *Manifest) { m.Files = []string{"../secrets"} }, false},
		{"dot dot nested", func(m *Manifest) { m.Files = []string{"skills/../../x"} }, false},
		{"backslash", func(m *Manifest) { m.Files = []string{`skills\x.md`} }, false},
		{"unclean", func(m *Manifest) { m.Files = []string{"./a.md"} }, false},
		{"empty entry", func(m *Manifest) { m.Files = []string{""} }, false},
		{"short fingerprint", func(m *Manifest) { m.Fingerprint = "abc" }, false},
		{"uppercase fingerprint", func(m *Manifest) {
			m.Fingerprint = "27FA969635CAF3DC34026424A3BFAC5B066D7B20C8E96DCC2CFC991C0E4BD99B"
		}, false},
		{"non-hex fingerprint", func(m *Manifest) {
			m.Fingerprint = "zz" + goldenSum[2:]
		}, false},
		{"empty fingerprint", func(m *Manifest) { m.Fingerprint = "" }, false},
		{"empty harness version", func(m *Manifest) { m.HarnessVersion = "" }, false},
		{"blank harness version", func(m *Manifest) { m.HarnessVersion = "   " }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := validManifest()
			tc.mut(&m)
			err := m.Validate()
			if tc.ok && err != nil {
				t.Fatalf("Validate: unexpected error %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("Validate accepted an invalid manifest")
				}
				if !errors.Is(err, ErrManifestInvalid) {
					t.Errorf("err = %v, want ErrManifestInvalid", err)
				}
			}
		})
	}
}
