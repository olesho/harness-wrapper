package screen

import (
	"strings"
	"testing"
)

func lines(s *Screen) []string {
	return strings.Split(s.Snapshot().Text, "\n")
}

// paintThenQuery replays the order claude-code 2.1.270 uses at startup: save
// the cursor at the top (DECSC), paint the screen, send a terminal query, then
// repaint one line with a RELATIVE cursor move. If the query moved the cursor,
// the repaint lands in the wrong row.
func paintThenQuery(t *testing.T, query ...string) *Screen {
	t.Helper()
	s := New(40, 10)
	write := func(p string) {
		if _, err := s.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	write("\x1b7\r\n  row one\r\n  row two\r\n❯ old choice\r\n  footer\r\n")
	for _, q := range query {
		write(q)
	}
	write("\x1b[2A\r❯ new choice") // up two rows from below the footer
	return s
}

// TestPrivateMarkerCSIDoesNotMoveTheCursor: vt10x only knows the "?" private
// marker and executes every final "u" as restore-cursor, so the kitty keyboard
// protocol's query (CSI ? u), push (CSI > flags u), pop (CSI < n u) and set
// (CSI = flags ; mode u) all teleported the cursor to the position saved at
// startup. claude 2.1.270 sends CSI ? u right after painting its folder-trust
// dialog; every incremental repaint after it then landed on the top rows and
// the rendered screen never showed the highlight move.
func TestPrivateMarkerCSIDoesNotMoveTheCursor(t *testing.T) {
	for name, query := range map[string][]string{
		"kitty query":          {"\x1b[?u"},
		"kitty push":           {"\x1b[>1u"},
		"kitty pop":            {"\x1b[<u"},
		"kitty set":            {"\x1b[=1;1u"},
		"xtversion + da query": {"\x1b[>0q", "\x1b[c"},
		"modifyOtherKeys":      {"\x1b[>4;2m"},
		"split across writes":  {"\x1b[", "?", "u"},
	} {
		t.Run(name, func(t *testing.T) {
			got := lines(paintThenQuery(t, query...))
			if strings.TrimSpace(got[3]) != "❯ new choice" {
				t.Errorf("row 3 = %q, want the repainted choice\nscreen:\n%s", got[3], strings.Join(got, "\n"))
			}
			if strings.Contains(got[0], "choice") {
				t.Errorf("the repaint landed on row 0: %q", got[0])
			}
		})
	}
}

// TestPlainCSIUStillRestoresTheCursor is the control: the unmarked ANSI.SYS
// restore (CSI u) is a real cursor command and must keep working, as must a
// "?"-marked DEC mode like cursor visibility.
func TestPlainCSIUStillRestoresTheCursor(t *testing.T) {
	s := New(40, 10)
	if _, err := s.Write([]byte("\r\n\r\n  here\x1b[s\r\n\r\n\x1b[?25l\x1b[uX")); err != nil {
		t.Fatal(err)
	}
	got := lines(s)
	if !strings.HasPrefix(got[2], "  hereX") {
		t.Errorf("row 2 = %q, want the X written at the restored position", got[2])
	}
}
