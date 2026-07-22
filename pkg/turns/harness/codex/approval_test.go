package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// approvalCorpusScreen renders one of the live 0.144.4 approval recordings
// through the emulator at the recorded geometry (120x40) and returns the
// snapshot text.
//
// Unlike corpusBytes, a MISSING recording is a FATAL error rather than a skip:
// these two fixtures are the only evidence the gate matches the real dialog, so
// a lost fixture must go red instead of silently greening the suite.
func approvalCorpusScreen(t *testing.T, scenario string) string {
	t.Helper()
	wd, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		p := filepath.Join(wd, "test/corpus/codex", scenario, "bytes.raw")
		if data, err := os.ReadFile(p); err == nil {
			scr := screen.New(120, 40)
			_, _ = scr.Write(data)
			return scr.Snapshot().Text
		}
		wd = filepath.Dir(wd)
	}
	t.Fatalf("test/corpus/codex/%s/bytes.raw not found; it is REQUIRED (it is the only "+
		"evidence the approval gate matches a real codex dialog) — restore it from "+
		"meta-harness test/corpus/codex/%s", scenario, scenario)
	return ""
}

// approvalScreen is a hand-written approval screen in the live corpus shape
// (anchor, body, a "›"-highlighted menu, footer). Used for the gate/ordering
// pins that need to vary ONE element at a time; the corpus recordings pin the
// real thing.
func approvalScreen(body string, menu []string) string {
	if menu == nil {
		menu = []string{
			"› 1. Yes, proceed (y)",
			"  2. Yes, and don't ask again (p)",
			"  3. No, and tell Codex what to do differently (esc)",
		}
	}
	lines := []string{
		"• Running touch /tmp/probe",
		"",
		"  Would you like to run the following command?",
		"",
	}
	if body != "" {
		lines = append(lines, "  "+body, "")
	}
	lines = append(lines, "  $ touch /tmp/probe", "")
	lines = append(lines, menu...)
	lines = append(lines, "", "  Press enter to confirm or esc to cancel")
	return strings.Join(lines, "\n")
}

// ── Corpus: the real dialogs ────────────────────────────────────────────────

func TestDetectInput_ApprovalCommandCorpus(t *testing.T) {
	req, ok := DetectInput(approvalCorpusScreen(t, "approval-command"))
	if !ok {
		t.Fatal("DetectInput did not recognize the live shell-command approval dialog")
	}
	if req.Kind != KindApproval {
		t.Errorf("Kind = %q, want KindApproval", req.Kind)
	}
	// The literal is pinned by pkg/chat/codex_dismiss_test.go and by orche's
	// default handler contract — assert it independently of the constant so a
	// rename of the constant's VALUE cannot slip through.
	if req.Kind != "approval_prompt" {
		t.Errorf("Kind = %q, want the pinned literal %q", req.Kind, "approval_prompt")
	}
	if want := "Would you like to run the following command?"; req.Prompt != want {
		t.Errorf("Prompt = %q, want %q", req.Prompt, want)
	}
	if req.ID == "" {
		t.Error("request ID is empty")
	}

	want := []struct {
		id, alias string
		keys      string
		hl        bool
	}{
		{"1", "proceed", "1\r", true},
		{"2", "proceed", "2\r", false},
		{"3", "deny", "3\r", false},
	}
	if len(req.Options) != len(want) {
		t.Fatalf("got %d options, want %d: %+v", len(req.Options), len(want), req.Options)
	}
	for i, w := range want {
		o := req.Options[i]
		if o.ID != w.id || o.Alias != w.alias || string(o.Keys) != w.keys || o.Highlighted != w.hl {
			t.Errorf("Options[%d] = {id:%q alias:%q keys:%q hl:%v label:%q}, want {id:%q alias:%q keys:%q hl:%v}",
				i, o.ID, o.Alias, string(o.Keys), o.Highlighted, o.Label, w.id, w.alias, w.keys, w.hl)
		}
	}
	if got := req.Options[0].Label; got != "Yes, proceed (y)" {
		t.Errorf("Options[0].Label = %q, want %q", got, "Yes, proceed (y)")
	}
	if got := req.Options[2].Label; got != "No, and tell Codex what to do differently (esc)" {
		t.Errorf("Options[2].Label = %q, want the deny row's live wording", got)
	}
}

