package claudecode

import (
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

// selectorTrustFrame is claude 2.1.251's folder-trust dialog as captured live
// (tmux, 2026-08-29 — see PUPPET-236). Every line matters: the workspace path
// and "Security guide" sit at column 0 and must NOT become options, the footer
// hint must terminate the block, and the default highlight is on "No, exit" —
// so the affirmative row is reached with a DOWN arrow, never a bare CR.
const selectorTrustFrame = "Accessing workspace:\n" +
	"/private/tmp/trustrepo\n" +
	"Quick safety check: Is this a project you created or one you trust? …\n" +
	"Claude Code'll be able to read, edit, and execute files here.\n" +
	"Security guide\n" +
	" ❯ No, exit\n" +
	"   Yes, I trust this folder\n" +
	"Enter to confirm · Esc to cancel\n"

// selectorTrustFrameSecondRow is the same dialog with the highlight moved down
// one row (what the screen looks like after the user, or a previous keypress,
// has navigated). The affirmative row is now the highlighted one and the DENY
// row is reached with an UP arrow.
const selectorTrustFrameSecondRow = "Accessing workspace:\n" +
	"/private/tmp/trustrepo\n" +
	"Quick safety check: Is this a project you created or one you trust? …\n" +
	"Security guide\n" +
	"   No, exit\n" +
	" ❯ Yes, I trust this folder\n" +
	"Enter to confirm · Esc to cancel\n"

// selectorTrustFrameBoxed is the same shape inside the bordered box the vt100
// emulator renders for some builds: the left edge shifts every line, so the
// label column is only stable once the border is stripped.
const selectorTrustFrameBoxed = "╭──────────────────────────────────────────────╮\n" +
	"│ Do you trust the files in this folder?       │\n" +
	"│                                              │\n" +
	"│ /Users/oleh/Work/aether/harness-wrapper      │\n" +
	"│                                              │\n" +
	"│ ❯ No, exit                                   │\n" +
	"│   Yes, I trust this folder                   │\n" +
	"│                                              │\n" +
	"│ Enter to confirm · Esc to cancel             │\n" +
	"╰──────────────────────────────────────────────╯\n"

// afterAnchor slices a frame the way DetectInputDetail does, so the parser
// tests see exactly the scope production gives it.
func afterAnchor(t *testing.T, frame string) string {
	t.Helper()
	_, after, ok := anchorSplit(frame)
	if !ok {
		t.Fatalf("fixture carries no known dialog anchor:\n%s", frame)
	}
	return after
}

type wantOption struct {
	id, alias, label, keys string
	highlighted            bool
}

func TestParseSelectorMenu(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
		want  []wantOption
	}{
		{
			name:  "live 2.1.251 capture, highlight on the deny row",
			frame: selectorTrustFrame,
			want: []wantOption{
				{id: "0", alias: "deny", label: "No, exit", keys: "\r", highlighted: true},
				{id: "1", alias: "proceed", label: "Yes, I trust this folder", keys: "\x1b[B\r"},
			},
		},
		{
			name:  "highlight on the second row: the other choice needs an UP arrow",
			frame: selectorTrustFrameSecondRow,
			want: []wantOption{
				{id: "0", alias: "deny", label: "No, exit", keys: "\x1b[A\r"},
				{id: "1", alias: "proceed", label: "Yes, I trust this folder", keys: "\r", highlighted: true},
			},
		},
		{
			name:  "bordered box: the left edge must not shift the label column",
			frame: selectorTrustFrameBoxed,
			want: []wantOption{
				{id: "0", alias: "deny", label: "No, exit", keys: "\r", highlighted: true},
				{id: "1", alias: "proceed", label: "Yes, I trust this folder", keys: "\x1b[B\r"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := parseSelectorMenu(afterAnchor(t, tc.frame))
			if len(opts) != len(tc.want) {
				t.Fatalf("len(options) = %d, want %d (%+v)", len(opts), len(tc.want), opts)
			}
			for i, w := range tc.want {
				o := opts[i]
				if o.ID != w.id || o.Alias != w.alias || o.Label != w.label ||
					string(o.Keys) != w.keys || o.Highlighted != w.highlighted {
					t.Errorf("options[%d] = {id:%q alias:%q label:%q keys:%q highlighted:%v}, "+
						"want {id:%q alias:%q label:%q keys:%q highlighted:%v}",
						i, o.ID, o.Alias, o.Label, o.Keys, o.Highlighted,
						w.id, w.alias, w.label, w.keys, w.highlighted)
				}
			}
		})
	}
}

// TestParseSelectorMenu_NeverABareCRForANonHighlightedRow is the property the
// whole positional-keys design exists for: claude highlights "No, exit", so a
// bare CR on the affirmative row would QUIT the CLI at startup. Asserted
// separately from the table so it cannot be lost in a fixture edit.
func TestParseSelectorMenu_NeverABareCRForANonHighlightedRow(t *testing.T) {
	for _, frame := range []string{selectorTrustFrame, selectorTrustFrameSecondRow, selectorTrustFrameBoxed} {
		for _, o := range parseSelectorMenu(afterAnchor(t, frame)) {
			if !o.Highlighted && string(o.Keys) == "\r" {
				t.Fatalf("option %q is not highlighted but its keys are a bare CR — "+
					"answering it would confirm whatever row claude has highlighted instead", o.Label)
			}
			if o.Highlighted && string(o.Keys) != "\r" {
				t.Errorf("highlighted option %q has keys %q, want a bare CR", o.Label, o.Keys)
			}
		}
	}
}

// TestParseSelectorMenu_ArrowsAreOneWrite pins that a row's keys are a single
// contiguous sequence ending in CR. Conversation.write passes the whole
// opt.Keys to one WriteStdin: "ESC [ B" in one write parses as Down, but split
// across writes it is a lone Esc, which CANCELS the dialog.
func TestParseSelectorMenu_ArrowsAreOneWrite(t *testing.T) {
	for _, o := range parseSelectorMenu(afterAnchor(t, selectorTrustFrame)) {
		keys := string(o.Keys)
		if !strings.HasSuffix(keys, "\r") {
			t.Errorf("option %q keys %q do not end in CR", o.Label, keys)
		}
		body := strings.TrimSuffix(keys, "\r")
		if body != "" && body != strings.Repeat("\x1b[B", strings.Count(body, "\x1b[B")) &&
			body != strings.Repeat("\x1b[A", strings.Count(body, "\x1b[A")) {
			t.Errorf("option %q keys %q are not a pure arrow run + CR", o.Label, keys)
		}
	}
}

func TestParseSelectorMenu_Rejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after string
	}{
		{
			name:  "no selector glyph at all",
			after: "\nSecurity guide\n  No, exit\n  Yes, I trust this folder\n",
		},
		{
			name:  "a single row is not a choice (a stray composer glyph)",
			after: "\nSecurity guide\n ❯ No, exit\nEnter to confirm · Esc to cancel\n",
		},
		{
			name:  "more rows than the cap: prose that happens to align",
			after: "\n" + " ❯ row one\n" + strings.Repeat("   filler row\n", 9),
		},
		{
			name:  "glyph with nothing after it",
			after: "\n ❯ \n   Yes, I trust this folder\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if opts := parseSelectorMenu(tc.after); opts != nil {
				t.Fatalf("parseSelectorMenu returned %d options, want nil (unparseable): %+v", len(opts), opts)
			}
		})
	}
}

