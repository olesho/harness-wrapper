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

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// The permission-mode conformance corpus: captured footer screens paired with
// the posture the adapter must read off them. harness-wrapper is canonical for
// shared corpora — screens are captured HERE and mirrored OUT to meta-harness
// with a byte-equal MANIFEST.sha256 as the sync invariant (see
// test/corpus/permission-mode/README.md and
// scripts/sync-permission-mode-corpus.sh).
//
// Its meta shape is NOT authCorpusMeta's — {harness, binary_version,
// recorded_at, cols, rows, mode, notes} vs {harness, authRequired, state, ...} —
// so this loader is written fresh rather than reusing loadAuthCorpus.
const permModeCorpusRoot = "../../test/corpus/permission-mode"

// permModeCorpusMeta is one case's meta.json. `mode` is the expected answer:
// for claude-code a canonical rung (plan|manual|ask|auto|bypass), for codex a
// COLLABORATION-axis value (plan|default) — see turns.PermissionModeDetector,
// which deliberately reports different axes per harness.
type permModeCorpusMeta struct {
	Harness       string `json:"harness"`
	BinaryVersion string `json:"binary_version"`
	RecordedAt    string `json:"recorded_at"`
	Cols          int    `json:"cols"`
	Rows          int    `json:"rows"`
	Mode          string `json:"mode"`

	// PendingParser, when non-empty, marks a capture whose OBSERVED truth
	// (meta.mode) the shipped parser does not yet report, and says why. It
	// exists so a real capture of a real gap can be recorded here without
	// either fabricating a passing expectation or turning the tree red for
	// work that belongs to the parser ticket.
	//
	// It is deliberately self-clearing: TestPermissionModeCorpusConformance
	// FAILS a pending case that now agrees with meta.mode, so the field
	// cannot silently outlive the fix it names.
	PendingParser string `json:"pending_parser,omitempty"`

	Notes string `json:"notes"`
}

type permModeCorpusCase struct {
	name   string // <harness>/<mode>, forward-slashed
	meta   permModeCorpusMeta
	screen string // screen.txt, verbatim
}

// loadPermissionModeCorpus walks the corpus tree and returns one case per
// meta.json, sorted by name. screen.txt is required next to every meta.json;
// bytes.raw is optional (present only when the capture rig produced one) and is
// not read here — the rendered screen is the fixture the detector consumes.
func loadPermissionModeCorpus(t *testing.T) []permModeCorpusCase {
	t.Helper()
	var cases []permModeCorpusCase
	err := filepath.WalkDir(permModeCorpusRoot, func(path string, d fs.DirEntry, err error) error {
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
		var m permModeCorpusMeta
		if err := json.Unmarshal(mb, &m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		sb, err := os.ReadFile(filepath.Join(dir, "screen.txt"))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(permModeCorpusRoot, dir)
		cases = append(cases, permModeCorpusCase{name: filepath.ToSlash(rel), meta: m, screen: string(sb)})
		return nil
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].name < cases[j].name })
	return cases
}

// TestPermissionModeCorpusConformance asserts the adapter's PermissionMode()
// answer matches meta.mode for every captured screen.
//
// It drives the REAL package API — resolveAdapter() then a type assertion to
// turns.PermissionModeDetector — because the per-harness footer parsers are
// unexported, the same way TestAuthCorpusConformance drives its fixtures
// through authRequired(). A capture whose meta.mode disagrees with the shipped
// parser is the exact drift this corpus exists to catch.
//
// Screen-render drift is NOT re-guarded here: the screenbench tree
// (test/corpus/claude-code/*) and the models corpus already pin how claude's
// bytes render through pkg/screen. This corpus pins only the footer→posture
// mapping.
//
// A case carrying meta.pending_parser is inverted: the parser is asserted to
// still DISAGREE with the capture, so the recorded gap stays visible and the
// marker self-clears the moment the parser ticket lands its fix.
func TestPermissionModeCorpusConformance(t *testing.T) {
	cases := loadPermissionModeCorpus(t)
	if len(cases) == 0 {
		t.Skip("no permission-mode corpus cases on disk yet — see test/corpus/permission-mode/README.md " +
			"for the open capture list; captures require a live claude-code/codex rig and are never fabricated")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			adapter, err := resolveAdapter(c.meta.Harness)
			if err != nil {
				t.Fatalf("resolveAdapter(%q): %v", c.meta.Harness, err)
			}
			det, ok := adapter.(turns.PermissionModeDetector)
			if !ok {
				t.Fatalf("%s adapter does not implement turns.PermissionModeDetector", c.meta.Harness)
			}
			snap := screen.Snapshot{Text: c.screen, Cols: c.meta.Cols, Rows: c.meta.Rows}
			got, readable := det.PermissionMode(snap)
			if c.meta.PendingParser != "" {
				if readable && got == c.meta.Mode {
					t.Fatalf("%s: pending_parser is stale — PermissionMode now reports %q, matching meta.mode. "+
						"Drop pending_parser from meta.json so this case starts enforcing.\npending_parser: %s",
						c.name, got, c.meta.PendingParser)
				}
				t.Logf("%s: known parser gap, want %q, parser reports %q (readable=%v)\n%s",
					c.name, c.meta.Mode, got, readable, c.meta.PendingParser)
				return
			}
			if !readable {
				t.Fatalf("PermissionMode(%s) reported no readable signal, want %q\n--- screen ---\n%s",
					c.name, c.meta.Mode, c.screen)
			}
			if got != c.meta.Mode {
				t.Errorf("PermissionMode(%s) = %q, want %q\n--- screen ---\n%s",
					c.name, got, c.meta.Mode, c.screen)
			}
		})
	}
}

// TestPermissionModeCorpusManifest asserts MANIFEST.sha256 is current — the same
// drift guard scripts/sync-permission-mode-corpus.sh --check enforces in CI, so a
// fixture edit that forgets to re-sync fails the unit gate too, in BOTH repos.
// The line format (lowercase hex, two spaces, forward-slashed path, sorted by
// path, trailing newline) must byte-match the generator.
func TestPermissionModeCorpusManifest(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(permModeCorpusRoot, "MANIFEST.sha256"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	type ent struct{ rel, hash string }
	var ents []ent
	err = filepath.WalkDir(permModeCorpusRoot, func(path string, d fs.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(permModeCorpusRoot, path)
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
		t.Errorf("MANIFEST.sha256 is stale; run scripts/sync-permission-mode-corpus.sh and commit.\n--- computed ---\n%s\n--- committed ---\n%s", b.String(), string(want))
	}
}
