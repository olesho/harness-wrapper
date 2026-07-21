package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
)

// The shared, cross-language model-picker conformance corpus. It is vendored
// byte-identically into harness-wrapper (canonical) and meta-harness; the two
// repos are in sync iff their test/corpus/models/MANIFEST.sha256 match.
// meta-harness runs the IDENTICAL corpus through its own parseModelPicker. See
// test/corpus/models/README.md and scripts/sync-models-corpus.sh.
//
// PATH DEPTH: this test lives in pkg/discovery/models (three levels below the
// repo root), so the corpus root is "../../../test/corpus/models" — one level
// deeper than pkg/chat/auth_corpus_test.go's "../../test/corpus/auth".
const modelsCorpusRoot = "../../../test/corpus/models"

type modelsCorpusMeta struct {
	Harness       string `json:"harness"`
	BinaryVersion string `json:"binary_version"`
	RecordedAt    string `json:"recorded_at"`
	Cols          int    `json:"cols"`
	Rows          int    `json:"rows"`
	Notes         string `json:"notes"`
	Expected      []Info `json:"expected"`
}

type modelsCorpusCase struct {
	name     string // <harness>/<case>, forward-slashed
	meta     modelsCorpusMeta
	raw      []byte // recorded terminal byte stream (bytes.raw)
	expected string // canonical rendered snapshot (expected.txt)
}

func loadModelsCorpus(t *testing.T) []modelsCorpusCase {
	t.Helper()
	var cases []modelsCorpusCase
	err := filepath.WalkDir(modelsCorpusRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "meta.json" {
			return nil
		}
		dir := filepath.Dir(path)
		mb, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var m modelsCorpusMeta
		if err := json.Unmarshal(mb, &m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "bytes.raw"))
		if err != nil {
			return err
		}
		exp, err := os.ReadFile(filepath.Join(dir, "expected.txt"))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(modelsCorpusRoot, dir)
		cases = append(cases, modelsCorpusCase{
			name:     filepath.ToSlash(rel),
			meta:     m,
			raw:      raw,
			expected: string(exp),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].name < cases[j].name })
	return cases
}

// TestModelsCorpusConformance is the offline half of the cross-language
// contract: for every recorded picker, render bytes.raw through pkg/screen and
// assert (1) the render byte-matches the pinned expected.txt (so a screen-render
// drift is caught), and (2) ParseModelPicker(text, harness) deep-equals the
// canonical meta.json.expected (so a parser drift is caught). The Go and TS
// implementations must agree on every fixture.
func TestModelsCorpusConformance(t *testing.T) {
	cases := loadModelsCorpus(t)
	if len(cases) == 0 {
		t.Fatalf("no model corpus cases found under %s", modelsCorpusRoot)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			scr := screen.New(c.meta.Cols, c.meta.Rows)
			if _, err := scr.Write(c.raw); err != nil {
				t.Fatalf("screen write: %v", err)
			}
			text := scr.Snapshot().Text
			if text != c.expected {
				t.Errorf("render(bytes.raw) != expected.txt\n--- got ---\n%s\n--- want ---\n%s", text, c.expected)
			}
			got := ParseModelPicker(text, c.meta.Harness)
			if !reflect.DeepEqual(got, c.meta.Expected) {
				t.Errorf("ParseModelPicker(%q) mismatch\n--- got ---\n%#v\n--- want ---\n%#v", c.meta.Harness, got, c.meta.Expected)
			}
		})
	}
}

// TestModelsCorpusManifest asserts MANIFEST.sha256 is current — the same drift
// guard scripts/sync-models-corpus.sh --check enforces in CI. The line format
// (lowercase hex, two spaces, forward-slashed path, sorted by path, trailing
// newline) must byte-match the generator and the TS test.
func TestModelsCorpusManifest(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(modelsCorpusRoot, "MANIFEST.sha256"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	type ent struct{ rel, hash string }
	var ents []ent
	err = filepath.WalkDir(modelsCorpusRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() == "MANIFEST.sha256" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		rel, _ := filepath.Rel(modelsCorpusRoot, path)
		ents = append(ents, ent{rel: filepath.ToSlash(rel), hash: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		t.Fatalf("hash corpus: %v", err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].rel < ents[j].rel })
	var b strings.Builder
	for _, e := range ents {
		fmt.Fprintf(&b, "%s  %s\n", e.hash, e.rel)
	}
	if b.String() != string(want) {
		t.Errorf("MANIFEST.sha256 is stale; run scripts/sync-models-corpus.sh and commit.\n--- computed ---\n%s\n--- committed ---\n%s", b.String(), string(want))
	}
}
