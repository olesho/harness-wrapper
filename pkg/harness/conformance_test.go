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
	"github.com/olesho/harness-wrapper/pkg/screen"
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

// openConformanceConv launches one live harness session and returns it with the
// close function the caller MUST defer. The close is deliberately the caller's
// deferred responsibility rather than t.Cleanup: probes run in a loop inside a
// single test function, so a t.Cleanup close would keep every launched process
// alive until the whole test ended. label appears in the open/close diagnostics.
//
// Every live probe in this file goes through here so the per-launch deadline and
// teardown discipline exist in exactly one place: on t.Fatal/t.Skip the deferred
// close still runs (both unwind through it), so a stalled launch never leaks a
// live harness process into the next iteration.
//
// ctx MUST outlive the probe, not just the call: chat.Open hands it to the
// wrapper as the SESSION's context, so cancelling it kills the harness process.
// Deriving a context inside this helper and cancelling it on return looks
// harmless and is not — it kills the harness the moment the launch succeeds and
// leaves every later poll reading a blank post-teardown screen.
func openConformanceConv(t *testing.T, ctx context.Context, label string, opts chat.Options) (*chat.Conversation, func()) {
	t.Helper()

	conv, err := chat.Open(ctx, opts)
	if err != nil {
		t.Fatalf("chat.Open(%s): %v", label, err)
	}
	return conv, func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer closeCancel()
		if err := conv.Close(closeCtx); err != nil {
			t.Logf("close (%s): %v", label, err)
		}
	}
}

// pollScreen polls conv's rendered screen until ready reports satisfied, or
// until timeout elapses. It exists because chat.Open does NOT wait for readiness
// (pkg/chat/ready.go:23 waitReadyForSend is unexported and reachable only via
// Send, which no probe here may call), so every live probe owns its own
// deadline. It returns the last snapshot observed either way, so a timed-out
// caller can still quote the screen it gave up on.
//
// ready runs on the test goroutine, so it may call t.Skipf/t.Fatalf directly —
// the Goexit unwinds through the caller's deferred close.
func pollScreen(conv *chat.Conversation, timeout time.Duration, ready func(screen.Snapshot) bool) (screen.Snapshot, bool) {
	deadline := time.Now().Add(timeout)
	for {
		snap := conv.ScreenSnapshot()
		if ready(snap) {
			return snap, true
		}
		if time.Now().After(deadline) {
			return snap, false
		}
		time.Sleep(footerPollInterval)
	}
}

