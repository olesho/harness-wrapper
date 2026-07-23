package wrapper

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/olesho/harness-wrapper/pkg/versions"
)

// Layer 4 of the test pyramid (see docs/md/internal/testing/README.md): live
// conformance against the REAL installed harness binaries, for the permission
// FLAG surface. pkg/harness/conformance_test.go owns the version-drift and
// turn-level half of layer 4; this file owns the launch-time permission argv we
// EMIT — the only thing that catches a claude-code/codex release that renames,
// removes or ADDS a permission value out from under
// argsWithHarnessPermissionMode.
//
// It lives in package wrapper (not package wrapper_test) on purpose: every
// anchor worth asserting against — the canonical rungs, both harnesses' native
// spellings, isSupportedPermissionMode / validatePermissionMode, and the two
// mappers claudePermissionMode / codexPermissionMode — is unexported. From an
// external test package the only option would be a second hardcoded copy of the
// mode list, which proves "the CLI still takes these strings" and says nothing
// about whether they are still what we emit. Nothing is exported to make this
// file possible; wrapper.PermissionRungs covers the five canonical rungs only.
//
// Gated behind HARNESS_WRAPPER_CONFORMANCE=1 — the SAME gate as
// pkg/harness/conformance_test.go — and additionally skips any harness whose
// binary is not on PATH, so `make test` stays green with the gate unset and
// with both binaries absent.
//
//	HARNESS_WRAPPER_CONFORMANCE=1 go test ./pkg/harness/ ./pkg/wrapper/ -run Conformance -v
//
// Every probe is `<flag...> --version`: no session, no TUI, no turn, no quota,
// and (measured under an isolated HOME/CODEX_HOME) ZERO files created, so the
// no-config-mutation criterion holds by construction. The probes must NOT use a
// subcommand's --help: `codex -s bogus exec --help` exits 0 because clap's
// subcommand help short-circuits ahead of global-flag validation, so a probe
// written that way passes for ANY value, including removed ones. Verified:
// `codex -s bogus exec --help` -> 0, `codex -s bogus --version` -> 2.
//
// Deliberate divergence from the TypeScript sibling suite (META-HARNESS-131):
// the TS port may express these probes against its own mode table, but two
// choices here are load-bearing and should be adopted rather than re-derived:
// (1) the probe set is DERIVED by filtering candidate modes through production's
// own validatePermissionMode, so the codex+plan exclusion is a consequence and a
// future rung enters the probe set automatically; (2) the allowed-set comparison
// runs BOTH directions — a value upstream ADDS is drift too, because it is a
// posture the harness now supports that our rung mapping cannot express.
//
// Assertions must never key off the versions.json pins: these probe the
// INSTALLED binary and pass regardless of pin skew (which
// TestConformance_VersionDrift reports separately).

// permissionConformanceEnv gates this file. Mirrors the conformance gate in
// pkg/harness/conformance_test.go, whose helper is unexported in package
// harness_test and therefore unreachable from here — the same shape as
// internal/env/openshell/containment_live_test.go and
// internal/env/daytona/live_test.go, each of which carries its own copy.
const permissionConformanceEnv = "HARNESS_WRAPPER_CONFORMANCE"

func requirePermissionConformance(t *testing.T) {
	t.Helper()
	if os.Getenv(permissionConformanceEnv) != "1" {
		t.Skipf("set %s=1 to run live permission-flag conformance against installed harness binaries", permissionConformanceEnv)
	}
}

// driftDoc is where a drift report points the reader.
const driftDoc = "docs/md/internal/versions-drift.md"

// permissionConformanceHarness binds a wrapper harness name to its versions.json
// key (for the binary lookup) and to the vocabulary this file probes upstream
// for. natives holds that harness's native spellings; the canonical rungs come
// from PermissionRungs so there is exactly ONE mode list in the repo.
type permissionConformanceHarness struct {
	wrapper     string
	versionsKey string
	natives     []string
	allowedSets []allowedSetProbe
}

