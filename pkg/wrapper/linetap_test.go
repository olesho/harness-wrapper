package wrapper

import (
	"strconv"
	"strings"
	"testing"
)

// collect runs the splitter over the given write chunks (optionally flushing)
// and returns the lines delivered, in order.
func collect(t *testing.T, flush bool, chunks ...string) []string {
	t.Helper()
	var got []string
	ls := newLineSplitter(func(line string) { got = append(got, line) })
	for _, c := range chunks {
		ls.write([]byte(c))
	}
	if flush {
		ls.flush()
	}
	return got
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("line count: got %d %q, want %d %q", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLineSplitterChunkBoundaries(t *testing.T) {
	// Lines split across arbitrary read-chunk boundaries reassemble in order.
	eq(t, collect(t, false, "he", "llo\nwor", "ld\nthird\n"),
		[]string{"hello", "world", "third"})
}

func TestLineSplitterCRLF(t *testing.T) {
	// A trailing '\r' from '\r\n' framing is trimmed; lone '\n' unaffected.
	eq(t, collect(t, false, "a\r\nb\r\nc\n"), []string{"a", "b", "c"})
}

func TestLineSplitterEmptyLinesPreserved(t *testing.T) {
	// Blank lines are real lines (a stream may emit them); they are not coalesced.
	eq(t, collect(t, false, "a\n\nb\n"), []string{"a", "", "b"})
}

func TestLineSplitterFlushesFinalUnterminatedLine(t *testing.T) {
	// Without flush, a line lacking a trailing newline is held, not delivered.
	eq(t, collect(t, false, "done\ntail"), []string{"done"})
	// flush (called once at PTY EOF) delivers it.
	eq(t, collect(t, true, "done\ntail"), []string{"done", "tail"})
	// A trailing '\r' on the final unterminated line is trimmed too.
	eq(t, collect(t, true, "x\ny\r"), []string{"x", "y"})
}

func TestLineSplitterNoTrailingFlushWhenClean(t *testing.T) {
	// If the buffer ended exactly on a newline, flush emits nothing extra.
	eq(t, collect(t, true, "a\nb\n"), []string{"a", "b"})
}

func TestLineSplitterMultiMBLine(t *testing.T) {
	// A single multi-megabyte line (no cap) is reassembled whole across many
	// small chunks. This is the "no line-length cap" guarantee for stream-json
	// records, which can be large.
	const total = 5 << 20 // 5 MiB
	const chunk = 7919    // a prime, so chunk boundaries fall mid-line repeatedly
	var got []string
	ls := newLineSplitter(func(line string) { got = append(got, line) })
	written := 0
	for written < total {
		n := chunk
		if written+n > total {
			n = total - written
		}
		ls.write([]byte(strings.Repeat("x", n)))
		written += n
	}
	if len(got) != 0 {
		t.Fatalf("no newline yet, but %d lines delivered", len(got))
	}
	ls.write([]byte("\n"))
	if len(got) != 1 {
		t.Fatalf("after newline: got %d lines, want 1", len(got))
	}
	if len(got[0]) != total {
		t.Fatalf("multi-MB line length: got %d, want %d", len(got[0]), total)
	}
}

func TestLineSplitterOrderedUnderSlowConsumer(t *testing.T) {
	// Delivery is synchronous, so a slow consumer back-pressures rather than
	// drops: every line arrives, in order. (We emulate "slow" by doing work in
	// the callback; correctness is that nothing is lost or reordered.)
	const n = 1000
	var got []string
	prev := -1
	ls := newLineSplitter(func(line string) {
		// Verify strict monotonic order as lines arrive.
		v, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("unexpected line %q: %v", line, err)
		}
		if v != prev+1 {
			t.Fatalf("out of order / dropped: got %d after %d", v, prev)
		}
		prev = v
		got = append(got, line)
	})
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte('\n')
	}
	// Feed the whole blob in two halves to exercise a mid-line boundary.
	blob := sb.String()
	ls.write([]byte(blob[:len(blob)/2]))
	ls.write([]byte(blob[len(blob)/2:]))
	if len(got) != n {
		t.Fatalf("got %d lines, want %d (non-lossy)", len(got), n)
	}
}

func TestLineSplitterNilIsNoOp(t *testing.T) {
	// No OnLine configured ⇒ nil splitter; the read loop calls these methods
	// unconditionally, so they must not panic.
	ls := newLineSplitter(nil)
	if ls != nil {
		t.Fatalf("newLineSplitter(nil) = %v, want nil", ls)
	}
	ls.write([]byte("anything\n")) // must not panic
	ls.flush()                     // must not panic
}
