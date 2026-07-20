package chat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The shared, cross-language auth conformance corpus. It is vendored
// byte-identically into harness-wrapper (canonical) and meta-harness; the two
// repos are in sync iff their test/corpus/auth/MANIFEST.sha256 match. meta-harness
// runs the IDENTICAL corpus through its own authRequired in
// test/chat/auth_corpus.test.ts. See test/corpus/auth/README.md and
// scripts/sync-auth-corpus.sh.
const authCorpusRoot = "../../test/corpus/auth"

type authCorpusMeta struct {
	Harness      string `json:"harness"`
	AuthRequired bool   `json:"authRequired"`
	State        string `json:"state"`
}

type authCorpusCase struct {
	name   string // <harness>/<case>, forward-slashed
	meta   authCorpusMeta
	screen string
}

func loadAuthCorpus(t *testing.T) []authCorpusCase {
	t.Helper()
	var cases []authCorpusCase
	err := filepath.WalkDir(authCorpusRoot, func(path string, d fs.DirEntry, err error) error {
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
		var m authCorpusMeta
		if err := json.Unmarshal(mb, &m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		sb, err := os.ReadFile(filepath.Join(dir, "screen.txt"))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(authCorpusRoot, dir)
		cases = append(cases, authCorpusCase{name: filepath.ToSlash(rel), meta: m, screen: string(sb)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].name < cases[j].name })
	return cases
}

// TestAuthCorpusConformance asserts authRequired() agrees with every captured
// screen's expected verdict. This is the offline half of the cross-language
// contract that keeps the Go and TS logged-out detection from silently diverging.
func TestAuthCorpusConformance(t *testing.T) {
	cases := loadAuthCorpus(t)
	if len(cases) == 0 {
		t.Fatalf("no auth corpus cases found under %s", authCorpusRoot)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := authRequired(c.meta.Harness, c.screen)
			if got != c.meta.AuthRequired {
				t.Errorf("authRequired(%q, %s) = %v, want %v\n--- screen ---\n%s",
					c.meta.Harness, c.name, got, c.meta.AuthRequired, c.screen)
			}
		})
	}
}

// TestAuthCorpusManifest asserts MANIFEST.sha256 is current — the same drift
// guard scripts/sync-auth-corpus.sh --check enforces in CI, so a fixture edit
// that forgets to re-sync fails the unit gate too, in BOTH repos. The line format
// (lowercase hex, two spaces, forward-slashed path, sorted by path, trailing
// newline) must byte-match the generator and the TS test.
func TestAuthCorpusManifest(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(authCorpusRoot, "MANIFEST.sha256"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	type ent struct{ rel, hash string }
	var ents []ent
	err = filepath.WalkDir(authCorpusRoot, func(path string, d fs.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(authCorpusRoot, path)
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
		t.Errorf("MANIFEST.sha256 is stale; run scripts/sync-auth-corpus.sh and commit.\n--- computed ---\n%s\n--- committed ---\n%s", b.String(), string(want))
	}
}