// allowedSetProbe describes one rejection probe: the flag to feed a bogus value,
// the regexp that lifts the enumeration out of the binary's own error message,
// and the set we expect it to enumerate — built from the unexported consts, never
// from string literals.
type allowedSetProbe struct {
	flag  string
	label string
	parse *regexp.Regexp
	want  []string
}

// claudeAllowedRE lifts commander.js's enumeration:
//
//	error: option '--permission-mode <mode>' argument 'zzz' is invalid. Allowed choices are acceptEdits, auto, bypassPermissions, manual, dontAsk, plan.
var claudeAllowedRE = regexp.MustCompile(`Allowed choices are ([^.]*)\.`)

// clapAllowedRE lifts clap's enumeration:
//
//	error: invalid value 'zzz' for '--sandbox <SANDBOX_MODE>'
//	  [possible values: read-only, workspace-write, danger-full-access]
var clapAllowedRE = regexp.MustCompile(`\[possible values:\s*([^\]]*)\]`)

func permissionConformanceHarnesses() []permissionConformanceHarness {
	return []permissionConformanceHarness{
		{
			wrapper:     "claude",
			versionsKey: "claude-code",
			natives:     []string{claudeModeAcceptEdits, claudeModeDontAsk, claudeModeBypassPermissions},
			allowedSets: []allowedSetProbe{{
				flag:  "--permission-mode",
				label: "--permission-mode",
				parse: claudeAllowedRE,
				// plan / manual / auto are simultaneously canonical rungs and
				// claude-native spellings, so they enter from the rung consts.
				want: []string{
					claudeModeAcceptEdits,
					permissionModeAuto,
					claudeModeBypassPermissions,
					permissionModeManual,
					claudeModeDontAsk,
					permissionModePlan,
				},
			}},
		},
		{
			wrapper:     "codex",
			versionsKey: "codex",
			natives:     []string{codexSandboxReadOnly, codexSandboxWorkspaceWrite, codexSandboxDangerFullAccess},
			allowedSets: []allowedSetProbe{
				{
					flag:  "-s",
					label: "-s/--sandbox",
					parse: clapAllowedRE,
					want:  []string{codexSandboxReadOnly, codexSandboxWorkspaceWrite, codexSandboxDangerFullAccess},
				},
				{
					flag:  "-a",
					label: "-a/--ask-for-approval",
					parse: clapAllowedRE,
					// Exactly the values codexPermissionMode returns on the
					// approval axis.
					want: []string{codexApprovalUntrusted, codexApprovalOnRequest, codexApprovalNever},
				},
			},
		},
	}
}

// probeModes returns the modes production would accept for this harness, in a
// stable order: the canonical rungs then the harness's native spellings, each
// filtered through production's OWN predicate. Using validatePermissionMode with
// nil Args means the one exclusion (codex + plan, rejected at wrapper.go because
// codex has no launch-time plan flag) is DERIVED, not hardcoded, and a rung or
// native added later enters the probe set automatically.
func (h permissionConformanceHarness) probeModes() []string {
	var out []string
	for _, mode := range append(PermissionRungs(), h.natives...) {
		if validatePermissionMode(&Config{Harness: h.wrapper, PermissionMode: mode}) == nil {
			out = append(out, mode)
		}
	}
	return out
}

// lookupHarnessBinary resolves a harness's binary via versions.json + PATH,
// exactly as TestConformance_VersionDrift does. Deliberately reads only Binary:
// the pinned VERSION is irrelevant here, these probes must pass against whatever
// is installed.
func lookupHarnessBinary(t *testing.T, versionsKey string) (string, bool) {
	t.Helper()
	all, err := versions.All()
	if err != nil {
		t.Fatalf("versions.All: %v", err)
	}
	e, ok := all[versionsKey]
	if !ok || e.Binary == "" {
		t.Logf("%s: no binary recorded in versions.json — skipping", versionsKey)
		return "", false
	}
	bin, err := exec.LookPath(e.Binary)
	if err != nil {
		t.Logf("%s: binary %q not on PATH — skipping", versionsKey, e.Binary)
		return "", false
	}
	return bin, true
}

