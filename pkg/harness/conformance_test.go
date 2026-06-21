package harness_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/versions"
)

// Layer 4 of the test pyramid (see TESTING.md): live conformance against the
// REAL installed harness binaries. Unlike the hermetic layers, this is the only
// thing that catches inward-contract drift (a new claude-code/codex version that
// breaks our screen scraping) BEFORE users do. It is gated behind
// HARNESS_WRAPPER_CONFORMANCE=1 and additionally skips any harness whose binary
// is not on PATH, so it is safe in normal runs and meant for a nightly job.
//
//	HARNESS_WRAPPER_CONFORMANCE=1 go test ./pkg/harness/ -run Conformance -v

const conformanceEnv = "HARNESS_WRAPPER_CONFORMANCE"

var semverRE = regexp.MustCompile(`\d+\.\d+\.\d+`)

// conformanceHarness binds a wrapper harness name to its versions.json key and
// the args needed to drive one real turn non-interactively.
type conformanceHarness struct {
	wrapper     string   // RunTurn TurnConfig.Harness
	versionsKey string   // key in versions.json
	args        []string // interactive args for a real turn
}

var conformanceHarnesses = []conformanceHarness{
	{wrapper: "claude", versionsKey: "claude-code", args: []string{"--dangerously-skip-permissions"}},
	{wrapper: "codex", versionsKey: "codex", args: nil},
}

func requireConformance(t *testing.T) {
	t.Helper()
	if os.Getenv(conformanceEnv) != "1" {
		t.Skipf("set %s=1 to run live conformance against installed harness binaries", conformanceEnv)
	}
}

// TestConformance_VersionDrift compares each installed harness's reported
// version against the pin in versions.json. A mismatch means our adapters are
// verified against a different version than what is installed — the early
// warning that screen-scraping may have drifted. Harnesses with no pin or no
// installed binary are skipped.
func TestConformance_VersionDrift(t *testing.T) {
	requireConformance(t)

	all, err := versions.All()
	if err != nil {
		t.Fatalf("versions.All: %v", err)
	}

	probed := 0
	for name, e := range all {
		if e.Pinned == "" || e.Binary == "" {
			continue
		}
		bin, err := exec.LookPath(e.Binary)
		if err != nil {
			t.Logf("%s: binary %q not on PATH — skipping", name, e.Binary)
			continue
		}
		probed++
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
			if err != nil {
				t.Fatalf("%s --version: %v\n%s", bin, err, out)
			}
			got := semverRE.FindString(string(out))
			if got == "" {
				t.Fatalf("could not parse a version from %q --version output: %q", bin, string(out))
			}
			if got != e.Pinned {
				t.Errorf("VERSION DRIFT: %s installed=%s pinned=%s (verified %s). "+
					"Re-verify the adapter against %s and re-bake the corpus, then bump versions.json.",
					name, got, e.Pinned, e.VerifiedAt, got)
			} else {
				t.Logf("%s: installed=%s matches pin", name, got)
			}
		})
	}
	if probed == 0 {
		t.Skip("no pinned harness binaries installed — nothing to check")
	}
}

// TestConformance_SentinelRoundTrip drives one real turn through each installed
// harness and asserts the version-independent invariant: a unique sentinel in
// the prompt survives verbatim into the captured reply. This is the single
// highest-value live check — it catches turn-boundary truncation and reply
// extraction drift regardless of glyph changes.
func TestConformance_SentinelRoundTrip(t *testing.T) {
	requireConformance(t)

	all, err := versions.All()
	if err != nil {
		t.Fatalf("versions.All: %v", err)
	}

	ran := 0
	for _, h := range conformanceHarnesses {
		e, ok := all[h.versionsKey]
		if !ok || e.Binary == "" {
			continue
		}
		bin, err := exec.LookPath(e.Binary)
		if err != nil {
			t.Logf("%s: binary %q not on PATH — skipping", h.wrapper, e.Binary)
			continue
		}
		ran++
		t.Run(h.wrapper, func(t *testing.T) {
			sentinel := fmt.Sprintf("CONFORMANCE-%d", time.Now().UnixNano())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()

			var out bytes.Buffer
			res, err := harness.RunTurn(ctx, harness.TurnConfig{
				Harness:       h.wrapper,
				BinaryPath:    bin,
				Args:          h.args,
				Prompt:        "Reply with exactly: " + sentinel,
				ExitAfterTurn: true,
				Output:        &out,
			})
			if err != nil {
				t.Fatalf("RunTurn(%s): %v\noutput:\n%s", h.wrapper, err, out.String())
			}
			if res.Turn.State != chat.TurnStateComplete {
				t.Fatalf("%s: turn state = %q (reason %q), want complete\noutput:\n%s",
					h.wrapper, res.Turn.State, res.Turn.Reason, out.String())
			}
			if !strings.Contains(res.Turn.Text, sentinel) && !strings.Contains(out.String(), sentinel) {
				t.Fatalf("%s: sentinel %q did not round-trip (turn truncated or extraction drifted)\nturn text:\n%s\noutput:\n%s",
					h.wrapper, sentinel, res.Turn.Text, out.String())
			}
			t.Logf("%s: sentinel round-trip OK", h.wrapper)
		})
	}
	if ran == 0 {
		t.Skip("no conformance harness binaries installed — nothing to run")
	}
}