// TestParseSelectorMenu_ProseIsNotAnOption is the over-matching guard called out
// in the design: "Security guide" and the workspace path sit next to the choice
// rows and would become spurious options — with an empty Alias, and worse,
// shifting the arrow offsets of every real row below them.
func TestParseSelectorMenu_ProseIsNotAnOption(t *testing.T) {
	opts := parseSelectorMenu(afterAnchor(t, selectorTrustFrame))
	for _, o := range opts {
		for _, prose := range []string{"Security guide", "/private/tmp/trustrepo", "Enter to confirm", "Claude Code'll"} {
			if strings.Contains(o.Label, prose) {
				t.Errorf("prose line %q was parsed as option %q", prose, o.Label)
			}
		}
		if o.Alias == "" {
			t.Errorf("option %q has no alias; only prose parses that way", o.Label)
		}
	}
}

func TestDetectInputDetail_States(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		want  Detection
		wantN int // expected option count when DetectOK
	}{
		{
			name: "no anchor",
			text: "Claude Code\n\n❯ \n",
			want: DetectNone,
		},
		{
			name: "anchor only, menu not painted yet",
			text: "Quick safety check: Is this a project you created or one you trust? …\n",
			want: DetectPending,
		},
		{
			name: "anchor plus a choice-shaped line the block rules reject",
			text: "Quick safety check: Is this a project you created or one you trust? …\n" +
				"Security guide\n ❯ No, exit\nEnter to confirm · Esc to cancel\n",
			want: DetectUnparseable,
		},
		{
			name:  "the real 2.1.251 frame",
			text:  selectorTrustFrame,
			want:  DetectOK,
			wantN: 2,
		},
		{
			name:  "the numbered frame still parses as before",
			text:  trustScreen,
			want:  DetectOK,
			wantN: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, det := DetectInputDetail(tc.text)
			if det != tc.want {
				t.Fatalf("Detection = %v, want %v (req %+v)", det, tc.want, req)
			}
			if tc.want != DetectOK {
				if req != nil {
					t.Errorf("request = %+v, want nil for %v", req, det)
				}
				// DetectInput must map every non-OK state to false — that is the
				// contract pkg/oneshot and the InputRequested path rely on.
				if _, ok := DetectInput(tc.text); ok {
					t.Error("DetectInput = true for a state that is not DetectOK")
				}
				return
			}
			if len(req.Options) != tc.wantN {
				t.Fatalf("len(Options) = %d, want %d", len(req.Options), tc.wantN)
			}
		})
	}
}

