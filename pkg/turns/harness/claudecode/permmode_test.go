package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/olesho/harness-wrapper/pkg/screen"
	"github.com/olesho/harness-wrapper/pkg/turns"
)

var _ turns.PermissionModeDetector = (*Adapter)(nil)

// The six live footer markers (claude-code 2.1.217) and the canonical rung
// each must translate to. The marker is only the glyph + words + "on";
// everything after it is a separately-varying tail (see footerTails).
//
// Six markers, FIVE rungs: --permission-mode dontAsk paints its own footer word
// but shares the manual rung (claude's own permissiveness rank table ties
// dontAsk with its default, and the rung ladder is a strict total order).
var footerMarkers = []struct {
	marker string
	rung   string
}{
	{"⏸ plan mode on", "plan"},
	{"⏸ manual mode on", "manual"},
	{"⏵⏵ accept edits on", "ask"},
	{"⏵⏵ auto mode on", "auto"},
	{"⏵⏵ bypass permissions on", "bypass"},
	{"⏵⏵ don't ask on", "manual"},
}

// Every tail observed on disk. The "(shift+tab to cycle)" hint is optional for
// EVERY mode (auto renders suffix-less across busy_test.go,
// turncomplete_busy_test.go and pkg/chat/quiescence_test.go), and its trailing
// segment is swapped wholesale while Claude works ("esc to interrupt",
// busy_test.go:16) or while sub-agents run ("↓ to manage", busy_test.go:34).
var footerTails = []string{
	"",
	" · ← for agents",
	" · esc to interrupt",
	" · ↓ to manage",
	" (shift+tab to cycle)",
	" (shift+tab to cycle) · ← for agents",
	" (shift+tab to cycle) · esc to interrupt",
	" (shift+tab to cycle) · ↓ to manage",
	// The effort indicator shares the footer row in
	// test/corpus/claude-code/interrupted-mid-reply.
	" (shift+tab to cycle) · ← for agents          ○ low · /effort",
}

// TestPermissionModeFromFooter_matrix crosses every mode with every observed
// tail, in both the bare and the 120-column right-padded form pkg/screen
// actually produces.
func TestPermissionModeFromFooter_matrix(t *testing.T) {
	for _, m := range footerMarkers {
		for _, tail := range footerTails {
			line := "  " + m.marker + tail
			for _, text := range []string{
				line,
				padTo(line, 120),
				// As it appears in a real frame: transcript above, input box below.
				"⏺ Done.\n✻ Baked for 3s\n❯ \n" + padTo(line, 120) + "\n",
			} {
				got, ok := permissionModeFromFooter(text)
				if !ok {
					t.Errorf("permissionModeFromFooter(%q) = _, false; want %q, true", text, m.rung)
					continue
				}
				if got != m.rung {
					t.Errorf("permissionModeFromFooter(%q) = %q; want %q", text, got, m.rung)
				}
			}
		}
	}
}