func TestDetectInput_ApprovalPatchCorpus(t *testing.T) {
	req, ok := DetectInput(approvalCorpusScreen(t, "approval-patch"))
	if !ok {
		t.Fatal("DetectInput did not recognize the live apply-patch approval dialog")
	}
	if req.Kind != KindApproval {
		t.Errorf("Kind = %q, want KindApproval", req.Kind)
	}
	if want := "Would you like to make the following edits?"; req.Prompt != want {
		t.Errorf("Prompt = %q, want %q", req.Prompt, want)
	}
	if req.ID == "" {
		t.Error("request ID is empty")
	}
	var aliases, keys []string
	highlighted := 0
	for i, o := range req.Options {
		aliases = append(aliases, o.Alias)
		keys = append(keys, string(o.Keys))
		if o.Highlighted {
			highlighted++
			if i != 0 {
				t.Errorf("Options[%d] carries the '›' highlight; only the live selector row (0) should", i)
			}
		}
	}
	if strings.Join(aliases, ",") != "proceed,proceed,deny" {
		t.Errorf("aliases = %v, want [proceed proceed deny]", aliases)
	}
	if strings.Join(keys, ",") != "1\r,2\r,3\r" {
		t.Errorf("keys = %q, want [1\\r 2\\r 3\\r]", keys)
	}
	if highlighted != 1 {
		t.Errorf("%d rows highlighted, want exactly 1", highlighted)
	}
}

func TestDetectInput_ApprovalIDsAreDistinctAndStable(t *testing.T) {
	cmd, ok := DetectInput(approvalCorpusScreen(t, "approval-command"))
	if !ok {
		t.Fatal("command dialog not detected")
	}
	patch, ok := DetectInput(approvalCorpusScreen(t, "approval-patch"))
	if !ok {
		t.Fatal("patch dialog not detected")
	}
	if cmd.ID == patch.ID {
		t.Errorf("the two dialogs share id %q; they must be distinct", cmd.ID)
	}
	// Stable across a redraw of the same screen — this is what drives the
	// adapter's InputRequested/InputResolved diffing.
	again, _ := DetectInput(approvalCorpusScreen(t, "approval-command"))
	if again.ID != cmd.ID {
		t.Errorf("re-detect changed the id: %q -> %q", cmd.ID, again.ID)
	}
}

// TestAutoDismiss_NeverTouchesApproval pins the TURNS-level switch (the
// chat-level half is pinned by pkg/chat/codex_dismiss_test.go) so a future
// refactor of AutoDismissKeys cannot regress a real approval into an
// auto-approval. AutoDismissKeys' default arm is what protects this today.
func TestAutoDismiss_NeverTouchesApproval(t *testing.T) {
	req, ok := DetectInput(approvalCorpusScreen(t, "approval-command"))
	if !ok {
		t.Fatal("command dialog not detected")
	}
	keys, ok := AutoDismissKeys(req)
	if ok {
		t.Errorf("AutoDismissKeys accepted a genuine approval dialog (would auto-approve), keys=%q", keys)
	}
	if keys != nil {
		t.Errorf("AutoDismissKeys returned keys %q for an approval; want nil", keys)
	}
}

func TestDetectInput_SyntheticApprovalMatchesCorpusShape(t *testing.T) {
	req, ok := DetectInput(approvalScreen("", nil))
	if !ok {
		t.Fatal("the synthetic approval screen did not classify; the gate pins below would be vacuous")
	}
	if req.Kind != KindApproval {
		t.Errorf("Kind = %q, want KindApproval", req.Kind)
	}
}

// ── Ordering pins: KindApproval is checked before every interstitial ────────

func TestDetectInput_ApprovalOutranksInterstitials(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		// continueAnchor → KindNotice auto-dismisses with a bare "\r", which on
		// an approval dialog would press Enter on the highlighted "Yes".
		{"continue anchor", "Press enter to continue"},
		// The updateAnchor branch return-(nil,false)s the WHOLE function when its
		// skip gate fails, which would swallow this dialog entirely.
		{"update anchor", "Update available! 1.0 -> 2.0"},
		{"migration anchor", "Choose how you'd like Codex to proceed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, ok := DetectInput(approvalScreen(tc.body, nil))
			if !ok {
				t.Fatalf("an approval body quoting %q was not classified at all", tc.body)
			}
			if req.Kind != KindApproval {
				t.Errorf("Kind = %q, want KindApproval (an approval body quoting %q)", req.Kind, tc.body)
			}
			if _, dismissable := AutoDismissKeys(req); dismissable {
				t.Errorf("an approval body quoting %q became auto-dismissable — that is auto-approval", tc.body)
			}
		})
	}
}

