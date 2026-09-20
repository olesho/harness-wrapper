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

	"github.com/olesho/harness-wrapper/pkg/transcript"
	transcriptcc "github.com/olesho/harness-wrapper/pkg/transcript/claudecode"
)

// The API-error tag corpus: real synthetic-error lines from Claude Code
// transcripts, replayed through the parser and the mapping. It is vendored in
// the same shape as the auth corpus so meta-harness can mirror it
// byte-identically; see test/corpus/apierror/README.md and
// scripts/sync-apierror-corpus.sh.
const apiErrorCorpusRoot = "../../test/corpus/apierror"

type apiErrorCorpusMeta struct {
	Harness            string   `json:"harness"`
	Tag                string   `json:"tag"`
	Verdict            string   `json:"verdict"` // wall | errored | none
	Code               string   `json:"code"`
	Reason             string   `json:"reason"`
	ScreenOnly         string   `json:"screenOnly"`
	ScreenAuthRequired bool     `json:"screenAuthRequired"`
	Occurrences        int      `json:"occurrences"`
	Versions           []string `json:"versions"`
}

type apiErrorCorpusCase struct {
	name string // <harness>/<case>
	meta apiErrorCorpusMeta
	line []byte
}

func loadAPIErrorCorpus(t *testing.T) []apiErrorCorpusCase {
	t.Helper()
	var cases []apiErrorCorpusCase
	err := filepath.WalkDir(apiErrorCorpusRoot, func(path string, d fs.DirEntry, err error) error {
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
		var m apiErrorCorpusMeta
		if err := json.Unmarshal(mb, &m); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		lb, err := os.ReadFile(filepath.Join(dir, "line.jsonl"))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(apiErrorCorpusRoot, dir)
		cases = append(cases, apiErrorCorpusCase{name: filepath.ToSlash(rel), meta: m, line: lb})
		return nil
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].name < cases[j].name })
	return cases
}

// TestAPIErrorCorpusParsesTheTag asserts the parser recovers the harness's own
// verdict from every real line. This is the half that used to be missing
// outright: both fields were dropped at transcript.Line, so nothing downstream
// could have read them however hard it tried.
func TestAPIErrorCorpusParsesTheTag(t *testing.T) {
	cases := loadAPIErrorCorpus(t)
	if len(cases) == 0 {
		t.Fatalf("no api-error corpus cases found under %s", apiErrorCorpusRoot)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs, err := transcriptcc.Events(c.line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(evs) != 1 {
				t.Fatalf("got %d events, want 1", len(evs))
			}
			if evs[0].APIError != c.meta.Tag {
				t.Errorf("APIError = %q, want %q", evs[0].APIError, c.meta.Tag)
			}
			if evs[0].Role != transcript.RoleAssistant {
				t.Errorf("Role = %q, want assistant", evs[0].Role)
			}
			if strings.TrimSpace(evs[0].Text) == "" {
				t.Error("Text is empty: the rendered error is what makes this look like a reply")
			}
			// The projection chat reads must carry it too, or the mapping sees
			// an ordinary reply.
			tturns := transcript.TurnsFromEvents(evs)
			if len(tturns) != 1 || tturns[0].APIError != c.meta.Tag {
				t.Errorf("TurnsFromEvents dropped the tag: %+v", tturns)
			}
		})
	}
}

