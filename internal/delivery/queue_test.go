package delivery

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// gate is a callback that records values and blocks until released.
type gate struct {
	mu   sync.Mutex
	got  []int
	open chan struct{}
}

func newGate() *gate { return &gate{open: make(chan struct{})} }

func (g *gate) deliver(v int) {
	<-g.open
	g.mu.Lock()
	g.got = append(g.got, v)
	g.mu.Unlock()
}

func (g *gate) values() []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]int(nil), g.got...)
}

func one(int) int64 { return 1 }

func TestPushDeliversInOrder(t *testing.T) {
	g := newGate()
	close(g.open)
	q := New(Limits{}, one, g.deliver)
	for i := range 500 {
		if err := q.Push(i, nil); err != nil {
			t.Fatal(err)
		}
	}
	q.Close()
	<-q.Drained()
	got := g.values()
	for i, v := range got {
		if v != i {
			t.Fatalf("delivery %d = %d, out of order", i, v)
		}
	}
	if len(got) != 500 {
		t.Fatalf("delivered %d of 500", len(got))
	}
}

// At the event bound a producer waits — it neither drops nor grows the queue
// — and goes through once the consumer takes one.
func TestPushWaitsAtTheEventBound(t *testing.T) {
	g := newGate()
	q := New(Limits{Events: 2}, one, g.deliver)
	for i := range 3 { // one in the callback, two queued
		if err := q.Push(i, nil); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { return q.Stats().DeliveringSince != (time.Time{}) })
	pushed := make(chan error, 1)
	go func() { pushed <- q.Push(3, nil) }()
	waitFor(t, func() bool { return !q.Stats().WaitingSince.IsZero() })
	if st := q.Stats(); st.Queued != 2 {
		t.Fatalf("queued %d, want the bound, 2", st.Queued)
	}
	close(g.open)
	if err := <-pushed; err != nil {
		t.Fatal(err)
	}
	q.Close()
	<-q.Drained()
	if got := g.values(); len(got) != 4 || got[3] != 3 {
		t.Fatalf("delivered %v", got)
	}
}

func TestPushWaitsAtTheByteBound(t *testing.T) {
	g := newGate()
	size := func(v int) int64 { return int64(v) }
	q := New(Limits{Events: 100, Bytes: 10}, size, g.deliver)
	_ = q.Push(1, nil) // taken by the callback
	waitFor(t, func() bool { return q.Stats().DeliveringSince != (time.Time{}) })
	_ = q.Push(6, nil)
	pushed := make(chan error, 1)
	go func() { pushed <- q.Push(5, nil) }() // 6+5 > 10
	waitFor(t, func() bool { return !q.Stats().WaitingSince.IsZero() })
	if st := q.Stats(); st.QueuedBytes != 6 {
		t.Fatalf("queued %d bytes, want 6", st.QueuedBytes)
	}
	close(g.open)
	if err := <-pushed; err != nil {
		t.Fatal(err)
	}
}

func TestPushRefusesAValueOverTheByteBound(t *testing.T) {
	q := New(Limits{Bytes: 10}, func(v int) int64 { return int64(v) }, func(int) {})
	if err := q.Push(11, nil); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Push = %v, want ErrTooLarge", err)
	}
	if err := q.Push(10, nil); err != nil {
		t.Fatalf("Push at the bound = %v", err)
	}
}

func TestPushGivesUpOnAbort(t *testing.T) {
	g := newGate()
	q := New(Limits{Events: 1}, one, g.deliver)
	_ = q.Push(0, nil)
	waitFor(t, func() bool { return q.Stats().DeliveringSince != (time.Time{}) })
	_ = q.Push(1, nil)
	abort := make(chan struct{})
	pushed := make(chan error, 1)
	go func() { pushed <- q.Push(2, abort) }()
	waitFor(t, func() bool { return !q.Stats().WaitingSince.IsZero() })
	close(abort)
	if err := <-pushed; !errors.Is(err, ErrAbandoned) {
		t.Fatalf("Push = %v, want ErrAbandoned", err)
	}
	if st := q.Stats(); st.Undelivered != 1 {
		t.Fatalf("undelivered = %d, want 1", st.Undelivered)
	}
}

// PushLast never waits, however stalled the consumer, and the producers still
// waiting are turned away rather than delivered after it.
func TestPushLastNeverWaits(t *testing.T) {
	g := newGate()
	q := New(Limits{Events: 1}, one, g.deliver)
	_ = q.Push(0, nil)
	waitFor(t, func() bool { return q.Stats().DeliveringSince != (time.Time{}) })
	_ = q.Push(1, nil)
	waiting := make(chan error, 1)
	go func() { waiting <- q.Push(2, nil) }()
	waitFor(t, func() bool { return !q.Stats().WaitingSince.IsZero() })

	done := make(chan struct{})
	go func() { q.PushLast(99); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PushLast waited for the stalled consumer")
	}
	if err := <-waiting; !errors.Is(err, ErrAbandoned) {
		t.Fatalf("the waiting Push = %v, want ErrAbandoned", err)
	}
	close(g.open)
	<-q.Drained()
	if got := g.values(); len(got) != 3 || got[2] != 99 {
		t.Fatalf("delivered %v, want 0, 1, then the last value", got)
	}
	if err := q.Push(3, nil); !errors.Is(err, ErrAbandoned) {
		t.Fatalf("Push after PushLast = %v, want ErrAbandoned", err)
	}
}

func TestAbandonDropsWhatIsQueued(t *testing.T) {
	g := newGate()
	q := New(Limits{}, one, g.deliver)
	for i := range 5 {
		_ = q.Push(i, nil)
	}
	waitFor(t, func() bool { return q.Stats().DeliveringSince != (time.Time{}) })
	q.Abandon()
	close(g.open)
	<-q.Drained()
	if got := g.values(); len(got) != 1 {
		t.Fatalf("delivered %v, want only the value already in the callback", got)
	}
	if st := q.Stats(); st.Undelivered != 4 {
		t.Fatalf("undelivered = %d, want 4", st.Undelivered)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}
