package harness_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/chat"
	"github.com/olesho/harness-wrapper/pkg/chat/memstore"
	"github.com/olesho/harness-wrapper/pkg/harness"
	"github.com/olesho/harness-wrapper/pkg/turns"
	"github.com/olesho/harness-wrapper/pkg/versions"
	"github.com/olesho/harness-wrapper/pkg/wrapper"
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
			checkVersionDrift(t, name, bin, e.Pinned, e.VerifiedAt)
		})
	}
	if probed == 0 {
		t.Skip("no pinned harness binaries installed — nothing to check")
	}
}

// checkVersionDrift probes bin's reported version and compares it against the
// pinned value, erroring on drift.
func checkVersionDrift(t *testing.T, name, bin, pinned, verifiedAt string) {
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
	if got != pinned {
		t.Errorf("VERSION DRIFT: %s installed=%s pinned=%s (verified %s). "+
			"Re-verify the adapter against %s and re-bake the corpus, then bump versions.json.",
			name, got, pinned, verifiedAt, got)
		return
	}
	t.Logf("%s: installed=%s matches pin", name, got)
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
			checkSentinelRoundTrip(t, h, bin)
		})
	}
	if ran == 0 {
		t.Skip("no conformance harness binaries installed — nothing to run")
	}
}

// checkSentinelRoundTrip drives one real turn through bin and asserts a unique
// sentinel survives verbatim into the captured reply.
func checkSentinelRoundTrip(t *testing.T, h conformanceHarness, bin string) {
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
}

// --- Live permission-footer conformance (claude-code) -----------------------
//
// Why this exists at layer 4 and nowhere else. The LAUNCH path degrades loudly:
// an unknown --permission-mode value makes claude exit and the harness surfaces
// it. The DETECTION path degrades SILENTLY — a renamed footer word makes
// permissionModeFromFooter return ("", false), or worse reports a stale rung,
// with no error anywhere. Corpus fixtures cannot catch that: they are frozen
// bytes and keep passing long after the real CLI has moved on. Claude has
// already flipped its DEFAULT permission mode to Manual once.
//
// So this launches the REAL binary once per canonical rung and asserts that the
// SHIPPED parser — reached through turns.PermissionModeDetector, never a local
// copy of permissionModeRE — reads back the rung that was launched. No turn is
// ever sent: the footer paints at idle, so the whole check costs no tokens.
//
// DELIBERATE DIVERGENCE FROM THE TYPESCRIPT SIBLING SUITE (META-HARNESS-131).
// Both sides agree on the footer CORE strings ("plan mode", "manual mode",
// "accept edits", "auto mode", "bypass permissions"). This side additionally
// refuses to assert the footer SUFFIX per mode, because this repo's own
// fixtures disprove any mode-keyed framing: `auto` renders suffix-less
// (claudecode/busy_test.go:19,49-50, turncomplete_busy_test.go:28,47),
// `bypass permissions` has its whole tail replaced while busy
// (busy_test.go:16, turncomplete_busy_test.go:24) and while sub-agents run
// (busy_test.go:34, "· ↓ to manage"), and a suffix-less `manual` line is on
// disk twice (test/corpus/auth/claude-code/not-logged-in-{churned,brewed}).
// The suffix is context- and version-dependent, NOT mode-dependent; asserting
// it per mode would bake in a falsehood. It is logged here, never asserted.
// The TS side can adopt or reject that knowingly.
//
// A live run of this test against claude 2.1.218 confirms it beyond the
// fixtures: four rungs rendered "(shift+tab to cycle)" while `manual` rendered
// the bare "⏸ manual mode on" with no suffix at all.

// claudeFooterGlyphLineRE locates the rendered footer line for REPORTING ONLY —
// drift messages and the dontAsk probe log. It matches the glyph alone and
// carries no mode vocabulary on purpose, so it can still quote a footer whose
// words have drifted out from under the shipped parser. It is NEVER consulted
// for an assertion: that is turns.PermissionModeDetector's job, and routing the
// assertion through the shipped parser is the entire point of this test.
var claudeFooterGlyphLineRE = regexp.MustCompile(`(?m)^.*[⏸⏵].*$`)