// TestDetectInputDetail_IDStableAcrossHighlightMoves pins that moving the
// highlight does NOT mint a new request id. inputID hashes kind + prompt +
// option LABELS only; if the highlight leaked into it, every arrow keypress
// would look like a fresh dialog the policy answers all over again.
func TestDetectInputDetail_IDStableAcrossHighlightMoves(t *testing.T) {
	a, detA := DetectInputDetail(selectorTrustFrame)
	b, detB := DetectInputDetail(selectorTrustFrameSecondRow)
	if detA != DetectOK || detB != DetectOK {
		t.Fatalf("detections = %v / %v, want both DetectOK", detA, detB)
	}
	if a.ID != b.ID {
		t.Errorf("request id changed when the highlight moved: %q vs %q", a.ID, b.ID)
	}
	// The KEYS must differ, though — that is the whole point.
	if string(a.Options[1].Keys) == string(b.Options[1].Keys) {
		t.Errorf("keys did not follow the highlight: both %q", a.Options[1].Keys)
	}
}

// TestOnScreen_UnparseableDialogErrorsOnce covers the adapter half: an
// unreadable blocking dialog emits exactly one Errored naming the anchor, stays
// quiet across redraws of the same frame, and never fabricates an InputResolved
// for a request that was never requested.
func TestOnScreen_UnparseableDialogErrorsOnce(t *testing.T) {
	const frame = "Quick safety check: Is this a project you created or one you trust? …\n" +
		"Security guide\n ❯ No, exit\nEnter to confirm · Esc to cancel\n"

	a := New()
	var errored, other []turns.Event
	for i := 0; i < 3; i++ {
		for _, ev := range a.OnScreen(screen.Snapshot{Text: frame}) {
			if ev.Kind == turns.Errored {
				errored = append(errored, ev)
			} else {
				other = append(other, ev)
			}
		}
	}
	if len(errored) != 1 {
		t.Fatalf("got %d Errored events across 3 identical snapshots, want exactly 1: %+v", len(errored), errored)
	}
	if !strings.Contains(errored[0].Reason, "unrecognized blocking dialog") ||
		!strings.Contains(errored[0].Reason, trustAnchorAlt) {
		t.Errorf("reason = %q, want it to name the failure and the anchor %q", errored[0].Reason, trustAnchorAlt)
	}
	if !strings.Contains(errored[0].Reason, "No, exit") {
		t.Errorf("reason = %q, want the raw candidate lines as evidence", errored[0].Reason)
	}
	for _, ev := range other {
		if ev.Kind == turns.InputResolved || ev.Kind == turns.InputRequested {
			t.Errorf("unexpected %v event for a dialog that was never surfaced: %+v", ev.Kind, ev)
		}
	}
}

// TestOnScreen_UnparseableReportsAgainAfterClearing pins that the dedup
// fingerprint is per-occurrence, not per-process: a dialog that clears and comes
// back must report again, or a second untrusted repo in one session is silent.
func TestOnScreen_UnparseableReportsAgainAfterClearing(t *testing.T) {
	const frame = "Quick safety check: Is this a project you created or one you trust? …\n" +
		"Security guide\n ❯ No, exit\nEnter to confirm · Esc to cancel\n"

	a := New()
	count := func(evs []turns.Event) int {
		n := 0
		for _, ev := range evs {
			if ev.Kind == turns.Errored {
				n++
			}
		}
		return n
	}
	if n := count(a.OnScreen(screen.Snapshot{Text: frame})); n != 1 {
		t.Fatalf("first paint emitted %d Errored, want 1", n)
	}
	if n := count(a.OnScreen(screen.Snapshot{Text: "Claude Code\n\n❯ \n"})); n != 0 {
		t.Fatalf("cleared screen emitted %d Errored, want 0", n)
	}
	if n := count(a.OnScreen(screen.Snapshot{Text: frame})); n != 1 {
		t.Fatalf("second occurrence emitted %d Errored, want 1", n)
	}
}
