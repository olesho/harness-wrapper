package claudecodetui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The debug lines the profile reads, as the pinned claude writes them
// (testdata/debug-2.1.283.log: a turn that completed, one whose retries ran
// out, and one at a usage wall), parse as the profile expects. When claude's
// pin moves, this fixture is captured again.
func TestDebugFixture(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "debug-2.1.283.log"))
	if err != nil {
		t.Fatal(err)
	}
	var got []debugLine
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if d := parseDebugLine(l); d.kind != lineOther && d.kind != lineEngine {
			got = append(got, d)
		}
	}
	want := []debugLine{
		{kind: lineTurnStart, turn: 1},
		{kind: lineTurnEnd, turn: 1, stop: "end_turn"},
		{kind: lineTurnStart, turn: 2},
		{kind: lineAPIError, attempt: 1, max: 3, status: 529},
		{kind: lineAPIError, attempt: 1, max: 3, status: 529},
		{kind: lineAPIError, attempt: 2, max: 3, status: 529},
		{kind: lineAPIError, attempt: 3, max: 3, status: 529},
		{kind: lineTurnEnd, turn: 2, stop: "stop_sequence"},
		{kind: lineTurnStart, turn: 3},
		{kind: lineAPIError, attempt: 1, max: 3, status: 429},
		{kind: lineTurnEnd, turn: 3, stop: "stop_sequence"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed\n%+v\nwant\n%+v", got, want)
	}
	for _, l := range []string{
		"2026-10-07T15:28:34.001Z [DEBUG] [engine] send set_model model=claude-sonnet-5",
		"2026-10-07T15:28:50.793Z [DEBUG] [engine] turn read waited 6ms for the previous turn's result",
		"2026-10-07T15:29:03.002Z [ERROR] [engine] turn ended in error: You've hit your session limit",
	} {
		if k := parseDebugLine(l).kind; k != lineEngine {
			t.Errorf("%q read as %d, want an engine line and no more", l, k)
		}
	}
	if k := parseDebugLine("2026-10-07T15:34:16.137Z [DEBUG] [init] configureGlobalMTLS starting").kind; k != lineOther {
		t.Errorf("an init line read as %d", k)
	}
}

// The tail returns whole lines, as claude appends them, and starts over when
// the file is truncated.
func TestTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "debug.log")
	tl := tail{path: p}
	if got := tl.lines(); got != nil {
		t.Fatalf("no file: %q", got)
	}
	appendTo := func(s string) {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(s)
		_ = f.Close()
	}
	appendTo("one\ntw")
	if got := tl.lines(); !reflect.DeepEqual(got, []string{"one"}) {
		t.Errorf("lines = %q", got)
	}
	appendTo("o\nthree\n")
	if got := tl.lines(); !reflect.DeepEqual(got, []string{"two", "three"}) {
		t.Errorf("lines = %q", got)
	}
	if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := tl.lines(); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("after truncation: %q", got)
	}
}