// claudeFooterSuffixRE splits a footer line at its " on" token so the tail can
// be LOGGED. Reporting only — see the divergence note above.
var claudeFooterSuffixRE = regexp.MustCompile(`[^\S\r\n]on\b(.*)$`)

// claudeAuthWallRE mirrors the login/expiry + onboarding wall that pkg/chat's
// unexported authRequired matches (pkg/chat/ready.go:322 and its regex block at
// :224-238). Kept as a local copy because that helper is unexported; it must be
// updated in lockstep with it.
//
// This gate is a real correctness hole if skipped: a logged-OUT claude STILL
// paints a footer — test/corpus/auth/claude-code/not-logged-in-churned/screen.txt:15
// renders "⏸ manual mode on". Without the gate an unauthenticated run makes
// every rung read back `manual`, so the `manual` case passes while the other
// four fail confusingly — and a future default flip would mask real drift.
var claudeAuthWallRE = []*regexp.Regexp{
	regexp.MustCompile(`(?i)choose the text style`), // theme picker (onboarding)
	regexp.MustCompile(`(?i)select login method`),   // login-method screen
	regexp.MustCompile(`(?i)\brun /login\b`),
	regexp.MustCompile(`(?i)\bnot logged in\b`),
	regexp.MustCompile(`(?i)\binvalid api key\b`),
}

const (
	// footerPollTimeout bounds ONE launch. chat.Open does not wait for
	// readiness — the only waiter (pkg/chat/ready.go:23 waitReadyForSend) is
	// unexported and reachable only via Send, which is forbidden here — so each
	// launch polls ScreenSnapshot() on its own deadline.
	footerPollTimeout  = 45 * time.Second
	footerPollInterval = 250 * time.Millisecond
)

// claudeFooterExpectation is the core pattern the drift message quotes back.
// It is the human-readable rendering of permissionModeRE's closed alternation
// (pkg/turns/harness/claudecode/permmode.go:55).
const claudeFooterExpectation = "<⏸|⏵> <plan mode|manual mode|accept edits|auto mode|bypass permissions> on"

// footerProbe is one launch's observation.
type footerProbe struct {
	line     string // whole footer line as rendered (reporting only), "" if none
	suffix   string // the version-dependent tail after " on" (logged, never asserted)
	rung     string // what the SHIPPED detector read
	readable bool   // the detector's second return
}