// runVersionProbe appends --version to argv and runs it, returning the combined
// output and the exit code. CombinedOutput mirrors checkVersionDrift; both
// binaries write their rejection enumeration to stderr.
func runVersionProbe(t *testing.T, bin string, argv ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append(append([]string{}, argv...), "--version")
	out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("%s %s: %v\n%s", bin, strings.Join(args, " "), err, out)
	return "", -1
}

// TestConformance_PermissionModeArgvAccepted asserts the mapping ROUND-TRIPS
// through the real binary: for every mode production's predicate accepts, the
// argv argsWithHarnessPermissionMode actually produces is accepted by the
// installed binary. This — not "claude still takes acceptEdits" — is what
// catches drift in claudePermissionMode / codexPermissionMode.
func TestConformance_PermissionModeArgvAccepted(t *testing.T) {
	requirePermissionConformance(t)

	probed := 0
	for _, h := range permissionConformanceHarnesses() {
		bin, ok := lookupHarnessBinary(t, h.versionsKey)
		if !ok {
			continue
		}
		probed++
		t.Run(h.wrapper, func(t *testing.T) {
			modes := h.probeModes()
			if len(modes) == 0 {
				t.Fatalf("PERMISSION FLAG DRIFT: %s: validatePermissionMode accepts NO mode — the production predicate or the rung consts changed shape", h.wrapper)
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					argv := argsWithHarnessPermissionMode(h.wrapper, nil, mode)
					if len(argv) == 0 {
						t.Fatalf("PERMISSION FLAG DRIFT: %s mode %q: argsWithHarnessPermissionMode emitted NO argv; expected a launch-time permission flag. See %s.",
							h.wrapper, mode, driftDoc)
					}
					out, code := runVersionProbe(t, bin, argv...)
					if code != 0 {
						t.Errorf("PERMISSION FLAG DRIFT: %s mode %q: the installed binary REJECTED the argv we emit.\n"+
							"  emitted: %s %s --version\n  exit:    %d\n  output:  %q\n"+
							"Our mapper produces a value the binary no longer accepts. See %s.",
							h.wrapper, mode, bin, strings.Join(argv, " "), code, strings.TrimSpace(out), driftDoc)
						return
					}
					t.Logf("%s %s -> %s (accepted)", h.wrapper, mode, strings.Join(argv, " "))
				})
			}
		})
	}
	if probed == 0 {
		t.Skip("no harness binaries installed — nothing to probe")
	}
}