// ── The mandatory-strict gate ──────────────────────────────────────────────

func TestDetectApproval_GateRequirements(t *testing.T) {
	// A false positive here is WORSE than the miss it replaces: readiness would
	// be suppressed and the turn would deadlock. So each leg of the gate is
	// pinned independently.
	t.Run("requires a '›' highlight on a parsed menu row", func(t *testing.T) {
		if req, ok := DetectInput(approvalScreen("", []string{
			"  1. Yes, proceed (y)",
			"  2. No, and tell Codex what to do (esc)",
		})); ok {
			t.Errorf("classified %q with no highlighted row", req.Kind)
		}
	})
	t.Run("requires a proceed-aliased row", func(t *testing.T) {
		if req, ok := DetectInput(approvalScreen("", []string{
			"› 1. Maybe later",
			"  2. No, cancel that",
		})); ok {
			t.Errorf("classified %q with no proceed row", req.Kind)
		}
	})
	t.Run("requires a deny-aliased row", func(t *testing.T) {
		if req, ok := DetectInput(approvalScreen("", []string{
			"› 1. Yes, proceed (y)",
			"  2. Tell me more",
		})); ok {
			t.Errorf("classified %q with no deny row", req.Kind)
		}
	})
	t.Run("requires the anchor", func(t *testing.T) {
		s := strings.Replace(approvalScreen("", nil), "Would you like to run", "Maybe run", 1)
		if req, ok := DetectInput(s); ok {
			t.Errorf("classified %q with no approval anchor", req.Kind)
		}
	})
}

// ── Adversarial: assistant prose that quotes the anchor ─────────────────────

// proseSpoof is an assistant reply that quotes the approval anchor AND
// enumerates proceed/deny-shaped rows, with NO "›" highlight on those rows —
// plus, higher up the screen, a scrollback echo of a past prompt that itself
// began with a number (codex renders past prompts as "›"-prefixed rows).
//
// This shape is why the gate must key on the highlight of a PARSED MENU row
// taken from the ANCHOR TAIL. A screen-wide highlight regex matches the echo and
// false-positives; so does a whole-screen row parse when the echo's digit does
// not collide with the enumerated ones.
func proseSpoof(echo string) string {
	return strings.Join([]string{
		echo,
		"",
		"• Codex asks for approval before running a command. It prints:",
		`    "Would you like to run the following command?"`,
		"  and then offers you:",
		"    1. Yes, run it",
		"    2. No, cancel that",
		"",
		"› ",
	}, "\n")
}

func TestDetectApproval_ProseSpoofIsNotAnApproval(t *testing.T) {
	cases := []struct{ name, echo string }{
		{"no scrollback echo", "› Explain the approval flow"},
		// The echo's digit COLLIDES with the enumeration — digit dedup covers it.
		{"'› 1. …' echo (digit collides)", "› 1. Explain the approval flow"},
		// The echo's digit does NOT collide, so dedup does not drop it: the echo
		// is itself a parsed row and lent its highlight to the gate until
		// detectApproval was scoped to the anchor tail. This is the case only
		// anchor-tail scoping catches.
		{"'› 4. …' echo (digit does not collide)", "› 4. Explain the approval flow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if req, ok := DetectInput(proseSpoof(tc.echo)); ok {
				t.Errorf("prose quoting the anchor classified as %q — a false positive here "+
					"deadlocks the turn", req.Kind)
			}
		})
	}
}

// ── Alias pins ─────────────────────────────────────────────────────────────

func TestApprovalOptionAliases(t *testing.T) {
	// aliasForLabel is package-private but the meaningful contract is what
	// DetectInput's parsed rows carry, so drive it through the detector.
	aliasesFor := func(t *testing.T, menu []string) []string {
		t.Helper()
		req, ok := DetectInput(approvalScreen("", menu))
		if !ok {
			t.Fatalf("menu did not classify as an approval: %v", menu)
		}
		var out []string
		for _, o := range req.Options {
			out = append(out, o.Alias)
		}
		return out
	}

	t.Run("bare 'Yes' / 'No' labels alias proceed / deny", func(t *testing.T) {
		// The pinned chat contract fixture carries exactly these labels. Bare
		// "No" lowercases to "no", which the comma/space-suffixed deny tokens
		// deliberately miss (so they cannot match "now"/"notice") — hence the
		// exact-match case in aliasForLabel. Without it the deny-row gate would
		// reject a real dialog rendering "2. No".
		got := aliasesFor(t, []string{"› 1. Yes", "  2. No"})
		if strings.Join(got, ",") != "proceed,deny" {
			t.Errorf("aliases = %v, want [proceed deny]", got)
		}
	})

	t.Run("longer yes/no phrasings alias proceed / deny", func(t *testing.T) {
		got := aliasesFor(t, []string{
			"› 1. Yes, proceed (y)",
			"  2. No, and tell Codex what to do (esc)",
		})
		if strings.Join(got, ",") != "proceed,deny" {
			t.Errorf("aliases = %v, want [proceed deny]", got)
		}
		got = aliasesFor(t, []string{"› 1. Accept the change", "  2. Reject it"})
		if strings.Join(got, ",") != "proceed,deny" {
			t.Errorf("aliases = %v, want [proceed deny]", got)
		}
	})
}