// TestConformance_ClaudePermissionFooter launches the real claude binary once
// per canonical rung and asserts the shipped footer parser reads back the rung
// that was launched. See the block comment above for why.
//
// Read-only: no turn, no /permissions, no /model, no settings screen, and HOME
// is deliberately NOT sandboxed (claude's credentials live there; an isolated
// HOME yields an unauthenticated session and this test would only ever skip).
// Safety comes from being read-only, not from isolation.
func TestConformance_ClaudePermissionFooter(t *testing.T) {
	requireConformance(t)

	all, err := versions.All()
	if err != nil {
		t.Fatalf("versions.All: %v", err)
	}
	// versions.json key, not the conformanceHarnesses table: that table's
	// wrapper name is "claude", which chat.resolveAdapter has no case for —
	// passing it to chat.Options.Harness returns ErrUnknownHarness. The chat
	// layer wants the adapter name "claude-code" (only pkg/harness normalizes
	// "claude", via the unexported turnHarnessName in run_turn.go).
	e, ok := all["claude-code"]
	if !ok || e.Binary == "" {
		t.Skip("claude-code has no versions.json entry with a binary — nothing to launch")
	}
	bin, err := exec.LookPath(e.Binary)
	if err != nil {
		t.Skipf("claude-code: binary %q not on PATH — skipping", e.Binary)
	}
	// Deliberately independent of the versions.json pin: this check is about
	// what the INSTALLED binary paints, so pin skew is a log line, not a skip.
	t.Logf("probing %s (versions.json pin: %s)", bin, e.Pinned)

	rungs := wrapper.PermissionRungs()
	seen := make(map[string]string, len(rungs)) // rung launched -> rung detected

	for _, rung := range rungs {
		p := probeClaudeFooter(t, bin, rung)
		t.Logf("rung %-6s -> detector=%q readable=%t footer=%q suffix=%q",
			rung, p.rung, p.readable, p.line, p.suffix)

		// Recorded BEFORE the per-rung verdict, and regardless of whether it
		// matched. Recording only the matches would make the distinctness check
		// below unfireable: readings that each equal their own launched rung are
		// distinct by construction. The collapse this guards against — every rung
		// reading back the same value — shows up only in the raw readings.
		if p.readable {
			seen[rung] = p.rung
		}

		if !p.readable {
			t.Errorf("FOOTER DRIFT: launched claude with permission rung %q, but the shipped "+
				"parser could not read a permission mode off the rendered footer.\n"+
				"  footer line as rendered: %q\n"+
				"  expected core (suffix intentionally not asserted): %s\n"+
				"  fix the alternation in pkg/turns/harness/claudecode/permmode.go:55 (permissionModeRE) "+
				"— a DIFFERENT package from this test, which is exactly why this message names it.\n"+
				"  see docs/md/internal/versions-drift.md",
				rung, p.line, claudeFooterExpectation)
			continue
		}
		if p.rung != rung {
			t.Errorf("FOOTER DRIFT: launched claude with permission rung %q, but the shipped "+
				"parser read back %q.\n"+
				"  footer line as rendered: %q\n"+
				"  expected core (suffix intentionally not asserted): %s\n"+
				"  fix the alternation/translation in pkg/turns/harness/claudecode/permmode.go:55 "+
				"(permissionModeRE) and its permissionModeRungs map — a DIFFERENT package from this "+
				"test, which is exactly why this message names it.\n"+
				"  see docs/md/internal/versions-drift.md",
				rung, p.rung, p.line, claudeFooterExpectation)
		}
	}

	assertDistinctRungReadings(t, seen)
	probeClaudeDontAsk(t, bin)
}

// probeClaudeFooter launches claude once at mode and polls the rendered screen
// until the shipped detector can read a permission mode (or the deadline
// expires). The conversation is closed before this returns — including on
// t.Fatal/t.Skip, which unwind through the deferred Close — so a stalled rung
// never leaks a live claude process into the next iteration.
func probeClaudeFooter(t *testing.T, bin, mode string) footerProbe {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), footerPollTimeout+30*time.Second)
	defer cancel()

	conv, err := chat.Open(ctx, chat.Options{
		Harness:        "claude-code",
		BinaryPath:     bin,
		PermissionMode: mode,
		Store:          memstore.New(),
		// Bypass-class launches surface claude's "Bypass Permissions mode"
		// acceptance screen, which claudecode.DetectInput emits under Kind
		// trust_prompt with a "proceed"-aliased first option — and the footer
		// NEVER paints until it is answered. Same entry, same two-screen
		// reason, as pkg/oneshot/oneshot.go:203 (documented at :167-176); it
		// also covers the folder-trust dialog. WorkingDir is left at the
		// process CWD (the repo checkout, which claude already trusts) rather
		// than t.TempDir(), so folder trust is not an extra variable.
		InputPolicy: &chat.InputPolicy{ByKind: map[string]chat.Disposition{
			"trust_prompt": {Kind: chat.DispositionAnswer, OptionID: "proceed"},
		}},
	})
	if err != nil {
		t.Fatalf("chat.Open(claude-code, permission mode %q): %v", mode, err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer closeCancel()
		if err := conv.Close(closeCtx); err != nil {
			t.Logf("close (mode %q): %v", mode, err)
		}
	}()

	det, ok := conv.Adapter().(turns.PermissionModeDetector)
	if !ok {
		t.Fatalf("claude-code adapter %T does not implement turns.PermissionModeDetector — "+
			"the capability this test asserts through has been removed", conv.Adapter())
	}

	var p footerProbe
	deadline := time.Now().Add(footerPollTimeout)
	for {
		snap := conv.ScreenSnapshot()
		if wall := claudeAuthWall(snap.Text); wall != "" {
			// Skip, never fail: a logged-out claude still paints a footer, so
			// asserting here would report bogus drift (see claudeAuthWallRE).
			t.Skipf("claude is not logged in (screen shows %q) — log in with `claude` and re-run; "+
				"an unauthenticated session paints a footer for every rung and would make this "+
				"check lie rather than fail", wall)
		}
		p.line, p.suffix = claudeFooterLine(snap.Text)
		p.rung, p.readable = det.PermissionMode(snap)
		if p.readable {
			return p
		}
		if time.Now().After(deadline) {
			return p
		}
		time.Sleep(footerPollInterval)
	}
}