// TestConformance_PermissionAllowedSets compares each binary's OWN enumeration
// of a permission flag's allowed values against the set our consts encode, in
// BOTH directions. One rejection probe per flag yields the exact upstream set:
//
//	claude --permission-mode zzz --version   # exit 1, "Allowed choices are ..."
//	codex  -s zzz --version                  # exit 2, "[possible values: ...]"
//	codex  -a zzz --version                  # exit 2, "[possible values: ...]"
//
// missing = a value we emit that upstream dropped -> breakage.
// extra   = a posture upstream gained that our rung mapping cannot express ->
// a real parity gap, reported under its own prefix so an unattended upstream
// release visibly reds the suite instead of passing silently.
func TestConformance_PermissionAllowedSets(t *testing.T) {
	requirePermissionConformance(t)

	// A value no binary would ever accept, so the probe always takes the
	// rejection path that prints the enumeration.
	const bogus = "zzz"

	probed := 0
	for _, h := range permissionConformanceHarnesses() {
		bin, ok := lookupHarnessBinary(t, h.versionsKey)
		if !ok {
			continue
		}
		probed++
		t.Run(h.wrapper, func(t *testing.T) {
			for _, p := range h.allowedSets {
				t.Run(strings.TrimLeft(p.flag, "-"), func(t *testing.T) {
					out, code := runVersionProbe(t, bin, p.flag, bogus)
					if code == 0 {
						t.Fatalf("PERMISSION FLAG DRIFT: %s %s: the binary ACCEPTED the bogus value %q — the probe can no longer observe the allowed set (a --help-style short-circuit, or the flag stopped validating). Output:\n%s\nSee %s.",
							h.wrapper, p.label, bogus, out, driftDoc)
					}
					m := p.parse.FindStringSubmatch(out)
					if m == nil {
						t.Fatalf("PERMISSION FLAG DRIFT: %s %s: could not parse an allowed-value enumeration out of the rejection output (pattern %q). Raw output:\n%s\nSee %s.",
							h.wrapper, p.label, p.parse.String(), out, driftDoc)
					}
					got := splitAllowedValues(m[1])
					if len(got) == 0 {
						t.Fatalf("PERMISSION FLAG DRIFT: %s %s: parsed an EMPTY allowed set out of %q. Raw output:\n%s\nSee %s.",
							h.wrapper, p.label, m[1], out, driftDoc)
					}
					missing, extra := diffSets(p.want, got)
					if len(missing) > 0 {
						t.Errorf("PERMISSION FLAG DRIFT: %s %s: value(s) we emit were DROPPED upstream: %s.\n  expected: %s\n  reported: %s\nSee %s.",
							h.wrapper, p.label, strings.Join(missing, ", "),
							strings.Join(sorted(p.want), ", "), strings.Join(sorted(got), ", "), driftDoc)
					}
					if len(extra) > 0 {
						t.Errorf("PERMISSION FLAG DRIFT (upstream added): %s %s: value(s) upstream now accepts that our rung mapping cannot express: %s.\n  expected: %s\n  reported: %s\nSee %s.",
							h.wrapper, p.label, strings.Join(extra, ", "),
							strings.Join(sorted(p.want), ", "), strings.Join(sorted(got), ", "), driftDoc)
					}
					if len(missing) == 0 && len(extra) == 0 {
						t.Logf("%s %s: upstream enumerates exactly %s", h.wrapper, p.label, strings.Join(sorted(got), ", "))
					}
				})
			}
		})
	}
	if probed == 0 {
		t.Skip("no harness binaries installed — nothing to probe")
	}
}

// TestConformance_BypassFlagsExist probes the two blanket bypass flags. Neither
// is ever EMITTED by argsWithHarnessPermissionMode, but both are hardcoded in
// production: SkipPermissionsFlag by validatePermissionMode's contradiction
// check and by BypassEnablingFlags, codexBypassFlag by codex's whole-directive
// suppression. A rename upstream silently defeats both.
func TestConformance_BypassFlagsExist(t *testing.T) {
	requirePermissionConformance(t)

	cases := []struct {
		harness     string
		versionsKey string
		flag        string
	}{
		{harness: "claude", versionsKey: "claude-code", flag: SkipPermissionsFlag},
		{harness: "codex", versionsKey: "codex", flag: codexBypassFlag},
	}

	probed := 0
	for _, c := range cases {
		bin, ok := lookupHarnessBinary(t, c.versionsKey)
		if !ok {
			continue
		}
		probed++
		t.Run(c.harness, func(t *testing.T) {
			out, code := runVersionProbe(t, bin, c.flag)
			if code != 0 {
				t.Errorf("PERMISSION FLAG DRIFT: %s: the installed binary REJECTED %s, which production hardcodes.\n  probed: %s %s --version\n  exit:   %d\n  output: %q\nSee %s.",
					c.harness, c.flag, bin, c.flag, code, strings.TrimSpace(out), driftDoc)
				return
			}
			t.Logf("%s: %s still exists", c.harness, c.flag)
		})
	}
	if probed == 0 {
		t.Skip("no harness binaries installed — nothing to probe")
	}
}

