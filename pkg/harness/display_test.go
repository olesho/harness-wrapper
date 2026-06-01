package harness

import (
	"strconv"
	"sync"
	"testing"
)

func TestDisplaySinkDeliversInOrder(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	d := newDisplaySink(func(line string) {
		mu.Lock()
		got = append(got, line)
		mu.Unlock()
	})
	for _, l := range []string{"a", "b", "c"} {
		d.push(l)
	}
	if dropped := d.close(); dropped != 0 {
		t.Fatalf("dropped %d with a fast consumer, want 0", dropped)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("got %v, want [a b c] in order", got)
	}
}

func TestDisplaySinkDropsOldestUnderBackpressure(t *testing.T) {
	// A blocked consumer + far more pushes than the queue holds ⇒ the sink drops
	// (counted) and never blocks the pusher. We hold the consumer until all
	// pushes are done, then release; the survivors are the NEWEST lines.
	release := make(chan struct{})
	var (
		mu   sync.Mutex
		seen []string
	)
	d := newDisplaySink(func(line string) {
		<-release // block the single consumer until released
		mu.Lock()
		seen = append(seen, line)
		mu.Unlock()
	})

	const n = displaySinkCap * 4
	for i := 0; i < n; i++ {
		d.push(strconv.Itoa(i)) // must never block even though the consumer is stuck
	}
	close(release)
	dropped := d.close()

	if dropped == 0 {
		t.Fatal("expected drops under back-pressure, got 0")
	}
	mu.Lock()
	delivered := len(seen)
	mu.Unlock()
	if uint64(delivered)+dropped < uint64(n) {
		t.Errorf("delivered(%d) + dropped(%d) = %d, want >= %d", delivered, dropped, uint64(delivered)+dropped, n)
	}
	// The very last line pushed should survive (drop-OLDEST keeps the newest).
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	if last != strconv.Itoa(n-1) {
		t.Errorf("last delivered = %q, want newest %q (drop-oldest)", last, strconv.Itoa(n-1))
	}
}

func TestDisplaySinkNilSafe(t *testing.T) {
	d := newDisplaySink(nil)
	if d != nil {
		t.Fatal("newDisplaySink(nil) should be nil")
	}
	d.push("x") // must not panic
	if d.close() != 0 {
		t.Fatal("nil close should return 0 and not panic")
	}
}

func TestDisplaySinkRecoversPanic(t *testing.T) {
	// A panicking callback is recovered+dropped, not fatal; later lines still flow.
	var (
		mu  sync.Mutex
		got int
	)
	first := true
	d := newDisplaySink(func(string) {
		mu.Lock()
		defer mu.Unlock()
		if first {
			first = false
			panic("boom")
		}
		got++
	})
	d.push("one") // panics inside the consumer
	d.push("two") // should still be delivered
	d.close()
	mu.Lock()
	defer mu.Unlock()
	if got != 1 {
		t.Fatalf("delivered %d after a panic, want 1 (panic recovered, drain continues)", got)
	}
}