// TestAliasForLabel_InterstitialTokensStillWinFirst pins the ORDER inside
// aliasForLabel: the approval vocabulary is tested AFTER skip/update so
// interstitial classification stays byte-identical to before it existed. That
// ordering is load-bearing — the update menu's auto-dismiss selects the row
// whose alias is "skip", so a "Skip" row that started aliasing something from
// the approval vocabulary would make AutoDismissKeys refuse the update notice
// (or, worse, leave the highlighted "Update now" as the only option).
func TestAliasForLabel_InterstitialTokensStillWinFirst(t *testing.T) {
	for label, want := range map[string]string{
		"Skip":                     "skip",
		"Skip until next version":  "skip",
		"Update now (runs npm i)":  "update",
		"Yes, proceed (y)":         "proceed",
		"No":                       "deny",
		"No, and tell Codex (esc)": "deny",
		"Tell me more":             "",
		// "now" / "notice" must NOT trip the deny tokens.
		"Not now, thanks later": "",
		"Notice of changes":     "",
	} {
		if got := aliasForLabel(label); got != want {
			t.Errorf("aliasForLabel(%q) = %q, want %q", label, got, want)
		}
	}
}

// ── Adapter transitions ────────────────────────────────────────────────────

// TestCodexAdapter_ApprovalRequestedThenResolved pins the adapter's event
// sequence for a genuine approval: the dialog appearing emits exactly one
// InputRequested carrying KindApproval, a redraw is silent, and the dialog
// clearing emits exactly one InputResolved with the SAME id and kind (so a
// client subscribed from the start can correlate them).
func TestCodexAdapter_ApprovalRequestedThenResolved(t *testing.T) {
	scr := screen.New(120, 40)
	a := New()

	dialog := "\x1b[H\x1b[2J" + strings.ReplaceAll(approvalScreen("", nil), "\n", "\r\n") + "\r\n"
	_, _ = scr.Write([]byte(dialog))
	evs := a.OnScreen(scr.Snapshot())
	if len(evs) != 1 {
		t.Fatalf("dialog frame: want exactly 1 event, got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != turns.InputRequested || evs[0].Input == nil || evs[0].Input.Kind != KindApproval {
		t.Fatalf("dialog frame: want InputRequested(approval_prompt), got %+v", evs[0])
	}
	id := evs[0].Input.ID
	if id == "" {
		t.Fatal("InputRequested carried an empty request id")
	}

	// Redraw of the same dialog → nothing (the id is unchanged).
	_, _ = scr.Write([]byte(dialog))
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 0 {
		t.Errorf("redraw: want 0 events, got %d: %+v", len(evs), evs)
	}

	// The user answered: a dialog-free idle composer → one InputResolved.
	_, _ = scr.Write([]byte("\x1b[H\x1b[2J  Done.\r\n\r\n› \r\n"))
	evs = a.OnScreen(scr.Snapshot())
	if len(evs) != 1 {
		t.Fatalf("cleared frame: want exactly 1 event, got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != turns.InputResolved || evs[0].Input == nil {
		t.Fatalf("cleared frame: want InputResolved, got %+v", evs[0])
	}
	if evs[0].Input.Kind != KindApproval || evs[0].Input.ID != id {
		t.Errorf("resolve = {kind:%q id:%q}, want {kind:%q id:%q}",
			evs[0].Input.Kind, evs[0].Input.ID, KindApproval, id)
	}

	// Nothing re-fires once resolved.
	if evs := a.OnScreen(scr.Snapshot()); len(evs) != 0 {
		t.Errorf("post-resolve: want 0 events, got %d: %+v", len(evs), evs)
	}
}