// The footer is painted with absolute-column jumps, so the gap widths between
// tokens are emulator artifacts. Widened gaps must read identically.
func TestPermissionModeFromFooter_widenedColumnGaps(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{"⏵⏵   bypass    permissions   on   (shift+tab to cycle)", "bypass"},
		{"⏸\tmanual\tmode\ton · ← for agents", "manual"},
		{"⏵⏵  accept   edits  on", "ask"},
		{"⏵⏵   don't    ask   on   (shift+tab to cycle)", "manual"},
	} {
		got, ok := permissionModeFromFooter(tc.text)
		if !ok {
			t.Errorf("permissionModeFromFooter(%q) = _, false; want %q", tc.text, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("permissionModeFromFooter(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
}

// Negatives: the parser must return ("", false) rather than guess.
func TestPermissionModeFromFooter_negatives(t *testing.T) {
	for _, text := range []string{
		"",
		"❯ \n✻ Baked for 3s\n",
		// Out-of-alternation mode word: a renamed/new Claude mode degrades to
		// unknown, never to a wrong rung.
		"⏵⏵ turbo mode on (shift+tab to cycle) · ← for agents",
		"⏸ yolo mode on · ← for agents",
		// The glyph and the words, but not in the footer's shape.
		"⏸ manual permission mode",
		"⏵⏵ auto mode off (shift+tab to cycle)",
		// "on" must be a whole word, not a prefix of the next one.
		"⏵⏵ auto mode online",
		// Words without the glyph are prose, not a marker.
		"auto mode on (shift+tab to cycle) · ← for agents",
		// The closed alternation stays closed with the sixth row in place: a
		// SEVENTH mode word must still degrade to unknown.
		"⏵⏵ frobnicate mode on (shift+tab to cycle) · ← for agents",
		// Near-miss on the sixth row itself: "don't ask" is the whole marker,
		// so "don't ask again" is not it.
		"⏵⏵ don't ask again on (shift+tab to cycle)",
		// The real per-tool dialog row (permission_dialog_pin_test.go) contains
		// the words "don't ask" and, elsewhere on screen, plenty of prose. No
		// glyph, so it is not a footer.
		"  2. Yes, and don't ask again for npm commands",
	} {
		if got, ok := permissionModeFromFooter(text); ok {
			t.Errorf("permissionModeFromFooter(%q) = %q, true; want \"\", false", text, got)
		}
	}
}

// Corpus negative: Claude Code's own release notes render a ⏸ glyph plus the
// words "manual permission mode" as ordinary boxed prose on the model-picker
// screen. expected.txt IS the canonical pkg/screen render of that recording
// (test/corpus/models/README.md), so it is a legitimate parser input — and a
// future loosening of the pattern must trip here rather than in production.
func TestPermissionModeFromFooter_corpusProseIsNotAFooter(t *testing.T) {
	text := readCorpusFile(t, "test", "corpus", "models", "claude-code", "model-picker", "expected.txt")
	if !strings.Contains(text, "⏸") {
		t.Fatal("fixture no longer contains the ⏸ prose glyph this test guards")
	}
	if got, ok := permissionModeFromFooter(text); ok {
		t.Errorf("model-picker prose read as %q; want no permission mode", got)
	}
}

// Corpus negative: the first-run theme picker paints no footer at all, so the
// screen carries no readable signal.
func TestPermissionModeFromFooter_corpusNoFooter(t *testing.T) {
	text := readCorpusFile(t, "test", "corpus", "auth", "claude-code", "theme-picker", "screen.txt")
	if got, ok := permissionModeFromFooter(text); ok {
		t.Errorf("theme-picker screen read as %q; want no permission mode", got)
	}
}

// Corpus positive (auto): render the recorded PTY bytes through pkg/screen —
// only the emulator reassembles the footer's column jumps into a contiguous
// line. Deliberately NOT asserted against expected.txt: those goldens are
// bootstrapped from a selected emulator's NORMALIZED snapshot (ANSI stripped,
// tabs expanded, every line right-trimmed) at a geometry that need not match
// meta.json.
func TestAdapter_PermissionMode_corpusAuto(t *testing.T) {
	a := New()
	for _, name := range []string{"multi-turn", "tool-call", "interrupted-mid-reply"} {
		got, ok := a.PermissionMode(lastLiveFrame(t, name))
		if !ok {
			t.Errorf("[%s] PermissionMode = _, false; want %q, true", name, "auto")
			continue
		}
		if got != "auto" {
			t.Errorf("[%s] PermissionMode = %q; want %q", name, got, "auto")
		}
	}
}

// Corpus positive (manual, suffix-less): both not-logged-in auth screens render
// "  ⏸ manual mode on · ← for agents" with no "(shift+tab to cycle)" — the
// on-disk proof that the hint is optional for every mode, not just some.
func TestAdapter_PermissionMode_corpusManual(t *testing.T) {
	a := New()
	for _, name := range []string{"not-logged-in-churned", "not-logged-in-brewed"} {
		text := readCorpusFile(t, "test", "corpus", "auth", "claude-code", name, "screen.txt")
		got, ok := a.PermissionMode(screen.Snapshot{Text: text})
		if !ok {
			t.Errorf("[%s] PermissionMode = _, false; want %q, true", name, "manual")
			continue
		}
		if got != "manual" {
			t.Errorf("[%s] PermissionMode = %q; want %q", name, got, "manual")
		}
	}
}

// The footer strings that already appear incidentally in this package's other
// tests must keep parsing — each is a free regression assertion.
func TestPermissionModeFromFooter_incidentalFixtures(t *testing.T) {
	for _, tc := range []struct {
		text string
		want string
	}{
		{"⏵⏵ bypass permissions on · esc to interrupt\n✢ Schlepping… (3s · ↓2 tokens)", "bypass"},
		{"✻ Baked for 3s\n❯ \n⏵⏵ auto mode on · ← for agents", "auto"},
		{"❯ \n⏵⏵ bypass permissions on (shift+tab to cycle) · ↓ to manage", "bypass"},
		{oneShotScreen, "bypass"},
	} {
		got, ok := permissionModeFromFooter(tc.text)
		if !ok {
			t.Errorf("permissionModeFromFooter(%q) = _, false; want %q", tc.text, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("permissionModeFromFooter(%q) = %q; want %q", tc.text, got, tc.want)
		}
	}
}

// padTo right-pads s with spaces to width runes, the way pkg/screen pads every
// rendered row to the terminal width.
func padTo(s string, width int) string {
	if n := len([]rune(s)); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func readCorpusFile(t *testing.T, parts ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", "..", "..", ".."}, parts...)...))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// TestPermissionModeFromFooter_dontAsk pins the sixth footer word added for
// claude's native --permission-mode dontAsk. It reports the EXISTING manual
// rung: claude's permissiveness rank table
// ({plan:0, bubble:1, default:1, dontAsk:1, acceptEdits:2, auto:3,
// bypassPermissions:4}) ties dontAsk with default (claude's spelling of
// manual), and the canonical rung ladder is a strict total order with no way to
// express a tie.
//
// The recorded capture (test/corpus/permission-mode/claude-code/dont-ask) uses
// an ASCII apostrophe; the U+2019 case below is SYNTHETIC and exists so a
// future release that typesets the footer cannot silently drop the read back to
// unknown.
func TestPermissionModeFromFooter_dontAsk(t *testing.T) {
	for _, text := range []string{
		"⏵⏵ don't ask on (shift+tab to cycle) · ← for agents",
		"⏵⏵ don't ask on",
		"⏵⏵ don’t ask on (shift+tab to cycle) · ← for agents",
		"⏸ don't ask on",
		padTo("  ⏵⏵ don't ask on (shift+tab to cycle) · ← for agents          ○ low · /effort", 120),
	} {
		got, ok := permissionModeFromFooter(text)
		if !ok {
			t.Errorf("permissionModeFromFooter(%q) = _, false; want \"manual\", true", text)
			continue
		}
		if got != "manual" {
			t.Errorf("permissionModeFromFooter(%q) = %q; want \"manual\"", text, got)
		}
	}
}

// The per-tool permission dialog carries the literal words "don't ask again"
// on a menu row. Adding the sixth alternation row must not turn that dialog
// into a footer reading.
func TestPermissionModeFromFooter_perToolDialogIsNotAFooter(t *testing.T) {
	if got, ok := permissionModeFromFooter(toolPermissionScreen); ok {
		t.Errorf("permissionModeFromFooter(toolPermissionScreen) = %q, true; want \"\", false", got)
	}
}

var _ turns.PermissionPostureDetector = (*Adapter)(nil)

// TestPermissionPostureFromFooter_matrix pins the FULL posture for every one of
// the six footer words: the canonical rung, claude's own spelling of it, and
// whether the Shift+Tab cycle can produce that spelling.
//
// The two rows that carry the whole distinction, and the reason this reader
// exists at all:
//
//   - "don't ask"    → {manual, dontAsk, OFF-ring}. Truthfully the manual rung,
//     but a launch-only posture; a driver asked for "manual" must cycle.
//   - "accept edits" → {ask, acceptEdits, ON-ring}. Also a second spelling of
//     its rung, but one the cycle produces — a driver asked for "ask" must NOT
//     press anything. It is the guard against over-correcting this fix.
func TestPermissionPostureFromFooter_matrix(t *testing.T) {
	for _, tc := range []struct {
		marker string
		want   turns.PermissionPosture
	}{
		{"⏸ plan mode on", turns.PermissionPosture{Rung: "plan", Native: "plan", OnRing: true}},
		{"⏸ manual mode on", turns.PermissionPosture{Rung: "manual", Native: "default", OnRing: true}},
		{"⏵⏵ accept edits on", turns.PermissionPosture{Rung: "ask", Native: "acceptEdits", OnRing: true}},
		{"⏵⏵ auto mode on", turns.PermissionPosture{Rung: "auto", Native: "auto", OnRing: true}},
		{"⏵⏵ bypass permissions on", turns.PermissionPosture{Rung: "bypass", Native: "bypassPermissions", OnRing: true}},
		{"⏵⏵ don't ask on", turns.PermissionPosture{Rung: "manual", Native: "dontAsk", OnRing: false}},
	} {
		for _, tail := range footerTails {
			text := padTo("  "+tc.marker+tail, 120)
			got, ok := permissionPostureFromFooter(text)
			if !ok {
				t.Errorf("permissionPostureFromFooter(%q) = _, false; want %+v, true", text, tc.want)
				continue
			}
			if got != tc.want {
				t.Errorf("permissionPostureFromFooter(%q) = %+v; want %+v", text, got, tc.want)
			}
		}
	}
}

// The curly-apostrophe variant of the don't-ask footer must yield the IDENTICAL
// posture, not merely the identical rung: one normalisation feeds both lookups.
func TestPermissionPostureFromFooter_curlyApostrophe(t *testing.T) {
	want := turns.PermissionPosture{Rung: "manual", Native: "dontAsk", OnRing: false}
	for _, text := range []string{
		"⏵⏵ don't ask on (shift+tab to cycle) · ← for agents",
		"⏵⏵ don’t ask on (shift+tab to cycle) · ← for agents",
		"⏵⏵   don’t    ask   on",
	} {
		got, ok := permissionPostureFromFooter(text)
		if !ok {
			t.Errorf("permissionPostureFromFooter(%q) = _, false; want %+v, true", text, want)
			continue
		}
		if got != want {
			t.Errorf("permissionPostureFromFooter(%q) = %+v; want %+v", text, got, want)
		}
	}
}

// A renamed or newly-added mode word degrades to the ZERO posture, never to a
// half-read one: an unknown word must not yield Rung "" with OnRing true, which
// a driver's on-ring test would then have to defend against.
func TestPermissionPostureFromFooter_negatives(t *testing.T) {
	for _, text := range []string{
		"",
		"❯ \n✻ Baked for 3s\n",
		"⏵⏵ frobnicate mode on (shift+tab to cycle) · ← for agents",
		"⏵⏵ don't ask again on (shift+tab to cycle)",
		"  2. Yes, and don't ask again for npm commands",
		"auto mode on (shift+tab to cycle) · ← for agents",
	} {
		got, ok := permissionPostureFromFooter(text)
		if ok {
			t.Errorf("permissionPostureFromFooter(%q) = %+v, true; want zero, false", text, got)
			continue
		}
		if got != (turns.PermissionPosture{}) {
			t.Errorf("permissionPostureFromFooter(%q) returned %+v with ok=false; want the zero posture", text, got)
		}
	}
}

// The two readers can no longer disagree — PermissionMode is a projection of
// PermissionPosture — but the assertion is what keeps a future refactor honest.
func TestAdapter_PermissionModeAgreesWithPosture(t *testing.T) {
	a := New()
	texts := []string{"", "❯ \n✻ Baked for 3s\n", "⏵⏵ turbo mode on"}
	for _, m := range footerMarkers {
		for _, tail := range footerTails {
			texts = append(texts, padTo("  "+m.marker+tail, 120))
		}
	}
	for _, text := range texts {
		snap := screen.Snapshot{Text: text}
		rung, rok := a.PermissionMode(snap)
		p, pok := a.PermissionPosture(snap)
		if rok != pok {
			t.Errorf("[%q] PermissionMode ok=%v but PermissionPosture ok=%v", text, rok, pok)
			continue
		}
		if p.Rung != rung {
			t.Errorf("[%q] PermissionPosture.Rung = %q; PermissionMode = %q", text, p.Rung, rung)
		}
	}
}

// permissionModeNatives and cycleRingNatives are keyed off the same closed
// alternation as permissionModeRungs. Drift between them would make
// permissionPostureFromFooter degrade a KNOWN mode to unknown, so pin it.
func TestPermissionModeTablesAreKeyedTogether(t *testing.T) {
	for key := range permissionModeRungs {
		native, ok := permissionModeNatives[key]
		if !ok {
			t.Errorf("permissionModeNatives is missing the %q row that permissionModeRungs has", key)
			continue
		}
		if _, ok := cycleRingNatives[native]; !ok {
			t.Errorf("cycleRingNatives is missing the %q row for footer word %q", native, key)
		}
	}
	for key := range permissionModeNatives {
		if _, ok := permissionModeRungs[key]; !ok {
			t.Errorf("permissionModeRungs is missing the %q row that permissionModeNatives has", key)
		}
	}
	// dontAsk is the ONE off-ring spelling, and the reason the capability
	// exists. If claude ever puts it on the cycle, this fix is no longer needed
	// and the change must be deliberate.
	if cycleRingNatives["dontAsk"] {
		t.Error(`cycleRingNatives["dontAsk"] = true; dontAsk is a launch-only posture absent from claude's ring function`)
	}
}
