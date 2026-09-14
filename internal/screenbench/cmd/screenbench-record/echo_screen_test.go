//go:build screenbench

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// cursorEchoStdin echoes a typed body back the way Claude sometimes paints its
// composer: each space is a cursor-column jump ("what\x1b[8Gis…") rather than
// a space byte, so the text is contiguous on the rendered screen but not in
// the raw stream. It records when the submit key arrives.
type cursorEchoStdin struct {
	d      *scriptDriver
	mu     sync.Mutex
	sent   strings.Builder
	bodyAt time.Time
	subAt  time.Time
}

func (c *cursorEchoStdin) WriteStdin(p []byte) (int, error) {
	c.mu.Lock()
	c.sent.Write(p)
	now := time.Now()
	isSubmit := string(p) == "\x1b[13u"
	if isSubmit {
		c.subAt = now
	} else {
		c.bodyAt = now
	}
	c.mu.Unlock()
	if !isSubmit {
		col := 3 // after "❯ "
		var b strings.Builder
		b.WriteString("\x1b[2J\x1b[5;1H❯ ")
		for i, w := range strings.Split(string(p), " ") {
			if i > 0 {
				col++ // the space is a jump, not a byte
				fmt.Fprintf(&b, "\x1b[%dG", col)
			}
			b.WriteString(w)
			col += len([]rune(w))
		}
		_, _ = c.d.Write([]byte(b.String()))
	}
	return len(p), nil
}

// TestSend_EchoPaintedWithCursorMovesIsSeen: production confirms the composer
// echo on the RENDERED screen; the recorder searched the raw bytes, so an echo
// painted with cursor moves was never found and every such send waited out the
// whole echo bound before submitting. The submit key must follow the echo
// promptly.
func TestSend_EchoPaintedWithCursorMovesIsSeen(t *testing.T) {
	d := newScriptDriver(nil, time.Second, 0)
	stdin := &cursorEchoStdin{d: d}
	d.stdin = stdin
	d.submitKey = []byte("\x1b[13u")
	d.echoGap = 1500 * time.Millisecond

	if err := d.send(context.Background(), "what is 2 plus 2?\n"); err != nil {
		t.Fatalf("send: %v", err)
	}
	stdin.mu.Lock()
	defer stdin.mu.Unlock()
	if got, want := stdin.sent.String(), "what is 2 plus 2?\x1b[13u"; got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if gap := stdin.subAt.Sub(stdin.bodyAt); gap > 500*time.Millisecond {
		t.Errorf("submit key followed the echoed body after %v; the echo was on screen at once, so it should not have waited out the %v bound", gap, d.echoGap)
	}
}
