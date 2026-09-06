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
		b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "test", "corpus", "claude-code", name, "bytes.raw"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sc := screen.New(120, 40)
		_, _ = sc.Write(b)
		got, ok := a.PermissionMode(sc.Snapshot())
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