// TestAPIErrorCorpusMapping asserts each real tag maps to the verdict its
// meta.json records, at watermark 0 (everything in the fixture belongs to this
// turn).
func TestAPIErrorCorpusMapping(t *testing.T) {
	for _, c := range loadAPIErrorCorpus(t) {
		t.Run(c.name, func(t *testing.T) {
			evs, err := transcriptcc.Events(c.line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			v, ok := apiErrorVerdictFrom(transcript.TurnsFromEvents(evs), 0)
			switch c.meta.Verdict {
			case "none":
				if ok {
					t.Fatalf("got verdict %+v, want none for tag %q", v, c.meta.Tag)
				}
				return
			case "wall", "errored":
				if !ok {
					t.Fatalf("no verdict for tag %q, want %s", c.meta.Tag, c.meta.Verdict)
				}
			default:
				t.Fatalf("meta.json verdict %q is not one of wall|errored|none", c.meta.Verdict)
			}
			if string(v.code) != c.meta.Code {
				t.Errorf("code = %q, want %q", v.code, c.meta.Code)
			}
			wantReason := map[string]string{
				"":                   "",
				"ReasonAuthRequired": ReasonAuthRequired,
				"ReasonUsageLimited": ReasonUsageLimited,
				"ReasonBillingWall":  ReasonBillingWall,
			}[c.meta.Reason]
			if v.reason != wantReason {
				t.Errorf("reason = %q, want %q (%s)", v.reason, wantReason, c.meta.Reason)
			}
			// Whatever the verdict, the harness's own words ride along.
			got := v.turnReason("claude-code")
			if !strings.Contains(got, c.meta.Tag) {
				t.Errorf("turn reason %q does not name the tag %q", got, c.meta.Tag)
			}
		})
	}
}

// TestAPIErrorCorpusOutcomeDelta pins what the tag CHANGES, per fixture.
//
// It is not a restatement of the mapping test: it runs today's screen-only
// path — the two relabels — over the same rendered text and asserts it yields
// what meta.json recorded. Every fixture records "complete", i.e. a false
// SUCCESS whose reply is the error message. If a future wrapper teaches the
// screen relabels to catch one of these, this test fails and the corpus has to
// be re-measured rather than quietly agreeing with itself.
func TestAPIErrorCorpusOutcomeDelta(t *testing.T) {
	for _, c := range loadAPIErrorCorpus(t) {
		t.Run(c.name, func(t *testing.T) {
			evs, err := transcriptcc.Events(c.line)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			rendered := evs[0].Text

			if got := authRequired(c.meta.Harness, rendered); got != c.meta.ScreenAuthRequired {
				t.Errorf("authRequired = %v, want %v (meta.screenAuthRequired)", got, c.meta.ScreenAuthRequired)
			}

			screenOnly := "complete"
			if _, ok := usageLimitMessage(c.meta.Harness, rendered); ok {
				screenOnly = "usage_limited"
			} else if c.meta.ScreenAuthRequired && strings.TrimSpace(rendered) == "" {
				// authRelabel's empty-gate. It cannot fire here — claude renders
				// the error as a bubble, so the extraction is non-empty — which
				// is exactly why these complete today.
				screenOnly = "auth_required"
			}
			if screenOnly != c.meta.ScreenOnly {
				t.Errorf("screen-only verdict = %q, want %q (the recorded delta is stale)", screenOnly, c.meta.ScreenOnly)
			}
		})
	}
}

// TestAPIErrorCorpusCoverage guards the corpus against silently shrinking, and
// records what it does NOT cover.
func TestAPIErrorCorpusCoverage(t *testing.T) {
	cases := loadAPIErrorCorpus(t)
	tags := map[string]int{}
	total := 0
	for _, c := range cases {
		tags[c.meta.Tag]++
		total += c.meta.Occurrences
	}
	if len(cases) < 19 || total < 99 {
		t.Errorf("corpus shrank: %d fixtures / %d occurrences, want >= 19 / 99", len(cases), total)
	}
	for _, want := range []string{"authentication_failed", "server_error", "unknown"} {
		if tags[want] == 0 {
			t.Errorf("no fixture for tag %q", want)
		}
	}
	// No billing capture exists on any machine this was built on — see the
	// corpus README's "Known gap". The mapping is covered synthetically here so
	// the absence is explicit rather than an untested arm.
	if _, ok := apiErrorClasses["billing_error"]; !ok {
		t.Fatal("billing_error is unmapped")
	}
	if tags["billing_error"] != 0 {
		t.Log("a real billing_error fixture now exists — update the corpus README's Known gap section")
	}
}

// TestAPIErrorCorpusManifest asserts MANIFEST.sha256 is current — the same
// drift guard scripts/sync-apierror-corpus.sh --check enforces in CI, so a
// fixture edit that forgets to re-sync fails the unit gate too, in BOTH repos.
func TestAPIErrorCorpusManifest(t *testing.T) {
	want, err := os.ReadFile(filepath.Join(apiErrorCorpusRoot, "MANIFEST.sha256"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	type ent struct{ rel, hash string }
	var ents []ent
	err = filepath.WalkDir(apiErrorCorpusRoot, func(path string, d fs.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(apiErrorCorpusRoot, path)
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
		t.Errorf("MANIFEST.sha256 is stale; run scripts/sync-apierror-corpus.sh and commit.\n--- computed ---\n%s\n--- committed ---\n%s", b.String(), string(want))
	}
}