// probeClaudeDontAsk launches claude's NATIVE dontAsk mode. This is a PROBE,
// not an assertion: dontAsk has no canonical rung, permissionModeRE's
// alternation is closed on the five known words, so ("", false) is the expected
// reading and must NOT fail. The log is the capture the parser's open question
// needs.
//
// WHAT THIS PROBE ALREADY FOUND (claude 2.1.218, 2026-07-23): dontAsk DOES
// paint a genuinely distinct sixth footer —
//
//	⏵⏵ don't ask on (shift+tab to cycle)
//
// and the shipped parser reads it as ("", false), i.e. correctly "unknown"
// rather than a wrong rung — the closed alternation behaving exactly as
// permmode.go documents. Turning that into a sixth rung is a behavior change to
// a SHIPPED parser in a different package (a "don't ask" row in
// permissionModeRE + permissionModeRungs, a new canonical rung in
// wrapper.PermissionRungs, and a sixth assertion above); it is deliberately out
// of scope here, and this probe stays a log until that lands. Note the
// apostrophe: any alternation row for it must survive claude rendering ' as a
// typographic ’.
func probeClaudeDontAsk(t *testing.T, bin string) {
	t.Helper()
	p := probeClaudeFooter(t, bin, "dontAsk")
	t.Logf("PROBE dontAsk (never asserted): footer=%q suffix=%q detector=%q readable=%t",
		p.line, p.suffix, p.rung, p.readable)
	if p.readable {
		t.Logf("PROBE dontAsk: the shipped parser read %q for claude's native dontAsk mode — "+
			"confirm whether dontAsk paints a distinct footer before treating this as a rung",
			p.rung)
	}
}

// assertDistinctRungReadings fails when the five rungs collapse onto fewer
// readings. A collapsed reading is the exact failure the auth precondition
// guards against, and it is also what a partially-renamed footer looks like.
func assertDistinctRungReadings(t *testing.T, seen map[string]string) {
	t.Helper()
	if len(seen) < 2 {
		return // nothing to compare; the per-rung errors above already fired
	}
	byReading := make(map[string][]string, len(seen))
	for launched, detected := range seen {
		byReading[detected] = append(byReading[detected], launched)
	}
	if len(byReading) == len(seen) {
		return
	}
	for detected, launched := range byReading {
		if len(launched) > 1 {
			sort.Strings(launched)
			t.Errorf("FOOTER DRIFT: rungs %v all read back as %q — the footer collapsed onto one "+
				"reading. Either claude is not honouring --permission-mode, or "+
				"pkg/turns/harness/claudecode/permmode.go:55 (permissionModeRE) no longer "+
				"distinguishes the modes. See docs/md/internal/versions-drift.md",
				launched, detected)
		}
	}
}

// claudeAuthWall returns the matched login/expiry wall text, or "" when the
// screen shows none.
func claudeAuthWall(text string) string {
	for _, re := range claudeAuthWallRE {
		if m := re.FindString(text); m != "" {
			return m
		}
	}
	return ""
}

// claudeFooterLine returns the rendered footer line and its version-dependent
// suffix, both for LOGGING and drift reporting only. Both are empty when no
// glyph line is on screen. text MUST be a pkg/screen render
// (screen.Snapshot.Text) — only the emulator reassembles claude's column-jump
// footer into a contiguous line; raw PTY bytes never match.
func claudeFooterLine(text string) (line, suffix string) {
	for _, cand := range claudeFooterGlyphLineRE.FindAllString(text, -1) {
		cand = strings.TrimRight(cand, " \t")
		m := claudeFooterSuffixRE.FindStringSubmatch(cand)
		if m == nil {
			continue
		}
		return cand, strings.TrimSpace(m[1])
	}
	return "", ""
}