// probeClaudeFooter launches claude once at mode and polls the rendered screen
// until the shipped detector can read a permission mode (or the deadline
// expires). The conversation is closed before this returns — including on
// t.Fatal/t.Skip, which unwind through the deferred close — so a stalled rung
// never leaks a live claude process into the next iteration.
func probeClaudeFooter(t *testing.T, bin, mode string) footerProbe {
	t.Helper()

	// Cancelled only when this probe returns: the conversation's process dies
	// with it (see openConformanceConv).
	ctx, cancel := context.WithTimeout(context.Background(), footerPollTimeout+30*time.Second)
	defer cancel()

	conv, closeConv := openConformanceConv(t, ctx, fmt.Sprintf("claude-code, permission mode %q", mode), chat.Options{
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
	defer closeConv()

	det, ok := conv.Adapter().(turns.PermissionModeDetector)
	if !ok {
		t.Fatalf("claude-code adapter %T does not implement turns.PermissionModeDetector — "+
			"the capability this test asserts through has been removed", conv.Adapter())
	}

	var p footerProbe
	pollScreen(conv, footerPollTimeout, func(snap screen.Snapshot) bool {
		if wall := claudeAuthWall(snap.Text); wall != "" {
			// Skip, never fail: a logged-out claude still paints a footer, so
			// asserting here would report bogus drift (see claudeAuthWallRE).
			t.Skipf("claude is not logged in (screen shows %q) — log in with `claude` and re-run; "+
				"an unauthenticated session paints a footer for every rung and would make this "+
				"check lie rather than fail", wall)
		}
		p.line, p.suffix = claudeFooterLine(snap.Text)
		p.rung, p.readable = det.PermissionMode(snap)
		return p.readable
	})
	return p
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

// --- Live /status conformance (codex) ---------------------------------------
//
// This is an ANCHOR CHECK, not a scrape. Codex's permissions rung lives on a
// `/status` box that nothing in Go reads today — (*codex.Adapter).PermissionMode
// says so in as many words (pkg/turns/harness/codex/codex.go:184-188: "a
// separate follow-up ticket"). This test therefore introduces no scraper and
// consumes none. It asserts only that the two permission-relevant `/status` rows
// still RENDER with the expected value shape, so the labels a future scraper
// will key on have a live guard the day it is written — and so that a codex
// release which renames or drops them is caught here rather than by the first
// user of the scraper that does not exist yet.
//
// The second assertion is the one that earns the launch. Codex's collaboration
// detector is already SHIPPED, and its riskiest rule is that the ABSENCE of the
// `Plan mode` marker means "default" and NOT "unknown"
// (pkg/turns/harness/codex/permmode.go:71-100). Absence is unfalsifiable from
// fixtures: a frozen screen with no marker keeps answering "default" forever. A
// codex release that starts painting a default-mode marker, or that changes the
// composer shape composerReadable depends on, silently flips the live answer to
// ("", false) with nothing else in the tree to catch it. So the same launch that
// reads the `/status` rows also asks the shipped detector what it sees.
//
// TWO AXES, DELIBERATELY NOT CONFLATED (codex.go:174-182). The launch flag pair
// names a PERMISSIONS rung (`--permission-mode manual` -> `-s read-only -a
// untrusted`); the detector reports a COLLABORATION mode (plan/default) that is
// reachable only by shift+tab from inside a running session — indeed
// `--permission-mode plan` is REJECTED at launch for codex (validatePermissionMode,
// pkg/wrapper/wrapper.go:367-372). "Launched read-only, detector says default" is
// therefore not a coincidence to celebrate but two independent facts, and the
// failure messages below keep them apart.
//
// AGREEMENT WITH THE TYPESCRIPT SIBLING SUITE (META-HARNESS-131): same launch
// pair, same two labels asserted exactly, same loose value shapes, same
// detector expectation of ("default", true). One deliberate divergence: this
// side does NOT assert the ORDER of the two rows within the box, nor any other
// row of `/status` (model, account, token usage) — those are account- and
// version-dependent, and asserting them would make this test fail for reasons
// that are not drift in the signal it guards.
//
// SAFETY. Read-only by construction: `/status` only prints. This test never
// opens `/permissions` — selecting a preset there WRITES ~/.codex/config.toml
// globally and would mutate the developer's own config as a side effect of
// running tests — and never `/model`, never a settings screen, never a turn (so
// it costs no tokens). CODEX_HOME is deliberately NOT sandboxed: codex's
// credentials live at $CODEX_HOME/auth.json, so an isolated CODEX_HOME yields an
// unauthenticated session and this test would only ever skip. Safety comes from
// the session being read-only, not from isolation.

const (
	// The two labels a future /status scraper will key on. Asserted EXACTLY —
	// they are the anchor. Their values are asserted loosely (below), because
	// codex reformats the value side across releases far more freely than it
	// renames a row.
	codexStatusPermissionsLabel   = "Permissions:"
	codexStatusCollaborationLabel = "Collaboration mode:"

	// The launch pair the value-shape assertions are tied to:
	// chat.Options.PermissionMode "manual" -> these codex flags, via
	// codexPermissionMode (pkg/wrapper/wrapper.go:684).
	codexStatusLaunchMode  = "manual"
	codexStatusLaunchFlags = "-s read-only -a untrusted"

	// The collaboration-axis value the shipped detector must report for a
	// launch that never touches shift+tab. NOT a permission rung.
	codexStatusWantCollabMode = "default"

	// codexStatusCommand is the slash command driven. It is only ever TYPED and
	// submitted — the /status box merely prints, which is what makes this whole
	// check safe to run against the developer's own codex.
	codexStatusCommand = "/status"

	// statusPollTimeout bounds the wait for the /status box to paint after the
	// command is submitted. Generous because codex renders the box only after
	// its own account/usage lookups settle.
	statusPollTimeout = 45 * time.Second

	// codexComposerEchoTimeout bounds the wait for the composer to echo the
	// typed command, which is the gate for sending the submit key (see below).
	codexComposerEchoTimeout = 15 * time.Second
)

// codexStatusRowRE builds the line matcher for one /status label. It matches the
// WHOLE rendered row so a drift report can quote it verbatim, and it is built
// from the label const so the assertion and the message can never disagree about
// what was expected. QuoteMeta because the labels end in ":".
func codexStatusRowRE(label string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^.*` + regexp.QuoteMeta(label) + `.*$`)
}

var (
	codexStatusPermissionsRowRE   = codexStatusRowRE(codexStatusPermissionsLabel)
	codexStatusCollaborationRowRE = codexStatusRowRE(codexStatusCollaborationLabel)

	// codexComposerEchoRE matches the idle composer row ("› ") once it carries
	// the typed command. It gates the submit key — see probeCodexStatus.
	codexComposerEchoRE = regexp.MustCompile(`(?m)^[^\S\r\n]*›[^\S\r\n]+` + regexp.QuoteMeta(codexStatusCommand))

	// Loose value shapes, one per flag of the launch pair. Deliberately
	// unanchored and case-insensitive: what is guarded is that the row still
	// REFLECTS the launched flags, not how codex cases or punctuates them —
	// codex 0.144.5 renders `-s read-only -a untrusted` as
	// "Read Only (untrusted)", and neither the capitalisation nor the
	// parenthesis is worth a failing test.
	codexStatusPermissionsValueChecks = []struct {
		flag string         // the launch flag this value must reflect
		re   *regexp.Regexp // the loose shape asserted
	}{
		{flag: "-s read-only", re: regexp.MustCompile(`(?i)read[-_ ]?only`)},
		{flag: "-a untrusted", re: regexp.MustCompile(`(?i)untrusted`)},
	}

	// The collaboration row must NOT read as Plan for a launch that never
	// pressed shift+tab. Same "non-plan default" fact the detector reports,
	// observed on the other axis's own row.
	codexStatusPlanValueRE = regexp.MustCompile(`(?i)\bplan\b`)
)

// codexAuthWallRE mirrors the codex sign-in walls that pkg/chat's unexported
// authRequired matches for this harness (codexOnboardingRE + codexLoggedOutRE,
// pkg/chat/ready.go:228-244). A local copy because those are unexported; it must
// be updated in lockstep with them.
//
// Skipping on a wall is a correctness requirement, not politeness: a logged-out
// codex paints an onboarding menu instead of the composer, `/status` never runs,
// and the detector answers ("", false) by design (composerReadable's signinWallRE
// arm). Failing there would report drift that does not exist.
var codexAuthWallRE = []*regexp.Regexp{
	regexp.MustCompile(`(?i)sign in with chatgpt`),
	regexp.MustCompile(`(?i)finish signing in via your browser`),
	regexp.MustCompile(`(?i)\b401 unauthorized\b`),
	regexp.MustCompile(`(?i)missing bearer or basic authentication`),
	regexp.MustCompile(`(?i)\bnot logged in\b`),
	regexp.MustCompile(`(?i)\bcodex(?: mcp)? login\b`),
}

// codexStatusProbe is the one launch's observation.
type codexStatusProbe struct {
	permissionsRow   string // whole rendered row, "" if never painted
	collaborationRow string // whole rendered row, "" if never painted
	collabMode       string // what the SHIPPED detector read
	readable         bool   // the detector's second return
	screenText       string // last rendered screen, for drift reports
}

// TestConformance_CodexStatusRows launches the real codex binary ONCE with a
// known permission pair, drives `/status`, and asserts (1) the two
// permission-relevant rows still render with the expected value shape and (2)
// the shipped collaboration detector still reads ("default", true) off that same
// screen. See the block comment above for why each half exists.
func TestConformance_CodexStatusRows(t *testing.T) {
	requireConformance(t)

	all, err := versions.All()
	if err != nil {
		t.Fatalf("versions.All: %v", err)
	}
	e, ok := all["codex"]
	if !ok || e.Binary == "" {
		t.Skip("codex has no versions.json entry with a binary — nothing to launch")
	}
	bin, err := exec.LookPath(e.Binary)
	if err != nil {
		t.Skipf("codex: binary %q not on PATH — skipping", e.Binary)
	}
	// Deliberately independent of the versions.json pin, exactly as the claude
	// footer check is: this asserts what the INSTALLED codex paints, so pin skew
	// is a log line rather than a skip.
	t.Logf("probing %s (versions.json pin: %s)", bin, e.Pinned)

	p := probeCodexStatus(t, bin)
	t.Logf("launch %s (--permission-mode %s) -> permissions=%q collaboration=%q detector=%q readable=%t",
		codexStatusLaunchFlags, codexStatusLaunchMode,
		strings.TrimSpace(p.permissionsRow), strings.TrimSpace(p.collaborationRow),
		p.collabMode, p.readable)

	assertCodexStatusRows(t, p)
	assertCodexCollaborationDetector(t, p)
}

// assertCodexStatusRows checks both /status rows: labels exactly, values loosely
// and tied to the launch pair.
func assertCodexStatusRows(t *testing.T, p codexStatusProbe) {
	t.Helper()

	if p.permissionsRow == "" {
		t.Errorf("STATUS ROW DRIFT: codex launched with %s (chat.Options.PermissionMode %q) rendered "+
			"no %q row in /status within %s.\n"+
			"  expected a row matching: %s\n"+
			"  labels actually rendered in the box: %s\n"+
			"  rendered screen:\n%s\n"+
			"  the label is the anchor a future /status scraper keys on (the scrape itself is still "+
			"a follow-up — see pkg/turns/harness/codex/codex.go:184-188). If codex renamed it, update "+
			"this label and the follow-up's scraper together.\n"+
			"  see docs/md/internal/versions-drift.md",
			codexStatusLaunchFlags, codexStatusLaunchMode, codexStatusPermissionsLabel,
			statusPollTimeout, codexStatusPermissionsRowRE,
			codexStatusLabelsOnScreen(p.screenText), indentScreen(p.screenText))
	} else {
		value := codexStatusRowValue(p.permissionsRow, codexStatusPermissionsLabel)
		for _, check := range codexStatusPermissionsValueChecks {
			if check.re.MatchString(value) {
				continue
			}
			t.Errorf("STATUS ROW DRIFT: codex launched with %s (chat.Options.PermissionMode %q) rendered "+
				"a %q row whose value no longer reflects %s.\n"+
				"  status line as rendered: %q\n"+
				"  row value: %q\n"+
				"  expected value to match: %s\n"+
				"  the LABEL still matches, so this is value-shape drift, not a rename: either codex "+
				"reworded how it renders that flag, or %s is no longer what --permission-mode %s maps "+
				"to (codexPermissionMode, pkg/wrapper/wrapper.go:684).\n"+
				"  see docs/md/internal/versions-drift.md",
				codexStatusLaunchFlags, codexStatusLaunchMode, codexStatusPermissionsLabel, check.flag,
				strings.TrimSpace(p.permissionsRow), value, check.re, check.flag, codexStatusLaunchMode)
		}
	}

	if p.collaborationRow == "" {
		t.Errorf("STATUS ROW DRIFT: codex launched with %s (chat.Options.PermissionMode %q) rendered "+
			"no %q row in /status within %s.\n"+
			"  expected a row matching: %s\n"+
			"  labels actually rendered in the box: %s\n"+
			"  rendered screen:\n%s\n"+
			"  this row is the /status-side view of the COLLABORATION axis that "+
			"pkg/turns/harness/codex/permmode.go reads off the composer marker — a different axis "+
			"from the launch-time permissions rung above. If codex renamed it, update this label.\n"+
			"  see docs/md/internal/versions-drift.md",
			codexStatusLaunchFlags, codexStatusLaunchMode, codexStatusCollaborationLabel,
			statusPollTimeout, codexStatusCollaborationRowRE,
			codexStatusLabelsOnScreen(p.screenText), indentScreen(p.screenText))
	} else if codexStatusPlanValueRE.MatchString(codexStatusRowValue(p.collaborationRow, codexStatusCollaborationLabel)) {
		t.Errorf("STATUS ROW DRIFT: codex launched with %s (chat.Options.PermissionMode %q) — which "+
			"never presses shift+tab — reported a PLAN collaboration mode in /status.\n"+
			"  status line as rendered: %q\n"+
			"  expected the non-plan default, i.e. a value NOT matching: %s\n"+
			"  either codex changed its default collaboration mode, or the composer marker and the "+
			"/status row have diverged. Note the axis: this is collaboration (plan|default), NOT the "+
			"launched permissions rung — --permission-mode plan is rejected at launch for codex "+
			"(validatePermissionMode, pkg/wrapper/wrapper.go:367-372).\n"+
			"  see docs/md/internal/versions-drift.md",
			codexStatusLaunchFlags, codexStatusLaunchMode,
			strings.TrimSpace(p.collaborationRow), codexStatusPlanValueRE)
	}
}

// assertCodexCollaborationDetector checks the SHIPPED detector's reading of the
// same screen. This is the only live check that codex's "absence of a Plan
// marker means default, not unknown" rule still holds.
func assertCodexCollaborationDetector(t *testing.T, p codexStatusProbe) {
	t.Helper()

	if !p.readable {
		t.Errorf("STATUS ROW DRIFT: codex launched with %s (chat.Options.PermissionMode %q), but the "+
			"shipped collaboration detector could not read a mode off the rendered screen "+
			"(got %q, readable=false; want %q, readable=true).\n"+
			"  collaboration row as rendered: %q\n"+
			"  rendered screen:\n%s\n"+
			"  fix pkg/turns/harness/codex/permmode.go — a DIFFERENT package from this test, which is "+
			"exactly why this message names it. An unreadable screen here means composerReadable's gate "+
			"stopped recognising a healthy codex screen: signinWallRE, DetectInput or PromptReady "+
			"drifted. It does NOT mean codex is in Plan mode.\n"+
			"  see docs/md/internal/versions-drift.md",
			codexStatusLaunchFlags, codexStatusLaunchMode, p.collabMode, codexStatusWantCollabMode,
			strings.TrimSpace(p.collaborationRow), indentScreen(p.screenText))
		return
	}
	if p.collabMode != codexStatusWantCollabMode {
		t.Errorf("STATUS ROW DRIFT: codex launched with %s (chat.Options.PermissionMode %q), but the "+
			"shipped collaboration detector read %q where %q was expected.\n"+
			"  collaboration row as rendered: %q\n"+
			"  this launch never presses shift+tab, so codex should paint NO collaboration marker and "+
			"the detector should report the default by ABSENCE. A %q reading means "+
			"collaborationPlanRE now fires on a default screen — fix "+
			"pkg/turns/harness/codex/permmode.go:59 (collaborationPlanRE), a DIFFERENT package from "+
			"this test. Do not confuse this collaboration value with the launched permissions rung "+
			"(%s); they are different axes (codex.go:174-182).\n"+
			"  see docs/md/internal/versions-drift.md",
			codexStatusLaunchFlags, codexStatusLaunchMode, p.collabMode, codexStatusWantCollabMode,
			strings.TrimSpace(p.collaborationRow), p.collabMode, codexStatusLaunchFlags)
	}
}

// probeCodexStatus launches codex once, waits for its composer, submits
// `/status`, and returns what the screen and the shipped detector reported. The
// conversation is closed before this returns — including on t.Fatal/t.Skip,
// which unwind through the deferred close — so a stalled launch never leaks a
// live codex process.
func probeCodexStatus(t *testing.T, bin string) codexStatusProbe {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), footerPollTimeout+statusPollTimeout+60*time.Second)
	defer cancel()

	conv, closeConv := openConformanceConv(t, ctx, "codex, /status", chat.Options{
		Harness:        "codex",
		BinaryPath:     bin,
		PermissionMode: codexStatusLaunchMode,
		Store:          memstore.New(),
		// A pending "Update available!" menu covers the composer and would wedge
		// the launch; the zero value SURFACES it for a client to answer and there
		// is no client here. Same choice pkg/oneshot makes for headless callers
		// (pkg/oneshot/oneshot.go:200).
		AutoSkipCodexUpdateNotice: true,
		// Cols/Rows are deliberately left at the 120x40 default
		// (pkg/chat/conversation.go:71): codex DEGRADES /status on narrow
		// terminals, so a miss caused by a hand-picked width would be an artifact
		// of this test rather than drift in codex.
	})
	defer closeConv()

	det, ok := conv.Adapter().(turns.PermissionModeDetector)
	if !ok {
		t.Fatalf("codex adapter %T does not implement turns.PermissionModeDetector — "+
			"the capability this test asserts through has been removed", conv.Adapter())
	}

	// 1. Wait for a prompt-ready composer. chat.Open does not do this, and
	//    typing into a startup interstitial would type /status into a menu.
	//    The detector's own readability gate IS codex's composer gate
	//    (composerReadable mirrors pkg/chat/ready.go's codex arm), so it doubles
	//    as the readiness signal here.
	ready, ok := pollScreen(conv, footerPollTimeout, func(snap screen.Snapshot) bool {
		if wall := codexAuthWall(snap.Text); wall != "" {
			t.Skipf("codex is not logged in (screen shows %q) — run `codex` and sign in, then re-run; "+
				"an unauthenticated session never paints /status and this check would report drift "+
				"that does not exist", wall)
		}
		_, readable := det.PermissionMode(snap)
		return readable
	})
	if !ok {
		t.Fatalf("codex composer never became prompt-ready within %s — /status was never submitted, "+
			"so nothing about the status rows can be concluded.\n  rendered screen:\n%s",
			footerPollTimeout, indentScreen(ready.Text))
	}

	// 2. Type /status, then submit it as a SEPARATE write. Send is forbidden
	//    here: /status produces no assistant turn, so Send would register a turn
	//    that never completes and stall to the deadline. Raw WriteStdin after
	//    AcquireControl is the escape hatch pkg/chat/send.go:20-27 documents for
	//    exactly this.
	release, err := conv.AcquireControl(ctx)
	if err != nil {
		t.Fatalf("AcquireControl: %v", err)
	}
	defer release()
	if _, err := conv.Wrapper().WriteStdin([]byte(codexStatusCommand)); err != nil {
		t.Fatalf("WriteStdin(%s): %v", codexStatusCommand, err)
	}
	// Observed live at codex 0.144.5: writing the text and the submit key in ONE
	// WriteStdin leaves "/status" sitting unsubmitted in the composer. Typing a
	// leading "/" opens codex's slash-command popup (it lists /status and
	// /statusline), and the Enter that arrives in the same burst is swallowed by
	// that popup rather than running the command. So the submit key goes out only
	// once the composer has visibly echoed the typed command.
	echoed, ok := pollScreen(conv, codexComposerEchoTimeout, func(snap screen.Snapshot) bool {
		return codexComposerEchoRE.MatchString(snap.Text)
	})
	if !ok {
		t.Fatalf("codex composer never echoed %q within %s — it was never submitted, so nothing "+
			"about the status rows can be concluded.\n  rendered screen:\n%s",
			codexStatusCommand, codexComposerEchoTimeout, indentScreen(echoed.Text))
	}

	// 3. Poll for both rows, re-sending the submit key once mid-window. The
	//    re-send is the cheap insurance against losing the race above on a slower
	//    machine: an Enter on an already-submitted (and therefore empty) composer
	//    is a no-op for codex, while a swallowed first Enter would otherwise burn
	//    the whole deadline and report drift that is not there.
	submit := func() {
		// CSI 13 u — the unmodified Enter of the kitty keyboard protocol codex
		// >= 0.141.0 enables at startup. A plain "\r" only inserts a newline in
		// the composer and /status never runs. Written inline because pkg/chat's
		// submitKeyForHarness (pkg/chat/ready.go:333, codex arm at :345-351) is
		// unexported — that helper is the contract this literal mirrors.
		if _, err := conv.Wrapper().WriteStdin([]byte("\x1b[13u")); err != nil {
			t.Fatalf("WriteStdin(submit key): %v", err)
		}
	}
	submit()

	var p codexStatusProbe
	rowsFound := func(snap screen.Snapshot) bool {
		p.permissionsRow = codexStatusPermissionsRowRE.FindString(snap.Text)
		p.collaborationRow = codexStatusCollaborationRowRE.FindString(snap.Text)
		return p.permissionsRow != "" && p.collaborationRow != ""
	}
	final, ok := pollScreen(conv, statusPollTimeout/2, rowsFound)
	if !ok {
		submit()
		final, _ = pollScreen(conv, statusPollTimeout/2, rowsFound)
	}

	// The detector is re-read on the SAME final screen the rows were read off, so
	// the two halves of this test describe one moment rather than two.
	p.screenText = final.Text
	p.collabMode, p.readable = det.PermissionMode(final)
	return p
}

// codexAuthWall returns the matched sign-in wall text, or "" when the screen
// shows none.
func codexAuthWall(text string) string {
	for _, re := range codexAuthWallRE {
		if m := re.FindString(text); m != "" {
			return m
		}
	}
	return ""
}

// codexStatusRowLabelRE captures the label side of any `<Label>:  <value>` row
// inside the /status box. Reporting only: it exists so a missing-label failure
// can name the labels codex DID render, turning "expected X, here is a 40-line
// screen" into "expected X, got these labels instead" — the rename is then
// readable without diffing the screen by eye.
var codexStatusRowLabelRE = regexp.MustCompile(`(?m)^[^\S\r\n]*│[^\S\r\n]+([A-Z][^\s│][^│]*?:)[^\S\r\n]`)

// codexStatusLabelsOnScreen lists the /status box labels present in text, in
// render order and de-duplicated.
func codexStatusLabelsOnScreen(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range codexStatusRowLabelRE.FindAllStringSubmatch(text, -1) {
		label := strings.TrimSpace(m[1])
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	if len(out) == 0 {
		return []string{"(none — the /status box did not render at all)"}
	}
	return out
}

// codexStatusRowValue returns the row's value side — everything after the label,
// with the emulator's right-padding and any box border trimmed. Used only for
// the collaboration row's negative value check, so a `Plan mode` marker painted
// elsewhere on the screen cannot be mistaken for the row's own value.
func codexStatusRowValue(row, label string) string {
	_, value, found := strings.Cut(row, label)
	if !found {
		return ""
	}
	return strings.TrimSpace(strings.Trim(strings.TrimRight(value, " \t"), "│|"))
}

// indentScreen indents a rendered screen for inclusion in a failure message and
// trims the emulator's trailing blank rows, which are pure padding
// (pkg/screen/screen.go:23-27 preserves them) and would otherwise bury the
// interesting part of the report under 20 empty lines.
func indentScreen(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = "    | " + strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}
