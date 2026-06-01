package wrapper

import "bytes"

// cr is the carriage return trimmed from the end of a '\r\n'-framed line so the
// durable tap delivers the same logical line whether the harness emits '\n' or
// '\r\n'.
var cr = []byte{'\r'}

// lineSplitter is the durable line tap's accumulator. It buffers raw PTY bytes
// across read-chunk boundaries and invokes onLine once per complete
// '\n'-terminated line (with any trailing '\r' trimmed). It preserves
// arbitrarily long lines (no cap) and, on flush, emits a final unterminated
// remainder.
//
// onLine runs synchronously in the PTY read goroutine, so delivery is ordered
// and non-lossy: a slow callback back-pressures the read loop rather than
// dropping a line. A nil *lineSplitter (no Config.OnLine configured) is an inert
// no-op, so the read loop need not branch.
type lineSplitter struct {
	onLine func(string)
	buf    []byte
}

// newLineSplitter returns a splitter that forwards completed lines to onLine, or
// nil when onLine is nil (the tap is disabled — the read loop calls into the
// nil-safe methods unconditionally).
func newLineSplitter(onLine func(string)) *lineSplitter {
	if onLine == nil {
		return nil
	}
	return &lineSplitter{onLine: onLine}
}

// write feeds a chunk of raw PTY bytes, emitting every line the chunk completes.
// The chunk may split a line anywhere (including mid-multibyte / mid-escape):
// the remainder is held until a later write completes it. A nil splitter is a
// no-op.
func (ls *lineSplitter) write(p []byte) {
	if ls == nil {
		return
	}
	ls.buf = append(ls.buf, p...)
	consumed := false
	for {
		i := bytes.IndexByte(ls.buf, '\n')
		if i < 0 {
			break
		}
		ls.onLine(string(bytes.TrimSuffix(ls.buf[:i], cr)))
		ls.buf = ls.buf[i+1:]
		consumed = true
	}
	// Reclaim the consumed prefix so the backing array doesn't grow unbounded
	// across a long run of small lines. Skip the copy when nothing was consumed
	// (a partial multi-MB line still accumulating) to keep that case amortized
	// O(n) rather than O(n^2).
	switch {
	case !consumed:
	case len(ls.buf) == 0:
		ls.buf = nil
	default:
		ls.buf = append([]byte(nil), ls.buf...)
	}
}

// flush emits any final unterminated line. It is called once, after the PTY
// closes, so a harness that exits without a trailing newline (or whose last
// stream-json record lacks one) still delivers its last line. A nil splitter or
// empty buffer is a no-op.
func (ls *lineSplitter) flush() {
	if ls == nil || len(ls.buf) == 0 {
		return
	}
	ls.onLine(string(bytes.TrimSuffix(ls.buf, cr)))
	ls.buf = nil
}