// TestPermissionConformanceParsers is the one HERMETIC test in this file: it
// pins the two enumeration parsers against the exact upstream error text
// recorded from claude 2.1.217 / codex-cli 0.144.5. Without it a parser
// regression would only ever surface as an "unparseable enumeration" failure on
// a gated nightly, long after the commit that caused it. Ungated on purpose —
// no binary is involved.
func TestPermissionConformanceParsers(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		const out = "error: option '--permission-mode <mode>' argument 'zzz' is invalid. " +
			"Allowed choices are acceptEdits, auto, bypassPermissions, manual, dontAsk, plan.\n"
		m := claudeAllowedRE.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("claudeAllowedRE did not match %q", out)
		}
		want := []string{claudeModeAcceptEdits, permissionModeAuto, claudeModeBypassPermissions, permissionModeManual, claudeModeDontAsk, permissionModePlan}
		if missing, extra := diffSets(want, splitAllowedValues(m[1])); len(missing) > 0 || len(extra) > 0 {
			t.Errorf("parsed set mismatch: missing=%v extra=%v", missing, extra)
		}
	})
	t.Run("clap", func(t *testing.T) {
		const out = "error: invalid value 'zzz' for '--sandbox <SANDBOX_MODE>'\n" +
			"  [possible values: read-only, workspace-write, danger-full-access]\n"
		m := clapAllowedRE.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("clapAllowedRE did not match %q", out)
		}
		want := []string{codexSandboxReadOnly, codexSandboxWorkspaceWrite, codexSandboxDangerFullAccess}
		if missing, extra := diffSets(want, splitAllowedValues(m[1])); len(missing) > 0 || len(extra) > 0 {
			t.Errorf("parsed set mismatch: missing=%v extra=%v", missing, extra)
		}
	})
	t.Run("unparseable", func(t *testing.T) {
		// The failure the gated tests must report loudly rather than skip.
		if m := clapAllowedRE.FindStringSubmatch("error: invalid value 'zzz'"); m != nil {
			t.Errorf("clapAllowedRE matched an enumeration-free message: %q", m)
		}
		if m := claudeAllowedRE.FindStringSubmatch("error: unknown option '--permission-mode'"); m != nil {
			t.Errorf("claudeAllowedRE matched an enumeration-free message: %q", m)
		}
	})
	t.Run("both directions", func(t *testing.T) {
		missing, extra := diffSets([]string{"a", "b"}, []string{"b", "c"})
		if len(missing) != 1 || missing[0] != "a" {
			t.Errorf("missing = %v, want [a]", missing)
		}
		if len(extra) != 1 || extra[0] != "c" {
			t.Errorf("extra = %v, want [c]", extra)
		}
	})
	t.Run("probe sets derive the codex+plan exclusion", func(t *testing.T) {
		for _, h := range permissionConformanceHarnesses() {
			modes := h.probeModes()
			if len(modes) == 0 {
				t.Fatalf("%s: probeModes is empty", h.wrapper)
			}
			hasPlan := false
			for _, m := range modes {
				if m == permissionModePlan {
					hasPlan = true
				}
			}
			if want := h.wrapper != "codex"; hasPlan != want {
				t.Errorf("%s: probeModes contains %q = %v, want %v (validatePermissionMode decides, not a hardcoded list)",
					h.wrapper, permissionModePlan, hasPlan, want)
			}
		}
	})
}

// splitAllowedValues turns a captured enumeration body ("a, b, c") into trimmed
// values, dropping empties so a trailing comma cannot fabricate a member.
func splitAllowedValues(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// diffSets compares want and got as SETS, returning what want has that got lacks
// (missing) and what got has that want lacks (extra), each sorted.
func diffSets(want, got []string) (missing, extra []string) {
	inGot := make(map[string]bool, len(got))
	for _, v := range got {
		inGot[v] = true
	}
	inWant := make(map[string]bool, len(want))
	for _, v := range want {
		inWant[v] = true
	}
	for v := range inWant {
		if !inGot[v] {
			missing = append(missing, v)
		}
	}
	for v := range inGot {
		if !inWant[v] {
			extra = append(extra, v)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// sorted returns a sorted copy, so a drift report reads the same on every run
// regardless of map iteration or upstream ordering.
func sorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
