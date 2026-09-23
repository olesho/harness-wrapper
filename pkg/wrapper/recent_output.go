package wrapper

import (
	"bytes"
	"sync"
)

// recentOutputBuffer keeps the last limit bytes of harness output and counts
// every byte ever written, so a reader can ask for "what arrived after the
// last thing I looked at" rather than rescanning the whole window.
type recentOutputBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
	total int64 // bytes written since the session started
}

func newRecentOutput(limit int) *recentOutputBuffer {
	return &recentOutputBuffer{limit: limit}
}

func (b *recentOutputBuffer) Write(p []byte) {
	if b == nil || b.limit <= 0 || len(p) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.total += int64(len(p))
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return
	}
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.limit; over > 0 {
		copy(b.buf, b.buf[over:])
		b.buf = b.buf[:b.limit]
	}
}

func (b *recentOutputBuffer) String() string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// Total reports how many bytes have been written in all.
func (b *recentOutputBuffer) Total() int64 {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// TextFrom returns the retained output from byte offset mark (counted from the
// first byte ever written) to the end, widened back to the start of the line
// mark falls in, so a line split across writes is read whole. through is the
// offset the returned text ends at: Total at the moment of the read. A mark
// older than the retained window yields the whole window.
func (b *recentOutputBuffer) TextFrom(mark int64) (text string, through int64) {
	if b == nil {
		return "", 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	i := 0
	if start := b.total - int64(len(b.buf)); mark > start {
		i = min(int(mark-start), len(b.buf))
	}
	i = bytes.LastIndexByte(b.buf[:i], '\n') + 1
	return string(b.buf[i:]), b.total
}
