package claudecodetui

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strconv"
)

// claude's debug log (--debug-file) is not a documented interface: the
// profile pins claude, reads only the lines below, and keeps one of each, as
// claude 2.1.283 writes them, in testdata/debug-2.1.283.log, which the unit
// tests parse (probes/tui-hybrid/FINDINGS.md has what each means).
//
//	[engine] turn N start
//	[engine] turn N end (… stop=<reason> resultLen=<n>)
//	[ERROR] API error (attempt k/N): <status> <body>
//	[onCancel] source=local streamMode=<mode>
//
// Any other line with "[engine] " says only that claude's engine logs, which
// Start waits for. A turn's start and end bracket it; a turn end's stop
// reason says how it ended (null or tool_use: an interrupt stopped it); an
// API error is one failed model request, which claude retries while k < N;
// [onCancel] is claude taking an interrupt (Esc).

// lineKind is what a debug line says.
type lineKind int

const (
	lineOther lineKind = iota
	// lineEngine: some other line of claude's engine.
	lineEngine
	lineTurnStart
	lineTurnEnd
	lineAPIError
	lineCancel
)

// debugLine is one debug line, read.
type debugLine struct {
	kind lineKind
	turn int
	// stop is a turn end's stop reason: end_turn, stop_sequence, null, ….
	stop string
	// attempt, max and status are an API error's.
	attempt, max, status int
}

var (
	reTurnStart = regexp.MustCompile(`\[engine\] turn (\d+) start\s*$`)
	reTurnEnd   = regexp.MustCompile(`\[engine\] turn (\d+) end \(.*\bstop=(\w+)`)
	reAPIError  = regexp.MustCompile(`API error \(attempt (\d+)/(\d+)\): (\d{3})\b`)
	reCancel    = regexp.MustCompile(`\[onCancel\]`)
	reEngine    = regexp.MustCompile(`\[engine\] `)
)

func parseDebugLine(s string) debugLine {
	atoi := func(v string) int { n, _ := strconv.Atoi(v); return n }
	switch {
	case reTurnStart.MatchString(s):
		m := reTurnStart.FindStringSubmatch(s)
		return debugLine{kind: lineTurnStart, turn: atoi(m[1])}
	case reTurnEnd.MatchString(s):
		m := reTurnEnd.FindStringSubmatch(s)
		return debugLine{kind: lineTurnEnd, turn: atoi(m[1]), stop: m[2]}
	case reAPIError.MatchString(s):
		m := reAPIError.FindStringSubmatch(s)
		return debugLine{kind: lineAPIError, attempt: atoi(m[1]), max: atoi(m[2]), status: atoi(m[3])}
	case reCancel.MatchString(s):
		return debugLine{kind: lineCancel}
	case reEngine.MatchString(s):
		return debugLine{kind: lineEngine}
	}
	return debugLine{}
}

// maxDebugLine bounds one debug line; the rest of a longer one is dropped.
const maxDebugLine = 64 << 10

// tail follows claude's debug log, which claude appends to (reopening the
// path for each write): each call returns the whole lines written since.
type tail struct {
	path string
	off  int64
	part []byte
}

func (t *tail) lines() []string {
	f, err := os.Open(t.path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err == nil && fi.Size() < t.off {
		t.off, t.part = 0, nil // truncated: start over
	}
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil || len(b) == 0 {
		return nil
	}
	t.off += int64(len(b))
	b = append(t.part, b...)
	cut := bytes.LastIndexByte(b, '\n')
	if cut < 0 {
		t.part = capLine(b)
		return nil
	}
	t.part = capLine(append([]byte(nil), b[cut+1:]...))
	var out []string
	for _, l := range bytes.Split(b[:cut], []byte{'\n'}) {
		out = append(out, string(capLine(l)))
	}
	return out
}

func capLine(b []byte) []byte {
	if len(b) > maxDebugLine {
		return b[:maxDebugLine]
	}
	return b
}
